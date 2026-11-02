package service

import (
	"context"
)

// runOnce polls once and forces each eligible, stable item synchronously. Force
// bypasses target caches, not source eligibility or StableFor. It starts no
// workers. Only poll/setup failures are returned; target errors are in Snapshot.
//
// This is a package-internal synchronous driver for tests; it is not exposed by
// the CLI or the HTTP API.
func (s *Service) runOnce(ctx context.Context) (Snapshot, error) {
	if err := s.begin(); err != nil {
		return s.Snapshot(), err
	}
	defer s.end()
	pollErr := s.poller.pollAll(ctx)
	for _, it := range s.items {
		it.takeRequest() // consume any startup force that the poll satisfied
		it.reconcile(ctx, true)
	}
	return s.Snapshot(), pollErr
}
