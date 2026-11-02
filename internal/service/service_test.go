package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/eui64"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/state"
)

type fakeROS struct {
	mu           sync.Mutex
	addresses    map[ros.Family][]ros.Address
	pollErrors   map[ros.Family]error
	pollCalls    map[ros.Family]int
	entries      map[ros.Family][]ros.ListEntry
	listCalls    int
	adds         []ros.ListEntry
	removes      []string
	operations   []string
	listError    error
	addError     error
	removeErrors map[string]error
	addressFunc  func(context.Context, ros.Family) ([]ros.Address, error)
	listFunc     func(context.Context, ros.Family, string) error
}

func newFakeROS() *fakeROS {
	return &fakeROS{
		addresses:  map[ros.Family][]ros.Address{ros.IPv4: {address("203.0.113.10/24")}},
		pollErrors: make(map[ros.Family]error), pollCalls: make(map[ros.Family]int),
		entries: make(map[ros.Family][]ros.ListEntry), removeErrors: make(map[string]error),
	}
}

func address(prefix string) ros.Address {
	return ros.Address{ID: "*1", Interface: "wan", Prefix: netip.MustParsePrefix(prefix)}
}

func (f *fakeROS) Addresses(ctx context.Context, family ros.Family) ([]ros.Address, error) {
	f.mu.Lock()
	f.pollCalls[family]++
	fn := f.addressFunc
	addresses := append([]ros.Address(nil), f.addresses[family]...)
	err := f.pollErrors[family]
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, family)
	}
	return addresses, err
}

func (f *fakeROS) ListEntries(ctx context.Context, family ros.Family, list string) ([]ros.ListEntry, error) {
	f.mu.Lock()
	f.listCalls++
	var entries []ros.ListEntry
	for _, entry := range f.entries[family] {
		if entry.List == list {
			entries = append(entries, entry)
		}
	}
	err, fn := f.listError, f.listFunc
	f.mu.Unlock()
	if err == nil && fn != nil {
		err = fn(ctx, family, list)
	}
	return entries, err
}

func (f *fakeROS) AddEntry(_ context.Context, family ros.Family, entry ros.ListEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, entry)
	f.operations = append(f.operations, "add "+entry.Address)
	if f.addError != nil {
		return f.addError
	}
	entry.ID = fmt.Sprintf("*added-%d", len(f.adds))
	entry.Dynamic = entry.Timeout != ""
	f.entries[family] = append(f.entries[family], entry)
	return nil
}

