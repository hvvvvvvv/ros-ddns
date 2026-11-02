package ros

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"

	"ros-ddns/internal/config"
)

var (
	cgnatPrefix      = netip.MustParsePrefix("100.64.0.0/10")
	limitedBroadcast = netip.MustParseAddr("255.255.255.255")
)

func selectAddress(addrs []Address, opts SelectOptions) (Selection, error) {
	if opts.Scope != config.ScopePublic && opts.Scope != config.ScopeAny {
		return Selection{}, fmt.Errorf("unsupported address selection scope %q (public|any)", opts.Scope)
	}
	var wrongInterface, disabledOrInvalid, invalidPrefix, wrongScope, wrongPrefix int
	eligible := make([]Address, 0, len(addrs))
	for _, address := range addrs {
		switch {
		case address.Interface != opts.Interface && address.ActualInterface != opts.Interface:
			wrongInterface++
		case address.Disabled || address.Invalid:
			disabledOrInvalid++
		case !address.Prefix.IsValid():
			invalidPrefix++
		case !addressInScope(address, opts.Scope):
			wrongScope++
		case opts.Prefix != nil && !opts.Prefix.Contains(address.Prefix.Addr()):
			wrongPrefix++
		default:
			eligible = append(eligible, address)
		}
	}
	if len(eligible) == 0 {
		prefix := "any"
		if opts.Prefix != nil {
			prefix = opts.Prefix.String()
		}
		return Selection{}, fmt.Errorf("%w for interface %q (scope=%q, prefix=%s): %d address(es), rejected interface=%d, disabled/invalid=%d, invalid-prefix=%d, scope=%d, prefix=%d",
			ErrNoEligibleAddress, opts.Interface, opts.Scope, prefix, len(addrs), wrongInterface, disabledOrInvalid, invalidPrefix, wrongScope, wrongPrefix)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].Prefix.Addr().Compare(eligible[j].Prefix.Addr()) < 0
	})
	if opts.Index < 0 || opts.Index >= len(eligible) {
		return Selection{}, fmt.Errorf("%w: index %d out of range for %d candidate(s)", ErrNoEligibleAddress, opts.Index, len(eligible))
	}
	candidates := make([]netip.Addr, len(eligible))
	for i, address := range eligible {
		candidates[i] = address.Prefix.Addr()
	}
	return Selection{Address: eligible[opts.Index], Candidates: candidates}, nil
}

func addressInScope(address Address, scope string) bool {
	ip := address.Prefix.Addr()
	if ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLoopback() {
		return false
	}
	if ip.Is4() {
		if ip == limitedBroadcast {
			return false
		}
		// /31 point-to-point links and /32 hosts have no directed broadcast.
		if bits := address.Prefix.Bits(); bits <= 30 {
			network := address.Prefix.Masked().Addr().As4()
			broadcast := binary.BigEndian.Uint32(network[:]) | (^uint32(0) >> bits)
			host := ip.As4()
			if binary.BigEndian.Uint32(host[:]) == broadcast {
				return false
			}
		}
		return scope == config.ScopeAny || (!ip.IsPrivate() && !cgnatPrefix.Contains(ip))
	}
	if address.LinkLocal {
		return false
	}
	return scope == config.ScopeAny || (address.Global && !address.Deprecated && !ip.IsPrivate())
}
