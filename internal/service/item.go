package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/eui64"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/state"
)

type item struct {
	service  *Service
	cfg      config.ItemConfig
	key      string
	wake     chan struct{}
	listWake chan struct{}

	// opMu serializes normal reconciliations. List-only renewal uses listMu
	// without opMu, so DNS cannot delay it. Network I/O never holds mu.
	opMu      sync.Mutex
	listMu    sync.Mutex
	persistMu sync.Mutex
	mu        sync.RWMutex
	status    ItemStatus
	// nextDomain is protected by opMu. A target consuming the shared deadline
	// must not keep later bindings from getting a turn on the next retry.
	nextDomain int

	sourceReady    bool
	sourceValid    bool
	sourcePhase    string
	sourceError    string
	stateError     string
	candidate      string
	valueSince     time.Time
	desired        string
	revision       uint64 // advances even if a value changes away and back in flight
	domainValues   []string
	domainTTLs     []int // last confirmed TTL, distinct from the configured/status TTL
	listValue      string
	listGeneration uint64    // advances after each successful list reconciliation
	listRenewAt    time.Time // monotonic scheduling clock; never restored from disk
	listBackoff    time.Duration
	listStale      bool
	pendingForce   bool
}

func newItem(s *Service, c config.ItemConfig) *item {
	it := &item{
		service: s, cfg: c, key: sourceKey(c), wake: make(chan struct{}, 1), listWake: make(chan struct{}, 1),
		status: initialStatus(c), sourcePhase: "waiting", pendingForce: s.runtime.SyncOnStart,
		domainValues: make([]string, len(c.Domains)),
		domainTTLs:   make([]int, len(c.Domains)),
	}
	if cached, ok := s.store.Get(c.Name); ok {
		if cached.SourceKey == it.key {
			it.status.SourceAddress = cached.SourceAddress
			it.status.RecordValue = cached.RecordValue
			it.status.SourceObservedAt = cached.SourceObservedAt
			it.status.LastSyncAt = cached.LastSyncAt
			it.desired = cached.DesiredValue
		}
		for i, d := range c.Domains {
			if previous, ok := cached.Domains[bindingKey(c.Source.Family, d)]; ok {
				status := &it.status.Domains[i]
				status.Value, status.RecordID, status.UpdatedAt = previous.Value, previous.RecordID, previous.UpdatedAt
				it.domainTTLs[i] = previous.TTL
				status.LastError = s.redactor.Replace(previous.LastError)
				// Startup verification must finish before list-only renewal can
				// report the item synced using restored DNS values.
				if !s.runtime.SyncOnStart && status.LastError == "" && previous.TTL == d.TTL {
					it.domainValues[i] = previous.Value
				}
			}
		}
		if old, target := cached.AddressList, c.AddressList; old != nil && target != nil &&
			old.Family == c.Source.Family && old.List == target.List && old.Comment == target.Comment && old.Timeout == target.Timeout {
			it.status.AddressList.Entries = append([]string{}, old.Entries...)
			it.status.AddressList.UpdatedAt = old.UpdatedAt
			it.status.AddressList.LastError = s.redactor.Replace(old.LastError)
			// The cache records configured lifetime, not the observed lifetime
			// of a foreign entry. Every restored list needs a fresh read.
			it.listStale = true
			it.listRenewAt = time.Now()
			if it.status.AddressList.LastError == "" {
				it.listValue = old.Value
			}
		}
	}
	it.refreshLocked()
	return it
}

func initialStatus(c config.ItemConfig) ItemStatus {
	status := ItemStatus{
		Name: c.Name, Disabled: c.Disabled, Family: c.Source.Family, Interface: c.Source.Interface,
		EUI64MAC: c.Source.EUI64MAC, Phase: "waiting", Domains: make([]DomainStatus, len(c.Domains)),
	}
	if len(c.Source.EUI64MACAddr) != 0 {
		status.EUI64MAC = c.Source.EUI64MACAddr.String()
	}
	if c.Disabled {
		status.Phase = "disabled"
	}
	for i, d := range c.Domains {
		status.Domains[i] = DomainStatus{Provider: d.Provider, FQDN: d.FQDN(), Type: config.RecordType(c.Source.Family), TTL: d.TTL, Line: d.Line}
	}
	if c.AddressList != nil {
		status.AddressList = &AddressListStatus{List: c.AddressList.List, Entries: []string{}}
	}
	return status
}

