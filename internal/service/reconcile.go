package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"time"

	"ros-ddns/internal/dns"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/ros"
)

type reconcileResult struct {
	desired  string
	revision uint64
	err      error
}

var errSourceChanged = errors.New("source changed or became unavailable during reconciliation")

func (it *item) sourceCurrentLocked(desired string, revision uint64) bool {
	return it.usableLocked() && it.desired == desired && it.revision == revision
}

func (it *item) sourceCurrent(desired string, revision uint64) bool {
	it.mu.RLock()
	defer it.mu.RUnlock()
	return it.sourceCurrentLocked(desired, revision)
}

// reconcile is intentionally callable directly by in-package tests. Normal
// source/retry work passes force=false; force checks every configured target.
func (it *item) reconcile(ctx context.Context, force bool) reconcileResult {
	ctx, flushLogs := logging.WithDeferredLogs(ctx)
	defer flushLogs() // Registered before Unlock: all output happens outside item locks.
	it.opMu.Lock()
	defer it.opMu.Unlock()
	it.mu.Lock()
	if !it.usableLocked() {
		it.pendingForce = it.pendingForce || force
		it.mu.Unlock()
		return reconcileResult{}
	}
	desired, revision, listGeneration := it.desired, it.revision, it.listGeneration
	if force {
		// Invalidate once, before any I/O. If this round runs out of time,
		// ordinary retries must still verify every unvisited forced target.
		// Displayed confirmations remain intact until a fresh result arrives.
		clear(it.domainValues)
		it.refreshLocked()
	}
	it.mu.Unlock()
	started := time.Now()
	args := []any{"item", it.cfg.Name, "value", desired, "force", force, "domains", len(it.cfg.Domains)}
	if it.cfg.AddressList != nil {
		args = append(args, "list", it.cfg.AddressList.List)
	}
	logging.LogContext(ctx, slog.LevelDebug, "reconciliation started", args...)
	ctx, cancel := context.WithTimeout(ctx, it.service.runtime.ReconcileTimeout.Duration)
	defer cancel()
	var failures []error
	stale := false
	start := it.nextDomain
	for offset := range len(it.cfg.Domains) {
		i := (start + offset) % len(it.cfg.Domains)
		binding := it.cfg.Domains[i]
		it.mu.RLock()
		current := it.sourceCurrentLocked(desired, revision)
		skip := it.domainValues[i] == desired
		hint := it.status.Domains[i].RecordID
		it.mu.RUnlock()
		if !current {
			stale = true
			break
		}
		if skip {
			logging.LogContext(ctx, slog.LevelDebug, "DNS record already confirmed; skipping", "item", it.cfg.Name,
				"binding", i, "provider", binding.Provider, "fqdn", binding.FQDN(), "value", desired)
			continue
		}
		record := dns.Record{
			Domain: binding.Domain, RR: binding.RR, Type: ros.Family(it.cfg.Source.Family).RecordType(),
			Value: desired, TTL: binding.TTL, Line: binding.Line,
		}
		var result dns.EnsureResult
		ensureStarted := time.Now()
		err := ctx.Err()
		if err == nil {
			// Only an actual attempt advances the cursor. The remaining
			// targets still receive the shared timeout below, without calls.
			it.nextDomain = (i + 1) % len(it.cfg.Domains)
			logging.LogContext(ctx, slog.LevelDebug, "reconciling DNS record", "item", it.cfg.Name, "binding", i,
				"provider", binding.Provider, "fqdn", binding.FQDN(), "type", record.Type, "value", desired,
				"ttl", binding.TTL, "line", binding.Line, "recordIdHint", hint != "")
			result, err = it.service.providers[binding.Provider].Ensure(ctx, record, hint)
		}
		duration := time.Since(ensureStarted)
		it.mu.Lock()
		if !it.sourceCurrentLocked(desired, revision) {
			// The in-flight call may have changed the remote record. Even if
			// the source returned to a previously confirmed value, recheck it.
			it.domainValues[i] = ""
			// Persist the invalidation alongside the last display value, so a
			// restart with SyncOnStart=false cannot restore it as a skip cache.
			it.status.Domains[i].LastError = it.service.errorText(errSourceChanged)
			it.refreshLocked()
			it.mu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Errorf("%s/%s: %s", binding.Provider, binding.FQDN(), it.service.errorText(err)))
			}
			stale = true
			break
		}
		status := &it.status.Domains[i]
		if err != nil {
			// The previous confirmed value remains visible, but a failed forced
			// verification invalidates the skip cache even if desired is unchanged.
			it.domainValues[i] = ""
			status.LastError = it.service.errorText(err)
			failures = append(failures, fmt.Errorf("%s/%s: %s", binding.Provider, binding.FQDN(), status.LastError))
		} else {
			it.domainValues[i], status.Value, status.RecordID = desired, desired, result.RecordID
			it.domainTTLs[i] = binding.TTL
			// updatedAt is the last actual add/update, not a confirmation of an
			// unchanged record. Ensure reports Changed only after a write.
			if result.Changed {
				status.UpdatedAt = it.service.now()
			}
			status.LastError = ""
		}
		it.refreshLocked()
		it.mu.Unlock()
		if err != nil {
			logging.LogContext(ctx, slog.LevelWarn, "DNS reconciliation failed", "item", it.cfg.Name, "binding", i,
				"provider", binding.Provider, "fqdn", binding.FQDN(), "error", it.service.errorText(err), "duration", duration)
		} else {
			logging.LogContext(ctx, slog.LevelInfo, "DNS record reconciled", "item", it.cfg.Name, "binding", i,
				"provider", binding.Provider, "fqdn", binding.FQDN(), "value", desired, "changed", result.Changed, "duration", duration)
		}
	}
	if !stale && it.cfg.AddressList != nil {
		it.listMu.Lock()
		it.mu.RLock()
		// A renewal may have completed while DNS was in flight. Recheck the
		// cache under listMu, and never overwrite a newer source revision.
		// A successful concurrent renewal also satisfies a forced list check.
		verified := !it.listStale && it.listValue == desired && (!force || it.listGeneration != listGeneration)
		stale = !it.sourceCurrentLocked(desired, revision)
		skip := stale || verified
		it.mu.RUnlock()
		if !skip {
			if err := it.reconcileListRevision(ctx, desired, revision); err != nil {
				stale = errors.Is(err, errSourceChanged)
				failures = append(failures, fmt.Errorf("address list %s: %s", it.cfg.AddressList.List, it.service.errorText(err)))
			}
		}
		it.listMu.Unlock()
		if verified && !stale {
			logging.LogContext(ctx, slog.LevelDebug, "address-list already verified; skipping", "item", it.cfg.Name,
				"list", it.cfg.AddressList.List, "value", desired)
		}
	}
	it.mu.Lock()
	stale = stale || !it.sourceCurrentLocked(desired, revision)
	if stale {
		it.pendingForce = it.pendingForce || force
		failures = append(failures, errSourceChanged)
	} else if len(failures) == 0 && it.syncedLocked() {
		it.status.LastSyncAt = it.service.now()
	}
	it.refreshLocked()
	it.mu.Unlock()
	if err := it.persist(ctx); err != nil {
		failures = append(failures, fmt.Errorf("persist state: %s", it.service.errorText(err)))
	}
	err := errors.Join(failures...)
	args = []any{"item", it.cfg.Name, "duration", time.Since(started), "failures", len(failures)}
	if err != nil {
		args = append(args, "error", it.service.errorText(err))
	}
	logging.LogContext(ctx, slog.LevelDebug, "reconciliation finished", args...)
	return reconcileResult{desired: desired, revision: revision, err: err}
}