func (f *fakeROS) RemoveEntry(_ context.Context, family ros.Family, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, id)
	f.operations = append(f.operations, "remove "+id)
	if err := f.removeErrors[id]; err != nil {
		return err
	}
	for i, entry := range f.entries[family] {
		if entry.ID == id {
			f.entries[family] = append(f.entries[family][:i], f.entries[family][i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeROS) setAddresses(family ros.Family, addrs ...ros.Address) {
	f.mu.Lock()
	f.addresses[family] = addrs
	f.mu.Unlock()
}

type ensureCall struct {
	record dns.Record
	hint   string
}

type fakeProvider struct {
	mu         sync.Mutex
	calls      []ensureCall
	failures   map[string]int
	errorText  string
	ensureFunc func(context.Context, dns.Record) error
	changed    *bool
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Ensure(ctx context.Context, r dns.Record, hint string) (dns.EnsureResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, ensureCall{record: r, hint: hint})
	fail := p.failures[r.FQDN()] > 0
	if fail {
		p.failures[r.FQDN()]--
	}
	errorText := p.errorText
	fn := p.ensureFunc
	changed := true
	if p.changed != nil {
		changed = *p.changed
	}
	p.mu.Unlock()
	if fail {
		if errorText == "" {
			errorText = "temporary DNS failure"
		}
		return dns.EnsureResult{}, errors.New(errorText)
	}
	if fn != nil {
		if err := fn(ctx, r); err != nil {
			return dns.EnsureResult{}, err
		}
	}
	return dns.EnsureResult{RecordID: "id-" + r.FQDN(), Value: r.Value, Changed: changed}, nil
}

func (p *fakeProvider) setChanged(changed bool) {
	p.mu.Lock()
	p.changed = &changed
	p.mu.Unlock()
}

func (p *fakeProvider) recorded() []ensureCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ensureCall(nil), p.calls...)
}

func (p *fakeProvider) fail(fqdn string, count int) {
	p.mu.Lock()
	if p.failures == nil {
		p.failures = make(map[string]int)
	}
	p.failures[fqdn] = count
	p.mu.Unlock()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testConfig() *config.Config {
	c := config.Default()
	c.Path = "test-config.yaml"
	c.State.Path = ""
	c.Runtime.PollInterval.Duration = time.Hour
	c.Runtime.MaxBackoff.Duration = 24 * time.Hour
	c.Runtime.ReconcileTimeout.Duration = time.Second
	c.Runtime.VerifyInterval.Duration = 0
	c.RouterOS.URL = "https://router.example"
	c.RouterOS.Username = "ddns"
	c.DNS.Providers["test"] = config.ProviderConfig{Type: "aliyun"}
	c.Items = []config.ItemConfig{{
		Name: "home", Source: config.SourceConfig{Interface: "wan", Family: "ipv4", Select: config.SelectConfig{Scope: "any"}},
		Domains: []config.DomainConfig{
			{Provider: "test", Domain: "example.com", RR: "home", TTL: 600, Line: "default"},
			{Provider: "test", Domain: "example.net", RR: "@", TTL: 300, Line: "telecom"},
		},
		AddressList: &config.AddressListConfig{List: "ddns", Comment: "ros-ddns:home"},
	}}
	return c
}

func fakeSelector(addrs []ros.Address, _ ros.SelectOptions) (ros.Selection, error) {
	if len(addrs) == 0 {
		return ros.Selection{}, ros.ErrNoEligibleAddress
	}
	return ros.Selection{Address: addrs[0]}, nil
}

func testService(t *testing.T, c *config.Config, router *fakeROS, provider *fakeProvider) (*Service, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	s, err := New(c, Deps{
		NewROS:      func(config.RouterOSConfig) (ros.Client, error) { return router, nil },
		NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) { return provider, nil },
		Selector:    fakeSelector, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, clock
}

func pollAndReconcile(t *testing.T, s *Service, force bool) {
	t.Helper()
	if err := s.poller.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, it := range s.items {
		it.takeRequest()
		if result := it.reconcile(context.Background(), force); result.err != nil {
			t.Fatal(result.err)
		}
	}
}

func TestFirstSyncUnchangedPollAndDesiredChange(t *testing.T) {
	router, provider := newFakeROS(), &fakeProvider{}
	s, clock := testService(t, testConfig(), router, provider)
	if s.Ready() {
		t.Fatal("ready before a fresh poll")
	}
	snapshot, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := snapshot.Items[0]
	if snapshot.ConfigPath != "test-config.yaml" || snapshot.StartedAt != clock.now() || !s.Ready() {
		t.Fatalf("bad snapshot metadata or readiness: %#v", snapshot)
	}
	if status.Phase != "synced" || status.SourceAddress != "203.0.113.10/24" || status.RecordValue != "203.0.113.10" || status.LastSyncAt != clock.now() {
		t.Fatalf("first status: %#v", status)
	}
	calls := provider.recorded()
	if len(calls) != 2 || len(router.adds) != 1 || router.listCalls != 1 || router.pollCalls[ros.IPv4] != 1 || router.pollCalls[ros.IPv6] != 0 {
		t.Fatalf("wrong calls: dns=%v adds=%v polls=%v", calls, router.adds, router.pollCalls)
	}
	for i, call := range calls {
		if call.record.Value != status.RecordValue || call.record.Type != "A" || call.record.TTL != status.Domains[i].TTL || call.record.Line != status.Domains[i].Line || call.hint != "" {
			t.Fatalf("bad DNS record: %#v", call)
		}
	}
	clock.advance(time.Hour)
	pollAndReconcile(t, s, false)
	if len(provider.recorded()) != 2 || len(router.adds) != 1 || router.listCalls != 1 {
		t.Fatal("unchanged poll rewrote or reread a synced target")
	}
	if len(s.items[0].wake) != 0 {
		t.Fatal("unchanged poll queued reconciliation")
	}
	router.setAddresses(ros.IPv4, address("203.0.113.11/24"))
	clock.advance(time.Hour)
	pollAndReconcile(t, s, false)
	calls = provider.recorded()
	if len(calls) != 4 {
		t.Fatalf("new desired did not update every binding: %v", calls)
	}
	for i, call := range calls[2:] {
		if call.record.Value != "203.0.113.11" || call.hint != status.Domains[i].RecordID {
			t.Fatalf("missing new value or RecordID hint: %#v", call)
		}
	}
	wantOps := []string{"add 203.0.113.10", "remove *added-1", "add 203.0.113.11"}
	if !reflect.DeepEqual(router.operations, wantOps) {
		t.Fatalf("list replacement order: %v", router.operations)
	}
	if got := s.Snapshot().Items[0].AddressList.Entries; !reflect.DeepEqual(got, []string{"203.0.113.11"}) {
		t.Fatalf("list status = %v", got)
	}
}

func TestIndependentFailuresAndRetryOnlyFailedBinding(t *testing.T) {
	router, provider := newFakeROS(), &fakeProvider{}
	provider.fail("home.example.com", 1)
	s, _ := testService(t, testConfig(), router, provider)
	snapshot, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatalf("per-binding error escaped runOnce: %v", err)
	}
	status := snapshot.Items[0]
	if status.Phase != "degraded" || status.Domains[0].LastError == "" || status.Domains[1].LastError != "" || status.Domains[1].Value == "" || len(router.adds) != 1 {
		t.Fatalf("partial successes were not preserved: %#v", status)
	}
	if result := s.items[0].reconcile(context.Background(), false); result.err != nil {
		t.Fatal(result.err)
	}
	calls := provider.recorded()
	if len(calls) != 3 || calls[2].record.FQDN() != "home.example.com" || router.listCalls != 1 || s.Snapshot().Items[0].Phase != "synced" {
		t.Fatalf("retry touched successful targets: %v", calls)
	}
	// A failed forced verification must be retried even though the last known
	// confirmed value still equals desired.
	provider.fail("example.net", 1)
	if result := s.items[0].reconcile(context.Background(), true); result.err == nil {
		t.Fatal("forced verification should have failed")
	}
	if result := s.items[0].reconcile(context.Background(), false); result.err != nil {
		t.Fatal(result.err)
	}
	calls = provider.recorded()
	if len(calls) != 6 || calls[5].record.FQDN() != "example.net" || router.listCalls != 2 {
		t.Fatalf("failed verification was not independently retried: %v", calls)
	}
}

func TestEUI64AndDerivationError(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			c := testConfig()
			c.Items[0].Source.Family = "ipv6"
			c.Items[0].Source.EUI64MAC = "00:1A:2B:3C:4D:5E"
			mac, err := eui64.ParseMAC(c.Items[0].Source.EUI64MAC)
			if err != nil {
				t.Fatal(err)
			}
			c.Items[0].Source.EUI64MACAddr = mac
			if invalid {
				c.Items[0].Source.EUI64MACAddr = mac[:4]
			}
			router, provider := newFakeROS(), &fakeProvider{}
			router.setAddresses(ros.IPv6, address("2001:db8:1234:5678::beef/64"))
			s, _ := testService(t, c, router, provider)
			snapshot, err := s.runOnce(context.Background())
			if err != nil || !s.Ready() {
				t.Fatalf("runOnce error=%v ready=%v", err, s.Ready())
			}
			status := snapshot.Items[0]
			if invalid {
				if status.Phase != "degraded" || !strings.Contains(status.LastError, "derive EUI-64") || len(provider.recorded()) != 0 || router.listCalls != 0 {
					t.Fatalf("derived an invalid MAC: %#v", status)
				}
				return
			}
			want := "2001:db8:1234:5678:21a:2bff:fe3c:4d5e"
			if status.RecordValue != want || status.SourceAddress != "2001:db8:1234:5678::beef/64" || status.EUI64MAC != "00:1a:2b:3c:4d:5e" {
				t.Fatalf("wrong EUI-64 status: %#v", status)
			}
			for _, call := range provider.recorded() {
				if call.record.Value != want || call.record.Type != "AAAA" {
					t.Fatalf("wrong derived DNS record: %#v", call)
				}
			}
			if router.adds[0].Address != want || router.pollCalls[ros.IPv4] != 0 || router.pollCalls[ros.IPv6] != 1 {
				t.Fatalf("wrong family or list value: %#v", router.adds)
			}
		})
	}
}