func sourceKey(c config.ItemConfig) string {
	key, _ := json.Marshal([]any{c.Source.Interface, c.Source.Family, c.Source.EUI64MACAddr.String(),
		c.Source.EUI64MAC, c.Source.Select.Scope, c.Source.Select.Prefix, c.Source.Select.PrefixNet, c.Source.Select.Index})
	return string(key)
}

func bindingKey(family string, d config.DomainConfig) string {
	// TTL changes require a new Ensure, but must retain the record ID hint for
	// providers that need it to disambiguate multiple matching records.
	key, _ := json.Marshal([]any{d.Provider, d.Domain, d.RR, config.RecordType(family), d.Line})
	return string(key)
}

// observe never passes a stale family snapshot to the selector after a failed
// poll. A gap resets an uncommitted candidate's stability window, not desired.
func (it *item) observe(addrs []ros.Address, pollErr error, observed, nextPoll time.Time) {
	it.observeContext(context.Background(), addrs, pollErr, observed, nextPoll)
}

func (it *item) observeContext(ctx context.Context, addrs []ros.Address, pollErr error, observed, nextPoll time.Time) {
	defer it.wakeListRenewal()
	it.mu.Lock()
	previousValid, previousError := it.sourceValid, it.sourceError
	previousDesired, previousPhase := it.desired, it.sourcePhase
	wasUsable := it.usableLocked()
	it.status.NextPollAt = nextPoll
	if pollErr != nil {
		it.sourceValid = false
		it.sourcePhase, it.sourceError = "degraded", it.service.errorText(pollErr)
		it.candidate, it.valueSince = "", time.Time{}
		it.refreshLocked()
		it.mu.Unlock()
		it.persist(ctx)
		slog.DebugContext(ctx, "source poll failed for item", "item", it.cfg.Name, "family", it.cfg.Source.Family,
			"error", it.service.errorText(pollErr))
		return
	}
	it.sourceReady = true
	it.mu.Unlock()

	selected, err := it.service.selector(append([]ros.Address(nil), addrs...), ros.SelectOptions{
		Interface: it.cfg.Source.Interface, Scope: it.cfg.Source.Select.Scope,
		Prefix: it.cfg.Source.Select.PrefixNet, Index: it.cfg.Source.Select.Index,
	})
	phase := "waiting"
	value := ""
	if err == nil && !selected.Address.Prefix.IsValid() {
		err = ros.ErrNoEligibleAddress
	}
	selectedBase := err == nil
	if err == nil {
		base := selected.Address.Prefix.Addr()
		if it.cfg.Source.EUI64MAC != "" || len(it.cfg.Source.EUI64MACAddr) != 0 {
			derived, deriveErr := eui64.Derive(base, it.cfg.Source.EUI64MACAddr)
			if deriveErr != nil {
				err, phase = fmt.Errorf("derive EUI-64 address: %w", deriveErr), "degraded"
			} else {
				value = derived.String()
			}
		} else if (it.cfg.Source.Family == config.FamilyIPv4 && !base.Is4()) ||
			(it.cfg.Source.Family == config.FamilyIPv6 && (!base.Is6() || base.Is4In6())) {
			err = fmt.Errorf("selected address does not match family %s", it.cfg.Source.Family)
		} else {
			value = base.String()
		}
	}
	it.mu.Lock()
	if selectedBase {
		it.status.SourceAddress = selected.Address.Prefix.String()
		it.status.SourceObservedAt = observed
	}
	if err != nil {
		it.sourceValid = false
		it.sourcePhase, it.sourceError = phase, it.service.errorText(err)
		it.candidate, it.valueSince = "", time.Time{}
		it.refreshLocked()
		it.mu.Unlock()
		it.persist(ctx)
		args := []any{"item", it.cfg.Name, "family", it.cfg.Source.Family, "interface", it.cfg.Source.Interface,
			"phase", phase, "error", it.service.errorText(err), "previousValid", previousValid, "previousPhase", previousPhase}
		if previousError == "" {
			slog.WarnContext(ctx, "source observation degraded", args...)
		} else {
			slog.DebugContext(ctx, "source observation still degraded", args...)
		}
		return
	}
	it.sourceValid, it.sourcePhase, it.sourceError = true, "", ""
	it.status.RecordValue = value
	if it.candidate != value {
		it.candidate, it.valueSince = value, observed
	}
	changed := false
	if value != it.desired && observed.Sub(it.valueSince) >= it.service.runtime.StableFor.Duration {
		it.desired, changed = value, true
		it.revision++
	}
	usable := it.usableLocked()
	if usable && it.listStale && it.listRenewAt.IsZero() {
		// Let the dedicated worker read the list even if DNS is still in
		// flight. The deferred wake also resumes overdue work after recovery.
		it.listRenewAt = time.Now()
	}
	trigger := usable && (changed || it.pendingForce || (!wasUsable && !it.syncedLocked()))
	reason := "recovery"
	if changed {
		reason = "source-changed"
	} else if it.pendingForce {
		reason = "startup-force"
	}
	sourceAddress, candidate, desired := it.status.SourceAddress, it.candidate, it.desired
	it.refreshLocked()
	it.mu.Unlock()
	it.persist(ctx)
	if trigger {
		it.enqueue(false)
	}
	if previousError != "" {
		slog.InfoContext(ctx, "source observation recovered", "item", it.cfg.Name, "family", it.cfg.Source.Family,
			"interface", it.cfg.Source.Interface, "value", value, "sourceAddress", sourceAddress)
	}
	slog.DebugContext(ctx, "source observed", "item", it.cfg.Name, "family", it.cfg.Source.Family,
		"interface", it.cfg.Source.Interface, "sourceAddress", sourceAddress, "candidate", candidate,
		"desired", desired, "changed", changed, "trigger", trigger)
	if changed {
		if previousDesired == "" {
			slog.InfoContext(ctx, "source value selected", "item", it.cfg.Name, "family", it.cfg.Source.Family,
				"interface", it.cfg.Source.Interface, "value", desired, "sourceAddress", sourceAddress)
		} else {
			slog.InfoContext(ctx, "source value changed", "item", it.cfg.Name, "family", it.cfg.Source.Family,
				"interface", it.cfg.Source.Interface, "from", previousDesired, "to", desired, "sourceAddress", sourceAddress)
		}
	}
	if trigger {
		slog.DebugContext(ctx, "reconciliation queued", "item", it.cfg.Name, "reason", reason)
	}
}

