package service

import (
	"context"
	"time"
)

func (it *item) wakeListRenewal() {
	select {
	case it.listWake <- struct{}{}:
	default:
	}
}

// runListRenewal owns its timer and never queues normal reconciliation. A
// successful static target has no deadline and never creates a timer here.
func (it *item) runListRenewal(ctx context.Context) {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		it.mu.Lock()
		due := it.listRenewAt
		if !due.IsZero() && !due.After(time.Now()) {
			it.listStale = true
			it.refreshLocked()
			if !it.usableLocked() {
				// Keep the deadline for source recovery, but do not spin on
				// an overdue timer while the source is unusable.
				due = time.Time{}
			}
		}
		it.mu.Unlock()

		var timerC <-chan time.Time
		if due.IsZero() {
			if timer != nil {
				timer.Stop()
			}
		} else {
			wait := max(0, time.Until(due))
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-it.listWake:
		case <-timerC:
			it.renewList(ctx)
		}
	}
}

func (it *item) renewList(ctx context.Context) {
	it.listMu.Lock()
	defer it.listMu.Unlock()
	it.mu.Lock()
	// A normal reconciliation may have refreshed the list while we waited
	// for listMu. Use the latest source and deadline after acquiring the lock.
	if ctx.Err() != nil || it.listRenewAt.IsZero() || it.listRenewAt.After(time.Now()) {
		it.mu.Unlock()
		return
	}
	it.listStale = true
	it.refreshLocked()
	if !it.usableLocked() {
		it.mu.Unlock()
		return
	}
	desired, revision := it.desired, it.revision
	it.mu.Unlock()

	listCtx, cancel := context.WithTimeout(ctx, it.service.runtime.ReconcileTimeout.Duration)
	defer cancel()
	err := it.reconcileList(listCtx, desired)
	it.mu.Lock()
	if err == nil && revision == it.revision && it.usableLocked() && it.syncedLocked() {
		it.status.LastSyncAt = it.service.now()
	}
	it.refreshLocked()
	it.mu.Unlock()
	it.persist()
}
