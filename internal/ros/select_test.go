package ros

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestSelectScopes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prefix     string
		global     bool
		linkLocal  bool
		deprecated bool
		public     bool
		any        bool
	}{
		{name: "IPv4 public", prefix: "8.8.8.8/24", public: true, any: true},
		{name: "IPv4 directed broadcast", prefix: "198.51.100.255/24"},
		{name: "IPv4 normal host", prefix: "198.51.100.42/24", public: true, any: true},
		{name: "IPv4 host ending in 255", prefix: "198.51.100.255/23", public: true, any: true},
		{name: "IPv4 directed broadcast /23", prefix: "198.51.101.255/23"},
		{name: "IPv4 directed broadcast /25", prefix: "198.51.100.127/25"},
		{name: "IPv4 directed broadcast /30", prefix: "198.51.100.3/30"},
		{name: "IPv4 normal host /30", prefix: "198.51.100.2/30", public: true, any: true},
		{name: "IPv4 first host /31", prefix: "198.51.100.254/31", public: true, any: true},
		{name: "IPv4 second host /31", prefix: "198.51.100.255/31", public: true, any: true},
		{name: "IPv4 host /32", prefix: "198.51.100.255/32", public: true, any: true},
		{name: "IPv4 host /0", prefix: "198.51.100.255/0", public: true, any: true},
		{name: "IPv4 10 private", prefix: "10.1.2.3/8", any: true},
		{name: "IPv4 172 private", prefix: "172.16.1.2/12", any: true},
		{name: "IPv4 192 private", prefix: "192.168.1.2/24", any: true},
		{name: "IPv4 CGNAT first", prefix: "100.64.0.0/10", any: true},
		{name: "IPv4 CGNAT last", prefix: "100.127.255.255/32", any: true},
		{name: "IPv4 below CGNAT", prefix: "100.63.255.255/32", public: true, any: true},
		{name: "IPv4 above CGNAT", prefix: "100.128.0.0/10", public: true, any: true},
		{name: "IPv4 CGNAT directed broadcast", prefix: "100.127.255.255/10"},
		{name: "IPv4 private directed broadcast", prefix: "192.168.1.255/24"},
		{name: "IPv4 link-local", prefix: "169.254.10.20/16"},
		{name: "IPv4 loopback", prefix: "127.2.3.4/8"},
		{name: "IPv4 multicast", prefix: "224.0.0.1/4"},
		{name: "IPv4 unspecified", prefix: "0.0.0.0/0"},
		{name: "IPv4 limited broadcast", prefix: "255.255.255.255/32"},
		{name: "IPv6 global", prefix: "2001:db8::42/64", global: true, public: true, any: true},
		{name: "IPv6 all host bits set", prefix: "2001:db8::ffff:ffff:ffff:ffff/64", global: true, public: true, any: true},
		{name: "IPv6 not global", prefix: "2001:db8::42/64", any: true},
		{name: "IPv6 deprecated", prefix: "2001:db8::42/64", global: true, deprecated: true, any: true},
		{name: "IPv6 ULA fd", prefix: "fd12:3456::42/64", global: true, any: true},
		{name: "IPv6 ULA fc", prefix: "fc00::42/64", global: true, any: true},
		{name: "IPv6 link-local flag", prefix: "2001:db8::42/64", global: true, linkLocal: true},
		{name: "IPv6 link-local address", prefix: "fe80::42/64", global: true},
		{name: "IPv6 multicast", prefix: "ff02::1/128", global: true},
		{name: "IPv6 unspecified", prefix: "::/0", global: true},
		{name: "IPv6 loopback", prefix: "::1/128", global: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address := Address{
				ID: "*1", Interface: "wan", Prefix: netip.MustParsePrefix(tc.prefix),
				Global: tc.global, LinkLocal: tc.linkLocal, Deprecated: tc.deprecated,
			}
			for _, scope := range []struct {
				name string
				want bool
			}{{"public", tc.public}, {"any", tc.any}} {
				t.Run(scope.name, func(t *testing.T) {
					got, err := Select([]Address{address}, SelectOptions{Interface: "wan", Scope: scope.name})
					if !scope.want {
						if !errors.Is(err, ErrNoEligibleAddress) {
							t.Errorf("Select error = %v, want ErrNoEligibleAddress", err)
						}
						return
					}
					if err != nil {
						t.Fatalf("Select: %v", err)
					}
					if got.Address != address || !reflect.DeepEqual(got.Candidates, []netip.Addr{address.Prefix.Addr()}) {
						t.Errorf("Select = %+v, want address %+v", got, address)
					}
				})
			}
		})
	}
}

func TestSelectInterfaceAndFlags(t *testing.T) {
	for _, tc := range []struct {
		name            string
		iface           string
		actualInterface string
		disabled        bool
		invalid         bool
		want            bool
	}{
		{"interface", "wan", "ether1", false, false, true},
		{"actual-interface", "pppoe-out1", "wan", false, false, true},
		{"both match", "wan", "wan", false, false, true},
		{"neither matches", "bridge", "ether1", false, false, false},
		{"exact match only", "wan-extra", "", false, false, false},
		{"disabled", "wan", "wan", true, false, false},
		{"invalid", "wan", "wan", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address := Address{Interface: tc.iface, ActualInterface: tc.actualInterface, Prefix: netip.MustParsePrefix("203.0.113.42/24"), Disabled: tc.disabled, Invalid: tc.invalid}
			for _, scope := range []string{"public", "any"} {
				got, err := Select([]Address{address}, SelectOptions{Interface: "wan", Scope: scope})
				if tc.want {
					if err != nil || got.Address != address {
						t.Errorf("scope %s: Select = %+v, %v, want %+v", scope, got, err, address)
					}
				} else if !errors.Is(err, ErrNoEligibleAddress) {
					t.Errorf("scope %s: error = %v, want ErrNoEligibleAddress", scope, err)
				}
			}
		})
	}
}

func TestSelectPrefixSortIndexAndNoMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prefix   string
		input    []string
		want     []string
		selected int
	}{
		{"IPv4 numeric order", "203.0.113.0/24", []string{"203.0.113.100/24", "198.51.100.5/24", "203.0.113.10/24", "203.0.113.2/24"}, []string{"203.0.113.2", "203.0.113.10", "203.0.113.100"}, 2},
		{"IPv6 numeric order", "2001:db8:1::/64", []string{"2001:db8:1::100/64", "2001:db8:2::2/64", "2001:db8:1::10/64", "2001:db8:1::2/64"}, []string{"2001:db8:1::2", "2001:db8:1::10", "2001:db8:1::100"}, 2},
		{"IPv4 host prefix", "203.0.113.42/32", []string{"203.0.113.41/24", "203.0.113.42/24"}, []string{"203.0.113.42"}, 1},
		{"IPv6 host prefix", "2001:db8::42/128", []string{"2001:db8::41/64", "2001:db8::42/64"}, []string{"2001:db8::42"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addrs := make([]Address, len(tc.input))
			for i, prefix := range tc.input {
				addrs[i] = Address{ID: prefix, Interface: "wan", Prefix: netip.MustParsePrefix(prefix), Global: true, Dynamic: i%2 == 0}
			}
			original := append([]Address(nil), addrs...)
			prefix := netip.MustParsePrefix(tc.prefix)
			index := 0
			if len(tc.want) > 1 {
				index = 1
			}
			got, err := Select(addrs, SelectOptions{Interface: "wan", Scope: "public", Prefix: &prefix, Index: index})
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			want := make([]netip.Addr, len(tc.want))
			for i, ip := range tc.want {
				want[i] = netip.MustParseAddr(ip)
			}
			if !reflect.DeepEqual(got.Candidates, want) || got.Address != original[tc.selected] {
				t.Errorf("Select = %+v, want candidates %v, selected %+v", got, want, original[tc.selected])
			}
			if !reflect.DeepEqual(addrs, original) {
				t.Errorf("Select mutated input: got %+v, want %+v", addrs, original)
			}
		})
	}
}

func TestSelectErrors(t *testing.T) {
	ipv4 := Address{Interface: "wan", Prefix: netip.MustParsePrefix("203.0.113.42/24")}
	v4Filter := netip.MustParsePrefix("198.51.100.0/24")
	v6Filter := netip.MustParsePrefix("2001:db8::/32")
	for _, tc := range []struct {
		name    string
		input   []Address
		prefix  *netip.Prefix
		index   int
		message string
	}{
		{"empty", nil, nil, 0, "0 address(es)"},
		{"invalid prefix", []Address{{Interface: "wan"}}, nil, 0, "invalid-prefix=1"},
		{"prefix excluded", []Address{ipv4}, &v4Filter, 0, "prefix=1"},
		{"prefix family mismatch", []Address{ipv4}, &v6Filter, 0, "prefix=1"},
		{"index equal to count", []Address{ipv4}, nil, 1, "index 1 out of range for 1 candidate(s)"},
		{"index above count", []Address{ipv4}, nil, 10, "index 10 out of range for 1 candidate(s)"},
		{"negative index", []Address{ipv4}, nil, -1, "index -1 out of range for 1 candidate(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Select(tc.input, SelectOptions{Interface: "wan", Scope: "public", Prefix: tc.prefix, Index: tc.index})
			if !errors.Is(err, ErrNoEligibleAddress) {
				t.Fatalf("error = %v, want ErrNoEligibleAddress", err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error = %v, want %q", err, tc.message)
			}
		})
	}
	if _, err := Select([]Address{ipv4}, SelectOptions{Interface: "wan", Scope: "unsupported"}); err == nil {
		t.Error("unsupported scope must return an error")
	}
}

func TestSelectReportsFilterCounts(t *testing.T) {
	prefix := netip.MustParsePrefix("203.0.113.0/24")
	addrs := []Address{
		{Interface: "other", Prefix: netip.MustParsePrefix("203.0.113.1/24")},
		{Interface: "wan", Prefix: netip.MustParsePrefix("203.0.113.2/24"), Disabled: true},
		{Interface: "wan", Prefix: netip.MustParsePrefix("203.0.113.3/24"), Invalid: true},
		{Interface: "wan"},
		{Interface: "wan", Prefix: netip.MustParsePrefix("192.168.1.1/24")},
		{Interface: "wan", Prefix: netip.MustParsePrefix("198.51.100.1/24")},
	}
	_, err := Select(addrs, SelectOptions{Interface: "wan", Scope: "public", Prefix: &prefix})
	if !errors.Is(err, ErrNoEligibleAddress) {
		t.Fatalf("error = %v, want ErrNoEligibleAddress", err)
	}
	for _, part := range []string{`interface "wan"`, `scope="public"`, "203.0.113.0/24", "6 address(es)", "interface=1", "disabled/invalid=2", "invalid-prefix=1", "scope=1", "prefix=1"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not contain %q", err, part)
		}
	}
}