func (it *item) enqueue(force bool) {
	it.mu.Lock()
	it.pendingForce = it.pendingForce || force
	// Updating the flag and enqueuing under one lock prevents a consumer from
	// consuming the flag between these operations and producing redundant work.
	select {
	case it.wake <- struct{}{}:
	default:
	}
	it.mu.Unlock()
}

func (it *item) takeRequest() (force, usable bool) {
	it.mu.Lock()
	defer it.mu.Unlock()
	usable = it.usableLocked()
	if usable {
		force, it.pendingForce = it.pendingForce, false
		// A request can arrive while a previous reconciliation is running.
		// Draining under mu coalesces it with the force flag just consumed.
		select {
		case <-it.wake:
		default:
		}
	}
	return force, usable
}

func (it *item) run(ctx context.Context) {
	var verify *time.Ticker
	var verifyC <-chan time.Time
	if interval := it.service.runtime.VerifyInterval.Duration; interval > 0 {
		verify = time.NewTicker(interval)
		verifyC = verify.C
		defer verify.Stop()
	}
	var retry *time.Timer
	var retryC <-chan time.Time
	var delay time.Duration
	var retryRevision uint64
	stopRetry := func() {
		if retry != nil {
			retry.Stop()
		}
		retryC = nil
	}
	defer stopRetry()
	for {
		select {
		case <-ctx.Done():
			return
		case <-verifyC:
			it.enqueue(true)
		case <-retryC:
			retryC = nil
			it.enqueue(false)
		case <-it.wake:
			force, usable := it.takeRequest()
			if !usable || ctx.Err() != nil {
				continue
			}
			it.mu.RLock()
			revision := it.revision
			it.mu.RUnlock()
			if force || revision != retryRevision {
				delay = 0
			}
			stopRetry()
			result := it.reconcile(ctx, force)
			if ctx.Err() != nil {
				return
			}
			it.mu.RLock()
			current := it.revision
			it.mu.RUnlock()
			if result.err != nil && result.revision == current {
				delay = growBackoff(delay, 5*time.Second, it.service.runtime.MaxBackoff.Duration)
				retryRevision = current
				wait := jitterBackoff(delay, it.service.runtime.MaxBackoff.Duration)
				retry = time.NewTimer(wait)
				retryC = retry.C
				slog.WarnContext(ctx, "reconciliation will retry", "item", it.cfg.Name, "after", wait, "error", result.err)
			} else {
				delay, retryRevision = 0, current
			}
		}
	}
}

