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
	"ros-ddns/internal/ros"
)

type reconcileResult struct {
	desired  string
	revision uint64
	err      error
}

// reconcile is intentionally callable directly by in-package tests. Normal
// source/retry work passes force=false; force checks every configured target.
func (it *item) reconcile(ctx context.Context, force bool) reconcileResult {
	it.opMu.Lock()
	defer it.opMu.Unlock()
	it.mu.Lock()
	if !it.usableLocked() {
		it.pendingForce = it.pendingForce || force
		it.mu.Unlock()
		return reconcileResult{}
	}
	desired, revision, listGeneration := it.desired, it.revision, it.listGeneration
	it.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, it.service.runtime.ReconcileTimeout.Duration)
	defer cancel()
	var failures []error
	for i, binding := range it.cfg.Domains {
		it.mu.RLock()
		skip := !force && it.domainValues[i] == desired
		hint := it.status.Domains[i].RecordID
		it.mu.RUnlock()
		if skip {
			continue
		}
		record := dns.Record{
			Domain: binding.Domain, RR: binding.RR, Type: ros.Family(it.cfg.Source.Family).RecordType(),
			Value: desired, TTL: binding.TTL, Line: binding.Line,
		}
		var result dns.EnsureResult
		err := ctx.Err()
		if err == nil {
			result, err = it.service.providers[binding.Provider].Ensure(ctx, record, hint)
		}
		it.mu.Lock()
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
			slog.Warn("DNS reconciliation failed", "item", it.cfg.Name, "binding", i,
				"provider", binding.Provider, "fqdn", binding.FQDN(), "error", it.service.errorText(err))
		} else {
			slog.Info("DNS record reconciled", "item", it.cfg.Name, "binding", i,
				"provider", binding.Provider, "fqdn", binding.FQDN(), "value", desired, "changed", result.Changed)
		}
	}
	if it.cfg.AddressList != nil {
		it.listMu.Lock()
		it.mu.RLock()
		// A renewal may have completed while DNS was in flight. Recheck the
		// cache under listMu, and never overwrite a newer source revision.
		// A successful concurrent renewal also satisfies a forced list check.
		verified := !it.listStale && it.listValue == desired && (!force || it.listGeneration != listGeneration)
		skip := !it.usableLocked() || revision != it.revision || verified
		it.mu.RUnlock()
		if !skip {
			if err := it.reconcileList(ctx, desired); err != nil {
				failures = append(failures, fmt.Errorf("address list %s: %s", it.cfg.AddressList.List, it.service.errorText(err)))
			}
		}
		it.listMu.Unlock()
	}
	it.mu.Lock()
	if len(failures) == 0 && desired == it.desired && it.usableLocked() && it.syncedLocked() {
		it.status.LastSyncAt = it.service.now()
	}
	it.refreshLocked()
	it.mu.Unlock()
	if err := it.persist(); err != nil {
		failures = append(failures, fmt.Errorf("persist state: %s", it.service.errorText(err)))
	}
	return reconcileResult{desired: desired, revision: revision, err: errors.Join(failures...)}
}

// reconcileList requires listMu; both normal reconciliation and list-only
// renewal hold it across the read, mutations and result publication.
func (it *item) reconcileList(ctx context.Context, desired string) error {
	c := it.cfg.AddressList
	family := ros.Family(it.cfg.Source.Family)
	var entries []ros.ListEntry
	err := ctx.Err()
	if err == nil {
		entries, err = it.service.client.ListEntries(ctx, family, c.List)
	}
	if err != nil {
		it.setListResult(desired, nil, false, err, 0)
		return err
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
	it.mu.RLock()
	renew := it.listStale && c.Timeout != ""
	it.mu.RUnlock()
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
			continue
		}
		if matches && !entry.Disabled && entry.Dynamic == (c.Timeout != "") && sameLifetime(c.Timeout, entry.Timeout) && !renew {
			present = true
			if entry.Dynamic {
				renewAfter = earlierRenewal(renewAfter, dynamicListRecheckDelay(entry.Timeout, it.service.runtime.VerifyInterval.Duration))
			}
			remaining = append(remaining, entry)
			continue
		}
		err := ctx.Err()
		if err == nil && entry.ID == "" {
			err = errors.New("managed stale address-list entry has no ID")
		}
		if err == nil {
			err = it.service.client.RemoveEntry(ctx, family, entry.ID)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("remove entry %s: %s", entry.ID, it.service.errorText(err)))
			remaining = append(remaining, entry)
		}
	}
	// Complete stale removals before adding. A failed removal is retried, rather
	// than adding a second managed value while the old one still exists.
	if !present && len(failures) == 0 {
		entry := ros.ListEntry{List: c.List, Address: desired, Comment: c.Comment, Timeout: c.Timeout}
		err := ctx.Err()
		if err == nil {
			err = it.service.client.AddEntry(ctx, family, entry)
		}
		if err != nil {
			// ErrEntryExists is a failure too: only a subsequent list read
			// can establish whether the conflicting entry is a valid target.
			failures = append(failures, fmt.Errorf("add entry: %s", it.service.errorText(err)))
		} else {
			remaining = append(remaining, entry)
			if c.Timeout != "" {
				renewAfter = earlierRenewal(renewAfter, dynamicListRecheckDelay(c.Timeout, it.service.runtime.VerifyInterval.Duration))
			}
		}
	}
	err = errors.Join(failures...)
	it.setListResult(desired, visibleEntries(remaining, c.Comment, desired), true, err, renewAfter)
	return err
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

func (it *item) setListResult(desired string, entries []string, observed bool, err error, renewAfter time.Duration) {
	it.mu.Lock()
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
	it.mu.Unlock()
	it.wakeListRenewal()
	if err != nil {
		slog.Warn("address-list reconciliation failed", "item", it.cfg.Name, "list", it.cfg.AddressList.List, "error", it.service.errorText(err))
	}
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
