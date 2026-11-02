// Package dns defines the DNS provider contract shared by the service and the
// provider implementations.
package dns

import (
	"context"
	"errors"
)

// Record is one A/AAAA record binding. Type is derived from the item family by
// the service and is never read from config.
type Record struct {
	Domain string
	RR     string
	Type   string
	Value  string
	TTL    int
	Line   string
}

// FQDN returns the fully qualified record name.
func (r Record) FQDN() string {
	if r.RR == "@" {
		return r.Domain
	}
	return r.RR + "." + r.Domain
}

// EnsureResult reports what Ensure did.
type EnsureResult struct {
	RecordID string
	Changed  bool
}

// Provider guarantees that the requested record value exists.
type Provider interface {
	Ensure(ctx context.Context, r Record, recordIDHint string) (EnsureResult, error)
}

// ErrAmbiguousRecord is returned when several records match the binding and
// none can be identified through the persisted record ID.
var ErrAmbiguousRecord = errors.New("multiple matching DNS records; refusing to modify")