// reconcileList requires listMu; both normal reconciliation and list-only
// renewal hold it across the read, mutations and result publication. The caller
// owns the deferred log scope and flushes it after releasing its item locks.
func (it *item) reconcileList(ctx context.Context, desired string) error {
	it.mu.RLock()
	revision := it.revision
	current := it.sourceCurrentLocked(desired, revision)
	it.mu.RUnlock()
	if !current {
		return errSourceChanged
	}
	return it.reconcileListRevision(ctx, desired, revision)
}

// Normal reconciliation carries its captured source revision through DNS and
// list work. The list-only caller captures a fresh revision in reconcileList.
func (it *item) reconcileListRevision(ctx context.Context, desired string, revision uint64) error {
	started := time.Now()
	c := it.cfg.AddressList
	family := ros.Family(it.cfg.Source.Family)
	logging.LogContext(ctx, slog.LevelDebug, "address-list reconciliation started", "item", it.cfg.Name,
		"family", family, "list", c.List, "address", desired, "timeout", listTimeout(c.Timeout))
	logFinished := func(entries, failures int, renewAfter time.Duration) {
		logging.LogContext(ctx, slog.LevelDebug, "address-list reconciliation finished", "item", it.cfg.Name,
			"list", c.List, "entries", entries, "failures", failures, "renewAfter", renewAfter, "duration", time.Since(started))
	}
	if !it.sourceCurrent(desired, revision) {
		logFinished(0, 1, 0)
		return errSourceChanged
	}
	var entries []ros.ListEntry
	err := ctx.Err()
	if err == nil {
		entries, err = it.service.client.ListEntries(ctx, family, c.List)
	}
	if err == nil {
		logging.LogContext(ctx, slog.LevelDebug, "address-list fetched", "item", it.cfg.Name,
			"family", family, "list", c.List, "entries", len(entries))
	}
	it.mu.Lock()
	current := it.sourceCurrentLocked(desired, revision)
	// Renewal may have become due while the read was in flight. Capture its
	// request before invalidating the cache for the mutations that follow.
	renew := it.listStale && c.Timeout != ""
	// A stale result must not leave a former confirmation usable after an
	// in-flight removal or add. Only a current result can clear this flag.
	it.listStale = true
	it.refreshLocked()
	it.mu.Unlock()
	if !current {
		logFinished(len(entries), 1, 0)
		return errors.Join(err, errSourceChanged)
	}
	if err != nil {
		logFinished(len(entries), 1, 0)
		return it.setListResult(ctx, desired, revision, nil, false, err, 0)
	}
	// ListEntries is scoped by family/list; filter again to protect against a
	// transport returning a broader result. A missing List is allowed for clients
	// that omit the redundant field on an already-scoped request.
	scoped := make([]ros.ListEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.List == "" || entry.List == c.List {
			scoped = append(scoped, entry)
		}
	}
	entries = scoped
	remainingCount := len(entries)
	present := false
	var renewAfter time.Duration
	var failures []error
	remaining := make([]ros.ListEntry, 0, len(entries)+1)
	for _, entry := range entries {
		matches := addressMatches(entry.Address, desired)
		if entry.Comment != c.Comment {
			if matches {
				if entry.Disabled {
					failures = append(failures, fmt.Errorf("foreign disabled entry %q (comment %q) matches %s; enable or remove it manually", entry.ID, entry.Comment, desired))
				} else {
					present = true
					// A foreign dynamic entry may expire too. Recheck its remaining
					// lifetime without ever changing its timeout or deleting it.
					if entry.Dynamic {
						renewAfter = earlierRenewal(renewAfter, dynamicListRecheckDelay(entry.Timeout, it.service.runtime.VerifyInterval.Duration))
					}
				}
			}
			remaining = append(remaining, entry)
			logging.LogContext(ctx, slog.LevelDebug, "address-list entry retained", "item", it.cfg.Name, "list", c.List,
				"address", entry.Address, "entry_id", entry.ID, "reason", "foreign")
			continue
		}
		if matches && !entry.Disabled && entry.Dynamic == (c.Timeout != "") && sameLifetime(c.Timeout, entry.Timeout) && !renew {
			present = true
			if entry.Dynamic {
				renewAfter = earlierRenewal(renewAfter, dynamicListRecheckDelay(entry.Timeout, it.service.runtime.VerifyInterval.Duration))
			}
			remaining = append(remaining, entry)
			logging.LogContext(ctx, slog.LevelDebug, "address-list entry retained", "item", it.cfg.Name, "list", c.List,
				"address", entry.Address, "entry_id", entry.ID, "reason", "valid")
			continue
		}
		err := ctx.Err()
		if err == nil && entry.ID == "" {
			err = errors.New("managed stale address-list entry has no ID")
		}
		if err == nil {
			if !it.sourceCurrent(desired, revision) {
				logFinished(remainingCount, len(failures)+1, renewAfter)
				return errors.Join(append(failures, errSourceChanged)...)
			}
			err = it.service.client.RemoveEntry(ctx, family, entry.ID)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("remove entry %s: %s", entry.ID, it.service.errorText(err)))
			remaining = append(remaining, entry)
		} else {
			remainingCount--
			logging.LogContext(ctx, slog.LevelInfo, "address-list entry removed", "item", it.cfg.Name,
				"family", family, "list", c.List, "address", entry.Address, "entry_id", entry.ID,
				"reason", listRemovalReason(entry, desired, c.Timeout, renew))
		}
	}
	// Complete stale removals before adding. A failed removal is retried, rather
	// than adding a second managed value while the old one still exists.
	if !present && len(failures) == 0 {
		entry := ros.ListEntry{List: c.List, Address: desired, Comment: c.Comment, Timeout: c.Timeout}
		err := ctx.Err()
		if err == nil {
			if !it.sourceCurrent(desired, revision) {
				logFinished(remainingCount, 1, renewAfter)
				return errSourceChanged
			}
			err = it.service.client.AddEntry(ctx, family, entry)
		}
		if err != nil {
			// ErrEntryExists is a failure too: only a subsequent list read
			// can establish whether the conflicting entry is a valid target.
			failures = append(failures, fmt.Errorf("add entry: %s", it.service.errorText(err)))
		} else {
			remaining = append(remaining, entry)
			logging.LogContext(ctx, slog.LevelInfo, "address-list entry added", "item", it.cfg.Name, "family", family,
				"list", c.List, "address", desired, "timeout", listTimeout(c.Timeout), "comment", c.Comment)
			if c.Timeout != "" {
				renewAfter = earlierRenewal(renewAfter, dynamicListRecheckDelay(c.Timeout, it.service.runtime.VerifyInterval.Duration))
			}
		}
	}
	err = errors.Join(failures...)
	logFinished(len(remaining), len(failures), renewAfter)
	return it.setListResult(ctx, desired, revision, visibleEntries(remaining, c.Comment, desired), true, err, renewAfter)
}

