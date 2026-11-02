package config

import (
	"net"
	"net/netip"
	"reflect"
	"testing"
)

func TestCloneItemOwnership(t *testing.T) {
	fixture := func() ItemConfig {
		prefix := netip.MustParsePrefix("2001:db8::/64")
		return ItemConfig{
			Name: "home",
			Source: SourceConfig{
				Interface: "wan", Family: FamilyIPv6, EUI64MAC: "00:11:22:33:44:55",
				EUI64MACAddr: net.HardwareAddr{0, 0x11, 0x22, 0x33, 0x44, 0x55},
				Select:       SelectConfig{Scope: ScopeAny, Prefix: prefix.String(), PrefixNet: &prefix},
			},
			Domains:     []DomainConfig{{Provider: "test", Domain: "example.com", RR: "home", TTL: 600, ttlSet: true}},
			AddressList: &AddressListConfig{List: "ddns", Timeout: "1h", Comment: "home"},
		}
	}
	for _, mutateClone := range []bool{false, true} {
		name := "mutate input"
		if mutateClone {
			name = "mutate clone"
		}
		t.Run(name, func(t *testing.T) {
			original := fixture()
			cloned := CloneItem(original)
			if !reflect.DeepEqual(cloned, original) {
				t.Fatalf("clone = %#v, want %#v", cloned, original)
			}
			changed, untouched := &original, &cloned
			if mutateClone {
				changed, untouched = &cloned, &original
			}
			changed.Name = "changed"
			changed.Domains[0].Domain = "changed.example"
			changed.Source.EUI64MACAddr[0] = 0xff
			*changed.Source.Select.PrefixNet = netip.MustParsePrefix("2001:db8:1::/64")
			changed.AddressList.List = "changed"
			if want := fixture(); !reflect.DeepEqual(*untouched, want) {
				t.Fatalf("mutation crossed ownership boundary: got %#v, want %#v", *untouched, want)
			}
		})
	}
}

func TestCloneItemZeroValue(t *testing.T) {
	if got := CloneItem(ItemConfig{}); !reflect.DeepEqual(got, ItemConfig{}) {
		t.Fatalf("zero-value clone = %#v", got)
	}
}