func TestEmptySourcePreservesTargets(t *testing.T) {
	router, provider := newFakeROS(), &fakeProvider{}
	s, clock := testService(t, testConfig(), router, provider)
	first, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	router.setAddresses(ros.IPv4)
	clock.advance(time.Hour)
	second, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := second.Items[0]
	if status.Phase != "waiting" || !s.Ready() || len(provider.recorded()) != 2 || router.listCalls != 1 || len(router.removes) != 0 {
		t.Fatalf("empty source mutated targets: %#v", status)
	}
	if !reflect.DeepEqual(status.Domains, first.Items[0].Domains) || !reflect.DeepEqual(status.AddressList, first.Items[0].AddressList) || s.items[0].desired != "203.0.113.10" {
		t.Fatal("empty source cleared confirmed or desired values")
	}
	// A first successful but empty poll counts for readiness too.
	fresh, _ := testService(t, testConfig(), router, &fakeProvider{})
	if _, err := fresh.runOnce(context.Background()); err != nil || !fresh.Ready() {
		t.Fatalf("empty first poll: err=%v ready=%v", err, fresh.Ready())
	}
}

func TestAddressListOwnershipAndNormalization(t *testing.T) {
	for _, family := range []ros.Family{ros.IPv4, ros.IPv6} {
		t.Run(string(family), func(t *testing.T) {
			c := testConfig()
			c.Items[0].Domains = nil
			c.Items[0].Source.Family = string(family)
			router := newFakeROS()
			desired, stale, other := "203.0.113.10/32", "203.0.113.20/24", "203.0.113.30"
			if family == ros.IPv6 {
				router.setAddresses(family, address("2001:db8::10/64"))
				desired, stale, other = "2001:0db8:0:0::10/128", "2001:db8::20/64", "2001:db8::30"
			}
			router.entries[family] = []ros.ListEntry{
				{ID: "stale-managed", List: "ddns", Address: stale, Comment: "ros-ddns:home"},
				{ID: "other-owner", List: "ddns", Address: other, Comment: "ros-ddns:home-other"},
				{ID: "foreign-desired", List: "ddns", Address: desired, Comment: "another owner"},
				{ID: "different-list", List: "other", Address: stale, Comment: "ros-ddns:home"},
			}
			s, _ := testService(t, c, router, &fakeProvider{})
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(router.removes, []string{"stale-managed"}) || len(router.adds) != 0 {
				t.Fatalf("ownership/duplicate failure: removals=%v additions=%v", router.removes, router.adds)
			}
			if got := snapshot.Items[0].AddressList.Entries; !reflect.DeepEqual(got, []string{normalizedAddress(desired)}) {
				t.Fatalf("list status was not normalized/scoped: %v", got)
			}
		})
	}
}

