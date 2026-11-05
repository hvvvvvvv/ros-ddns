package service

import (
	"context"
	"log/slog"
	"time"

	"ros-ddns/internal/logging"
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
			it.mu.RLock()
			name, list, desired := it.cfg.Name, it.cfg.AddressList.List, it.desired
			it.mu.RUnlock()
			slog.DebugContext(ctx, "address-list renewal timer fired", "item", name, "list", list, "address", desired)
			it.renewList(ctx)
		}
	}
}

func (it *item) renewList(ctx context.Context) {
	ctx, flushLogs := logging.WithDeferredLogs(ctx)
	defer flushLogs() // listMu is released before any buffered log reaches slog.
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
	logging.LogContext(ctx, slog.LevelInfo, "address-list renewal started", "item", it.cfg.Name,
		"list", it.cfg.AddressList.List, "address", desired, "timeout", listTimeout(it.cfg.AddressList.Timeout))

	listCtx, cancel := context.WithTimeout(ctx, it.service.runtime.ReconcileTimeout.Duration)
	defer cancel()
	err := it.reconcileList(listCtx, desired)
	it.mu.Lock()
	if err == nil && revision == it.revision && it.usableLocked() && it.syncedLocked() {
		it.status.LastSyncAt = it.service.now()
	}
	it.refreshLocked()
	it.mu.Unlock()
	it.persist(ctx)
	args := []any{"item", it.cfg.Name, "list", it.cfg.AddressList.List}
	if err != nil {
		args = append(args, "error", it.service.errorText(err))
	}
	logging.LogContext(ctx, slog.LevelDebug, "address-list renewal finished", args...)
}
