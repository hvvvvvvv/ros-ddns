package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ros-ddns/internal/ros"
)

func TestAddressMatchesOnlyHosts(t *testing.T) {
	for _, tc := range []struct {
		entry, desired string
		match          bool
	}{
		{"203.0.113.0", "203.0.113.0", true},
		{"203.0.113.0/24", "203.0.113.0", false},
		{"203.0.113.0/32", "203.0.113.0", true},
		{"2001:0db8:0:0::", "2001:db8::", true},
		{"2001:0db8:0:0::/64", "2001:db8::", false},
		{"2001:0db8:0:0::/128", "2001:db8::", true},
		{"::ffff:203.0.113.0", "203.0.113.0", true},
		{"::ffff:203.0.113.0/120", "203.0.113.0", false},
		{"::ffff:203.0.113.0/128", "203.0.113.0", true},
		{"203.0.113.1/32", "203.0.113.0", false},
		{"invalid", "invalid", false},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			if got := addressMatches(tc.entry, tc.desired); got != tc.match {
				t.Fatalf("addressMatches(%q, %q) = %v", tc.entry, tc.desired, got)
			}
		})
	}
}

func TestAddressListSubnetReplacementAndForeignOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, source, desired, subnet, host string
		family                              ros.Family
	}{
		{"ipv4", "203.0.113.0/24", "203.0.113.0", "203.0.113.0/24", "203.0.113.0/32", ros.IPv4},
		{"ipv6", "2001:db8::/64", "2001:db8::", "2001:0db8:0:0::/64", "2001:0db8:0:0::/128", ros.IPv6},
	} {
		for _, hostRoute := range []bool{false, true} {
			entry := tc.subnet
			if hostRoute {
				entry = tc.host
			}
			t.Run(tc.name+"/"+entry, func(t *testing.T) {
				c := testConfig()
				c.Items[0].Domains = nil
				c.Items[0].Source.Family = string(tc.family)
				router := newFakeROS()
				router.setAddresses(tc.family, address(tc.source))
				foreign := ros.ListEntry{ID: "foreign-subnet", List: "ddns", Address: tc.subnet, Comment: "ros-ddns:home-other"}
				router.entries[tc.family] = []ros.ListEntry{
					{ID: "owned", List: "ddns", Address: entry, Comment: "ros-ddns:home"}, foreign,
				}
				s, _ := testService(t, c, router, &fakeProvider{})
				snapshot, err := s.runOnce(context.Background())
				if err != nil || snapshot.Items[0].Phase != "synced" {
					t.Fatalf("runOnce = %#v, %v", snapshot, err)
				}
				if hostRoute {
					if len(router.operations) != 0 {
						t.Fatalf("valid host route was rewritten: %v", router.operations)
					}
				} else if want := []string{"remove owned", "add " + tc.desired}; !reflect.DeepEqual(router.operations, want) {
					t.Fatalf("subnet replacement = %v, want %v", router.operations, want)
				}
				foundForeign := false
				for _, got := range router.entries[tc.family] {
					if got.ID == foreign.ID {
						foundForeign = got == foreign
					}
				}
				if !foundForeign {
					t.Fatal("foreign subnet was changed")
				}
			})
		}
	}
}

func TestAddressListStatusPreservesSubnetPrefix(t *testing.T) {
	entries := []ros.ListEntry{
		{Address: "203.0.113.0/24", Comment: "owned"},
		{Address: "2001:0db8:0:0::/64", Comment: "owned"},
		{Address: "203.0.113.0/25", Comment: "foreign"},
	}
	want := []string{"2001:db8::/64", "203.0.113.0/24"}
	if got := visibleEntries(entries, "owned", "203.0.113.0"); !reflect.DeepEqual(got, want) {
		t.Fatalf("subnet status = %v, want %v", got, want)
	}
}

func TestAddressListReconcilesEnabledStateAndLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, configured, actual   string
		disabled, foreign, dynamic bool
		replace, conflict          bool
	}{
		{name: "disabled owned", disabled: true, replace: true},
		{name: "static to dynamic", configured: "1h", replace: true},
		{name: "dynamic to static", actual: "1h", dynamic: true, replace: true},
		{name: "dynamic without timeout to static", dynamic: true, replace: true},
		{name: "static with timeout to dynamic", configured: "1h", actual: "1h", replace: true},
		{name: "static with timeout to static", actual: "1h", replace: true},
		{name: "dynamic without timeout to dynamic", configured: "1h", dynamic: true, replace: true},
		{name: "different dynamic lifetime", configured: "1h", actual: "59m", dynamic: true, replace: true},
		{name: "valid static"},
		{name: "valid dynamic", configured: "1h", actual: "01:00:00", dynamic: true},
		{name: "foreign enabled", configured: "1h", actual: "5m", dynamic: true, foreign: true},
		{name: "foreign static", configured: "1h", foreign: true},
		{name: "foreign dynamic without timeout", dynamic: true, foreign: true},
		{name: "foreign disabled", disabled: true, foreign: true, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			c.Items[0].Domains = nil
			c.Items[0].AddressList.Timeout = tc.configured
			router := newFakeROS()
			entry := ros.ListEntry{ID: "*match", List: "ddns", Address: "203.0.113.10/32", Comment: "ros-ddns:home",
				Timeout: tc.actual, Disabled: tc.disabled, Dynamic: tc.dynamic}
			if tc.foreign {
				entry.Comment = "manual owner"
			}
			router.entries[ros.IPv4] = []ros.ListEntry{entry}
			s, _ := testService(t, c, router, &fakeProvider{})
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			status := snapshot.Items[0]
			if tc.conflict {
				if status.Phase != "degraded" || !strings.Contains(status.LastError, entry.ID) ||
					!strings.Contains(status.LastError, entry.Comment) || !strings.Contains(status.LastError, "manually") {
					t.Fatalf("foreign conflict not actionable: %#v", status)
				}
				if result := s.items[0].reconcile(context.Background(), false); result.err == nil {
					t.Fatal("foreign conflict did not return an error on retry")
				}
			} else if status.Phase != "synced" || status.LastError != "" {
				t.Fatalf("list did not sync: %#v", status)
			}
			if tc.replace {
				want := []string{"remove *match", "add 203.0.113.10"}
				if !reflect.DeepEqual(router.operations, want) || len(router.adds) != 1 {
					t.Fatalf("replacement order = %v", router.operations)
				}
				if added := router.adds[0]; added.Disabled || added.Timeout != tc.configured || added.Comment != "ros-ddns:home" {
					t.Fatalf("replacement entry = %#v", added)
				}
			} else if len(router.operations) != 0 {
				t.Fatalf("unexpected writes: %v", router.operations)
			}
			wantRenewal := !tc.conflict && ((tc.foreign && tc.dynamic) || (!tc.foreign && tc.configured != ""))
			if got := !s.items[0].listRenewAt.IsZero(); got != wantRenewal {
				t.Fatalf("renewal scheduled = %v, want %v", got, wantRenewal)
			}
		})
	}
}