func TestAddressListAddErrorsAndFailedRemoval(t *testing.T) {
	for _, removal := range []error{errors.New("HTTP 404: gone"), errors.New("HTTP 500: failure"), nil} {
		t.Run(fmt.Sprint(removal), func(t *testing.T) {
			router := newFakeROS()
			router.entries[ros.IPv4] = []ros.ListEntry{{ID: "old", List: "ddns", Comment: "ros-ddns:home", Address: "203.0.113.20/32"}}
			router.removeErrors["old"] = removal
			router.addError = errors.New("failure: already have such entry")
			s, _ := testService(t, testConfig(), router, &fakeProvider{})
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			status := snapshot.Items[0]
			if removal != nil {
				if status.Phase != "degraded" || status.AddressList.LastError == "" || len(router.adds) != 0 {
					t.Fatalf("failed removal must defer addition: %#v", status)
				}
			} else if status.Phase != "degraded" || len(router.adds) != 1 || status.AddressList.LastError == "" {
				t.Fatalf("untyped duplicate error fabricated success: %#v", status)
			}
		})
	}
}

func TestPersistedHintsStartupForceAndFreshReadiness(t *testing.T) {
	c := testConfig()
	c.State.Path = filepath.Join(t.TempDir(), "state.json")
	router, provider := newFakeROS(), &fakeProvider{}
	s, _ := testService(t, c, router, provider)
	first, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	provider = &fakeProvider{}
	restarted, _ := testService(t, c, router, provider)
	if restarted.Ready() || restarted.Snapshot().Items[0].Phase != "waiting" {
		t.Fatal("persisted state bypassed fresh source readiness")
	}
	if result := restarted.items[0].reconcile(context.Background(), true); result.err != nil || len(provider.recorded()) != 0 {
		t.Fatal("reconciled from disk before a successful poll")
	}
	if err := restarted.poller.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	force, usable := restarted.items[0].takeRequest()
	if !force || !usable {
		t.Fatal("startup force not retained until fresh source was ready")
	}
	if result := restarted.items[0].reconcile(context.Background(), force); result.err != nil {
		t.Fatal(result.err)
	}
	calls := provider.recorded()
	if len(calls) != 2 || calls[0].hint != first.Items[0].Domains[0].RecordID || calls[1].hint != first.Items[0].Domains[1].RecordID {
		t.Fatalf("persisted RecordIDs not passed to provider: %v", calls)
	}
	// With SyncOnStart off, DNS caches remain valid, but every restored list
	// must be observed again because a foreign dynamic entry may have expired.
	c.Runtime.SyncOnStart = false
	provider = &fakeProvider{}
	quiet, _ := testService(t, c, router, provider)
	if err := quiet.poller.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(quiet.items[0].wake) != 1 || quiet.Snapshot().Items[0].Phase == "synced" {
		t.Fatal("SyncOnStart=false trusted the restored list without revalidation")
	}
	reads := router.listCalls
	pollAndReconcile(t, quiet, false)
	if len(provider.recorded()) != 0 || router.listCalls != reads+1 || len(router.adds) != 1 {
		t.Fatal("list cache was not revalidated independently of cached DNS")
	}
}

