// Package eui64 parses 48-bit MAC addresses and derives EUI-64 interface
// identifiers as defined by RFC 4291 (Appendix A).
package eui64

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

const macLen = 6

// ParseMAC parses and validates a 48-bit unicast MAC address.
//
// Accepted forms are the ones understood by net.ParseMAC (colon, hyphen and
// dotted separators) plus 12 contiguous hexadecimal digits. EUI-64 (8-byte),
// all-zero, multicast and broadcast addresses are rejected.
func ParseMAC(s string) (net.HardwareAddr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("MAC address is empty")
	}

	normalized := s
	if !strings.ContainsAny(s, ":-.") {
		if len(s) != 12 {
			return nil, fmt.Errorf("invalid MAC %q: expected 6 octets or 12 hex digits", s)
		}
		var b strings.Builder
		for i := 0; i < len(s); i += 2 {
			if i > 0 {
				b.WriteByte(':')
			}
			b.WriteString(s[i : i+2])
		}
		normalized = b.String()
	}

	hw, err := net.ParseMAC(normalized)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC %q: %w", s, err)
	}
	if len(hw) != macLen {
		return nil, fmt.Errorf("invalid MAC %q: expected 6 octets, got %d", s, len(hw))
	}
	if hw[0]&0x01 != 0 {
		return nil, fmt.Errorf("invalid MAC %q: multicast/broadcast address", s)
	}

	allZero := true
	for _, b := range hw {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil, fmt.Errorf("invalid MAC %q: all-zero address", s)
	}

	out := make(net.HardwareAddr, len(hw))
	copy(out, hw)
	return out, nil
}

// Derive builds an EUI-64 IPv6 address from the first 64 bits of base and the
// supplied MAC address. The U/L bit is flipped and 0xFF,0xFE is inserted in the
// middle of the MAC, per RFC 4291.
func Derive(base netip.Addr, mac net.HardwareAddr) (netip.Addr, error) {
	if !base.IsValid() {
		return netip.Addr{}, fmt.Errorf("invalid base address")
	}
	if !base.Is6() || base.Is4In6() {
		return netip.Addr{}, fmt.Errorf("base address %s is not IPv6", base)
	}
	if len(mac) != macLen {
		return netip.Addr{}, fmt.Errorf("invalid MAC length %d: expected 6 octets", len(mac))
	}
	if mac[0]&0x01 != 0 {
		return netip.Addr{}, fmt.Errorf("invalid MAC %s: multicast/broadcast address", mac)
	}
	allZero := true
	for _, b := range mac {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return netip.Addr{}, fmt.Errorf("invalid MAC %s: all-zero address", mac)
	}

	base16 := base.As16()
	var addr [16]byte
	copy(addr[:8], base16[:8])
	addr[8] = mac[0] ^ 0x02
	addr[9] = mac[1]
	addr[10] = mac[2]
	addr[11] = 0xff
	addr[12] = 0xfe
	addr[13] = mac[3]
	addr[14] = mac[4]
	addr[15] = mac[5]

	return netip.AddrFrom16(addr), nil
}