func listTimeout(timeout string) string {
	if timeout == "" {
		return "static"
	}
	return timeout
}

func listRemovalReason(entry ros.ListEntry, desired, timeout string, renew bool) string {
	switch {
	case entry.Disabled:
		return "entry-disabled"
	case !addressMatches(entry.Address, desired):
		return "address-changed"
	case entry.Dynamic != (timeout != ""):
		return "static-dynamic-mismatch"
	case !sameLifetime(timeout, entry.Timeout):
		return "timeout-mismatch"
	case renew:
		return "renewal"
	default:
		return "stale"
	}
}

// visibleEntries shows managed entries plus any existing entry satisfying the
// target, even if another owner supplied it. Unrelated owners stay out of status.
func visibleEntries(entries []ros.ListEntry, comment, desired string) []string {
	set := make(map[string]bool)
	for _, entry := range entries {
		value := normalizedAddress(entry.Address)
		if entry.Comment == comment || addressMatches(entry.Address, desired) {
			set[value] = true
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (it *item) setListResult(ctx context.Context, desired string, revision uint64, entries []string, observed bool, err error, renewAfter time.Duration) error {
	it.mu.Lock()
	if !it.sourceCurrentLocked(desired, revision) {
		it.mu.Unlock()
		return errors.Join(err, errSourceChanged)
	}
	status := it.status.AddressList
	if observed {
		status.Entries = entries
	}
	status.LastError = it.service.errorText(err)
	renewing := !it.listRenewAt.IsZero() || it.cfg.AddressList.Timeout != ""
	it.listRenewAt = time.Time{}
	if err != nil {
		it.listValue = ""
		it.listStale = true
		if renewing {
			it.listBackoff = growBackoff(it.listBackoff, 5*time.Second, it.service.runtime.MaxBackoff.Duration)
			it.listRenewAt = time.Now().Add(jitterBackoff(it.listBackoff, it.service.runtime.MaxBackoff.Duration))
		}
	} else {
		it.listValue = desired
		it.listGeneration++
		it.listStale = false
		it.listBackoff = 0
		if renewAfter > 0 {
			// Scheduling uses the monotonic clock, independently of Deps.Now.
			it.listRenewAt = time.Now().Add(renewAfter)
		}
		status.UpdatedAt = it.service.now()
	}
	it.refreshLocked()
	entryCount := len(status.Entries)
	it.mu.Unlock()
	it.wakeListRenewal()
	if err != nil {
		logging.LogContext(ctx, slog.LevelWarn, "address-list reconciliation failed", "item", it.cfg.Name,
			"family", it.cfg.Source.Family, "list", it.cfg.AddressList.List, "error", it.service.errorText(err))
	} else {
		logging.LogContext(ctx, slog.LevelDebug, "address-list confirmed", "item", it.cfg.Name,
			"list", it.cfg.AddressList.List, "entries", entryCount, "renewAfter", renewAfter)
		if renewAfter > 0 {
			logging.LogContext(ctx, slog.LevelDebug, "address-list renewal scheduled", "item", it.cfg.Name,
				"list", it.cfg.AddressList.List, "after", renewAfter)
		}
	}
	return err
}

func normalizedAddress(value string) string {
	value = strings.TrimSpace(value)
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.Unmap().String()
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.String()
	}
	return value
}

func addressMatches(value, desired string) bool {
	want, err := netip.ParseAddr(desired)
	if err != nil {
		return false
	}
	value = strings.TrimSpace(value)
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.Unmap() == want.Unmap()
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Bits() == prefix.Addr().BitLen() && prefix.Addr().Unmap() == want.Unmap()
	}
	return false
}

func earlierRenewal(current, candidate time.Duration) time.Duration {
	if candidate > 0 && (current == 0 || candidate < current) {
		return candidate
	}
	return current
}
