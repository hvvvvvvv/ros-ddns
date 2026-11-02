// Package ros contains the RouterOS REST client contract, address types and
// address selection helpers.
package ros

import (
	"context"
	"errors"
	"net/netip"

	"ros-ddns/internal/config"
)

// Family is an address family supported by the service.
type Family string

const (
	IPv4 Family = "ipv4"
	IPv6 Family = "ipv6"
)

// Valid reports whether f is a supported family.
func (f Family) Valid() bool {
	return f == IPv4 || f == IPv6
}

// RecordType returns the DNS record type corresponding to the family.
func (f Family) RecordType() string {
	if f == IPv6 {
		return "AAAA"
	}
	return "A"
}

// Address is one entry from /ip/address or /ipv6/address.
type Address struct {
	Interface       string
	ActualInterface string
	Prefix          netip.Prefix
	Disabled        bool
	Invalid         bool
	Global          bool
	LinkLocal       bool
	Deprecated      bool
}

// ListEntry is one entry from /ip/firewall/address-list or
// /ipv6/firewall/address-list.
type ListEntry struct {
	ID       string
	List     string
	Address  string
	Comment  string
	Timeout  string
	Dynamic  bool
	Disabled bool
}

// Client is the RouterOS REST contract. It is intentionally small so tests can
// fake it and a future native-API transport can implement the same interface.
type Client interface {
	Addresses(ctx context.Context, family Family) ([]Address, error)
	ListEntries(ctx context.Context, family Family, list string) ([]ListEntry, error)
	AddEntry(ctx context.Context, family Family, e ListEntry) error
	RemoveEntry(ctx context.Context, family Family, id string) error
}

// SelectOptions narrows the candidate addresses for one item.
type SelectOptions struct {
	Interface string
	Scope     string // "public" or "any"
	Prefix    *netip.Prefix
	Index     int
}

// Selection is the chosen base address.
type Selection struct {
	Address Address
}

// ErrNoEligibleAddress is returned when no address satisfies the selector.
var ErrNoEligibleAddress = errors.New("no eligible address")

// ErrEntryExists means AddEntry encountered a duplicate. It does not confirm
// that the existing entry is enabled or satisfies the requested target.
var ErrEntryExists = errors.New("address-list entry already exists")

// NewRESTClient builds the production REST client (implemented by the ROS
// workstream).
func NewRESTClient(cfg config.RouterOSConfig) (Client, error) {
	return newRESTClient(cfg)
}

// Select chooses one base address from addrs (implemented by the ROS
// workstream).
func Select(addrs []Address, opts SelectOptions) (Selection, error) {
	return selectAddress(addrs, opts)
}
