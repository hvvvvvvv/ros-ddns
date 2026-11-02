package eui64

import (
	"net"
	"net/netip"
	"testing"
)

func TestParseMAC(t *testing.T) {
	t.Parallel()

	valid := []string{
		"00:1A:2B:3C:4D:5E",
		"00-1a-2b-3c-4d-5e",
		"001a.2b3c.4d5e",
		"001a2b3c4d5e",
		" 02:00:00:00:00:01 ",
	}
	for _, in := range valid {
		t.Run("valid_"+in, func(t *testing.T) {
			t.Parallel()
			hw, err := ParseMAC(in)
			if err != nil {
				t.Fatalf("ParseMAC(%q): %v", in, err)
			}
			if len(hw) != 6 {
				t.Fatalf("ParseMAC(%q) length = %d, want 6", in, len(hw))
			}
		})
	}

	invalid := []string{
		"",
		"01:00:5e:00:00:01",       // multicast
		"ff:ff:ff:ff:ff:ff",       // broadcast
		"00:00:00:00:00:00",       // all-zero
		"00:11:22:33:44",          // too short
		"00:11:22:33:44:55:66:77", // EUI-64
		"00:11:22:33:44:5g",       // not hex
		"00112233445",             // 11 hex digits
	}
	for _, in := range invalid {
		t.Run("invalid_"+in, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseMAC(in); err == nil {
				t.Fatalf("ParseMAC(%q) succeeded, want error", in)
			}
		})
	}
}

func TestDerive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		mac  string
		want string
	}{
		{
			name: "global unicast MAC",
			base: "2001:db8:1234:5678:aaaa:bbbb:cccc:dddd",
			mac:  "00:1A:2B:3C:4D:5E",
			want: "2001:db8:1234:5678:21a:2bff:fe3c:4d5e",
		},
		{
			name: "locally administered MAC flips U/L bit",
			base: "2001:db8:1:2::1",
			mac:  "02:00:00:00:00:01",
			want: "2001:db8:1:2:0:ff:fe00:1",
		},
		{
			name: "prefix bits only",
			base: "fd00:abcd::ffff:1",
			mac:  "0a:0b:0c:0d:0e:0f",
			want: "fd00:abcd::80b:cff:fe0d:e0f",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mac, err := ParseMAC(tt.mac)
			if err != nil {
				t.Fatalf("ParseMAC(%q): %v", tt.mac, err)
			}
			got, err := Derive(netip.MustParseAddr(tt.base), mac)
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("Derive(%s, %s) = %s, want %s", tt.base, tt.mac, got, tt.want)
			}
		})
	}
}

func TestDeriveRejectsWrongInputs(t *testing.T) {
	t.Parallel()

	mac := net.HardwareAddr{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e}
	if _, err := Derive(netip.MustParseAddr("192.0.2.1"), mac); err == nil {
		t.Fatal("Derive accepted an IPv4 base address")
	}
	if _, err := Derive(netip.MustParseAddr("2001:db8::1"), net.HardwareAddr{0x01, 0x02}); err == nil {
		t.Fatal("Derive accepted a short MAC")
	}
	if _, err := Derive(netip.MustParseAddr("2001:db8::1"), net.HardwareAddr{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}); err == nil {
		t.Fatal("Derive accepted a multicast MAC")
	}
	if _, err := Derive(netip.MustParseAddr("2001:db8::1"), net.HardwareAddr{0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("Derive accepted an all-zero MAC")
	}
}