func TestAddressListRemoval500Mentioning404PreventsAdd(t *testing.T) {
	router := newFakeROS()
	router.entries[ros.IPv4] = []ros.ListEntry{{ID: "old", List: "ddns", Address: "203.0.113.20", Comment: "ros-ddns:home"}}
	router.removeErrors["old"] = &ros.APIError{StatusCode: 500, Message: "backend failed", Detail: "upstream diagnostic mentions 404"}
	s, _ := testService(t, testConfig(), router, &fakeProvider{})
	snapshot, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := snapshot.Items[0]
	if len(router.adds) != 0 || !reflect.DeepEqual(router.removes, []string{"old"}) || status.Phase != "degraded" ||
		!strings.Contains(status.AddressList.LastError, "500") || !strings.Contains(status.AddressList.LastError, "404") {
		t.Fatalf("removal failure masked: status=%#v, operations=%v", status, router.operations)
	}
}

// duplicateOnAddROS simulates a competing owner adding an entry between our
// initial list read and add. The service must observe it before claiming sync.
type duplicateOnAddROS struct {
	*fakeROS
	conflict ros.ListEntry
}

func (r *duplicateOnAddROS) AddEntry(ctx context.Context, family ros.Family, entry ros.ListEntry) error {
	r.mu.Lock()
	r.entries[family] = append(r.entries[family], r.conflict)
	r.addError = fmt.Errorf("concurrent add: %w", ros.ErrEntryExists)
	r.mu.Unlock()
	return r.fakeROS.AddEntry(ctx, family, entry)
}

func TestDuplicateAddRequiresObservedValidEntry(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign disabled=%v", disabled), func(t *testing.T) {
			router := newFakeROS()
			conflict := ros.ListEntry{ID: "foreign", List: "ddns", Address: "203.0.113.10", Comment: "manual owner", Disabled: disabled}
			s, _ := testService(t, testConfig(), router, &fakeProvider{})
			s.client = &duplicateOnAddROS{fakeROS: router, conflict: conflict}
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			status := snapshot.Items[0]
			if status.Phase != "degraded" || status.AddressList.LastError == "" || !status.LastSyncAt.IsZero() ||
				len(status.AddressList.Entries) != 0 || s.items[0].listValue != "" || len(router.adds) != 1 {
				t.Fatalf("duplicate add fabricated success: %#v", status)
			}
			cached, ok := s.store.Get("home")
			if !ok || cached.AddressList.Value != "" || cached.AddressList.LastError == "" {
				t.Fatalf("duplicate was persisted as a success: %#v", cached)
			}
			result := s.items[0].reconcile(context.Background(), false)
			status = s.Snapshot().Items[0]
			if disabled {
				if result.err == nil || status.Phase != "degraded" || !strings.Contains(status.LastError, "foreign disabled") {
					t.Fatalf("disabled foreign conflict fabricated success: %#v, %v", status, result.err)
				}
			} else if result.err != nil || status.Phase != "synced" || status.AddressList.LastError != "" || s.items[0].listValue != conflict.Address {
				t.Fatalf("valid observed entry failed to sync: %#v, %v", status, result.err)
			}
			if router.listCalls != 2 || len(router.adds) != 1 || len(router.removes) != 0 || !reflect.DeepEqual(router.entries[ros.IPv4], []ros.ListEntry{conflict}) {
				t.Fatalf("retry modified the foreign entry or added a duplicate: %v", router.operations)
			}
		})
	}
}

func TestDuplicatePhraseAloneDoesNotConfirmAdd(t *testing.T) {
	for _, failure := range []error{
		errors.New("already have such entry"),
		&ros.APIError{StatusCode: 500, Message: "already have such entry"},
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			router := newFakeROS()
			router.addError = failure
			s, _ := testService(t, testConfig(), router, &fakeProvider{})
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			status := snapshot.Items[0]
			if status.Phase != "degraded" || !strings.Contains(status.AddressList.LastError, failure.Error()) ||
				!status.LastSyncAt.IsZero() || s.items[0].listValue != "" {
				t.Fatalf("duplicate phrase fabricated success: %#v", status)
			}
		})
	}
}