func growBackoff(previous, initial, maximum time.Duration) time.Duration {
	if previous <= 0 {
		return min(initial, maximum)
	}
	if previous >= maximum/2 {
		return maximum
	}
	return min(previous*2, maximum)
}

func jitterBackoff(delay, maximum time.Duration) time.Duration {
	// Positive jitter of up to 10%, always capped at the configured maximum.
	return delay + time.Duration(float64(min(delay/10, maximum-delay))*rand.Float64())
}

func (it *item) usableLocked() bool {
	return it.sourceReady && it.sourceValid && it.desired != "" && it.candidate == it.desired
}

func (it *item) syncedLocked() bool {
	for i, value := range it.domainValues {
		if value != it.desired || it.status.Domains[i].LastError != "" {
			return false
		}
	}
	return it.status.AddressList == nil || (!it.listStale && it.listValue == it.desired && it.status.AddressList.LastError == "")
}

func (it *item) refreshLocked() {
	var problems []string
	if it.sourceError != "" {
		problems = append(problems, it.sourceError)
	}
	for _, d := range it.status.Domains {
		if d.LastError != "" {
			problems = append(problems, d.Provider+"/"+d.FQDN+": "+d.LastError)
		}
	}
	if list := it.status.AddressList; list != nil && list.LastError != "" {
		problems = append(problems, "address list "+list.List+": "+list.LastError)
	}
	if it.stateError != "" {
		problems = append(problems, "state: "+it.stateError)
	}
	it.status.LastError = strings.Join(problems, "; ")
	switch {
	case it.sourcePhase == "degraded":
		it.status.Phase = "degraded"
	case !it.usableLocked():
		it.status.Phase = "waiting"
	case len(problems) > 0:
		it.status.Phase = "degraded"
	case it.syncedLocked():
		it.status.Phase = "synced"
	default:
		it.status.Phase = "waiting"
	}
}

func (it *item) snapshot() ItemStatus {
	it.mu.RLock()
	defer it.mu.RUnlock()
	return cloneStatus(it.status)
}

func cloneStatus(status ItemStatus) ItemStatus {
	status.Domains = append([]DomainStatus{}, status.Domains...)
	if status.AddressList != nil {
		list := *status.AddressList
		list.Entries = append([]string{}, list.Entries...)
		status.AddressList = &list
	}
	return status
}

func (it *item) persist(ctx context.Context) error {
	// Lock before taking the snapshot so an earlier observation can never
	// overwrite a newer reconciliation's cache from another goroutine.
	it.persistMu.Lock()
	defer it.persistMu.Unlock()
	it.mu.RLock()
	cached := state.Item{
		SourceKey: it.key, SourceAddress: it.status.SourceAddress, RecordValue: it.status.RecordValue,
		SourceObservedAt: it.status.SourceObservedAt,
		DesiredValue:     it.desired, LastSyncAt: it.status.LastSyncAt,
		Domains: make(map[string]state.Domain, len(it.cfg.Domains)),
	}
	for i, d := range it.cfg.Domains {
		status := it.status.Domains[i]
		cached.Domains[bindingKey(it.cfg.Source.Family, d)] = state.Domain{
			RecordID: status.RecordID, Value: status.Value, TTL: it.domainTTLs[i], UpdatedAt: status.UpdatedAt, LastError: status.LastError,
		}
	}
	if c := it.cfg.AddressList; c != nil {
		status := it.status.AddressList
		cached.AddressList = &state.AddressList{
			Family: it.cfg.Source.Family, List: c.List, Comment: c.Comment, Timeout: c.Timeout,
			Value: it.listValue, Entries: append([]string{}, status.Entries...), UpdatedAt: status.UpdatedAt, LastError: status.LastError,
		}
	}
	it.mu.RUnlock()
	err := it.service.store.Put(it.cfg.Name, cached)
	it.mu.Lock()
	it.stateError = it.service.errorText(err)
	it.refreshLocked()
	it.mu.Unlock()
	if err != nil {
		logging.LogContext(ctx, slog.LevelWarn, "could not persist item state", "item", it.cfg.Name, "error", it.service.errorText(err))
	}
	return err
}