func TestDisabledItemsFactoriesAndSelectorOptions(t *testing.T) {
	c := testConfig()
	second := cloneConfigItem(c.Items[0])
	second.Name, second.Source.Family = "v6", "ipv6"
	second.AddressList = nil
	c.Items = append(c.Items, second)
	disabled := cloneConfigItem(c.Items[0])
	disabled.Name, disabled.Disabled = "off", true
	disabled.Domains[0].Provider = "not-constructed"
	c.Items = append(c.Items, disabled)
	prefix := netip.MustParsePrefix("203.0.113.0/24")
	c.Items[0].Source.Select = config.SelectConfig{Scope: "any", Prefix: prefix.String(), PrefixNet: &prefix, Index: 2}
	router, provider := newFakeROS(), &fakeProvider{}
	router.setAddresses(ros.IPv6, address("2001:db8::1/64"))
	rosFactories, providerFactories := 0, 0
	var optionsMu sync.Mutex
	options := make(map[string]ros.SelectOptions)
	s, err := New(c, Deps{
		NewROS: func(config.RouterOSConfig) (ros.Client, error) { rosFactories++; return router, nil },
		NewProvider: func(name string, _ config.ProviderConfig) (dns.Provider, error) {
			providerFactories++
			if name != "test" {
				t.Fatalf("unused provider constructed: %s", name)
			}
			return provider, nil
		},
		Selector: func(a []ros.Address, o ros.SelectOptions) (ros.Selection, error) {
			optionsMu.Lock()
			options[a[0].Prefix.String()] = o
			optionsMu.Unlock()
			return fakeSelector(a, o)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rosFactories != 1 || providerFactories != 1 || router.pollCalls[ros.IPv4] != 1 || router.pollCalls[ros.IPv6] != 1 || len(options) != 2 {
		t.Fatalf("shared factory/poll counts incorrect: %d %d %v", rosFactories, providerFactories, router.pollCalls)
	}
	if o := options["203.0.113.10/24"]; o.Interface != "wan" || o.Index != 2 || o.Scope != "any" || o.Prefix == nil || *o.Prefix != prefix {
		t.Fatalf("lost selection options: %#v", o)
	}
	if len(snapshot.Items) != 3 || snapshot.Items[2].Phase != "disabled" || !snapshot.Items[2].Disabled || s.TriggerItem("off") || s.TriggerItem("missing") {
		t.Fatalf("disabled/unknown item handling: %#v", snapshot.Items)
	}
	// Deep copies isolate both caller-owned config and API response slices.
	c.Items[0].Domains[0].Domain = "mutated.example"
	snapshot.Items[0].Domains[0].Value = "mutated"
	snapshot.Items[0].AddressList.Entries[0] = "mutated"
	snapshot.Items[2].Domains[0].FQDN = "mutated"
	fresh := s.Snapshot()
	if fresh.Items[0].Domains[0].Value == "mutated" || fresh.Items[0].AddressList.Entries[0] == "mutated" || fresh.Items[2].Domains[0].FQDN == "mutated" || s.items[0].cfg.Domains[0].Domain != "example.com" {
		t.Fatal("snapshot or config data was aliased")
	}
}

func TestDependencyDefaultsAndSetupErrors(t *testing.T) {
	c := testConfig()
	router := newFakeROS()
	deps := Deps{NewROS: func(config.RouterOSConfig) (ros.Client, error) { return router, nil }, NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) { return &fakeProvider{}, nil }}
	s, err := New(c, deps)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(s.selector).Pointer() != reflect.ValueOf(ros.Select).Pointer() || s.now == nil {
		t.Fatal("default selector/clock not wired")
	}
	if _, err := New(nil, deps); err == nil {
		t.Fatal("nil config accepted")
	}
	missingFactory := deps
	missingFactory.NewProvider = nil
	if _, err := New(c, missingFactory); err == nil {
		t.Fatal("missing provider factory accepted")
	}
	nilROS := deps
	nilROS.NewROS = func(config.RouterOSConfig) (ros.Client, error) { return nil, nil }
	if _, err := New(c, nilROS); err == nil {
		t.Fatal("nil ROS client accepted")
	}
	// An injected store takes precedence over a path that cannot be opened.
	c.State.Path = t.TempDir()
	deps.State = &state.Store{}
	if _, err := New(c, deps); err != nil {
		t.Fatalf("injected state was ignored: %v", err)
	}
}

func TestErrorSecretsAreRedactedFromSnapshotAndState(t *testing.T) {
	c := testConfig()
	c.RouterOS.Password = "router-super-secret"
	c.DNS.Providers["test"] = config.ProviderConfig{AccessKeyID: "test-access-key", AccessKeySecret: "provider-super-secret", SecurityToken: "temporary-session-token"}
	c.State.Path = filepath.Join(t.TempDir(), "state.json")
	provider := &fakeProvider{errorText: "failure with router-super-secret test-access-key provider-super-secret temporary-session-token"}
	provider.fail("home.example.com", 1)
	s, _ := testService(t, c, newFakeROS(), provider)
	snapshot, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.State.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.RouterOS.Password, "test-access-key", "provider-super-secret", "temporary-session-token"} {
		if strings.Contains(snapshot.Items[0].LastError, secret) || strings.Contains(string(data), secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	if !strings.Contains(snapshot.Items[0].LastError, "[redacted]") {
		t.Fatal("error was not preserved with redaction")
	}
}

func TestTTLChangeRetainsRecordIDAndInvalidatesOnlyChangedBinding(t *testing.T) {
	c := testConfig()
	c.State.Path = filepath.Join(t.TempDir(), "state.json")
	router := newFakeROS()
	s, _ := testService(t, c, router, &fakeProvider{})
	first, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Runtime.SyncOnStart = false
	c.Items[0].Domains[0].TTL = 120
	provider := &fakeProvider{}
	restarted, _ := testService(t, c, router, provider)
	if err := restarted.poller.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A restart between polling and Ensure must not cache the new TTL as
	// confirmed. It should still retain the previous record ID.
	restarted, _ = testService(t, c, router, provider)
	pollAndReconcile(t, restarted, false)
	calls := provider.recorded()
	if len(calls) != 1 || calls[0].hint != first.Items[0].Domains[0].RecordID || calls[0].record.TTL != 120 || router.listCalls != 2 || len(router.adds) != 1 {
		t.Fatalf("TTL change lost hint or rewrote unaffected targets: %v", calls)
	}
}

func TestDomainUpdatedAtTracksOnlyActualWrites(t *testing.T) {
	c := testConfig()
	c.State.Path = ""
	provider := &fakeProvider{}
	s, clock := testService(t, c, newFakeROS(), provider)
	firstAt := clock.now()

	first, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range first.Items[0].Domains {
		if !domain.UpdatedAt.Equal(firstAt) {
			t.Fatalf("initial add timestamp = %s, want %s", domain.UpdatedAt, firstAt)
		}
	}

	clock.advance(time.Hour)
	provider.setChanged(false)
	second, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range second.Items[0].Domains {
		if !domain.UpdatedAt.Equal(firstAt) {
			t.Fatalf("unchanged confirmation moved timestamp to %s, want %s", domain.UpdatedAt, firstAt)
		}
	}

	clock.advance(time.Hour)
	provider.setChanged(true)
	third, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range third.Items[0].Domains {
		if !domain.UpdatedAt.Equal(clock.now()) {
			t.Fatalf("actual update timestamp = %s, want %s", domain.UpdatedAt, clock.now())
		}
	}
}
