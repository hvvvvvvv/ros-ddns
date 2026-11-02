package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/ros"
)

func TestStableForAndInterruptedObservations(t *testing.T) {
	c := testConfig()
	c.Runtime.StableFor.Duration = time.Minute
	router, provider := newFakeROS(), &fakeProvider{}
	s, clock := testService(t, c, router, provider)
	snapshot, err := s.runOnce(context.Background())
	if err != nil || snapshot.Items[0].Phase != "waiting" || len(provider.recorded()) != 0 {
		t.Fatalf("force bypassed stability: snapshot=%#v err=%v", snapshot, err)
	}
	clock.advance(59 * time.Second)
	pollAndReconcile(t, s, true)
	if len(provider.recorded()) != 0 {
		t.Fatal("applied an unstable source")
	}
	clock.advance(time.Second)
	pollAndReconcile(t, s, false)
	if len(provider.recorded()) != 2 || s.items[0].desired != "203.0.113.10" {
		t.Fatal("stable source did not become desired")
	}
	router.setAddresses(ros.IPv4, address("203.0.113.11/24"))
	pollAndReconcile(t, s, false)
	clock.advance(45 * time.Second)
	router.setAddresses(ros.IPv4)
	pollAndReconcile(t, s, true)
	clock.advance(time.Minute)
	router.setAddresses(ros.IPv4, address("203.0.113.11/24"))
	pollAndReconcile(t, s, true)
	if len(provider.recorded()) != 2 || s.items[0].desired != "203.0.113.10" {
		t.Fatal("an observation gap committed the candidate or rewrote old desired")
	}
	clock.advance(time.Minute)
	pollAndReconcile(t, s, false)
	if len(provider.recorded()) != 4 || s.items[0].desired != "203.0.113.11" {
		t.Fatal("recovered stable candidate did not reconcile")
	}
}

func TestSharedFamilyPollsFailuresAndReadiness(t *testing.T) {
	c := testConfig()
	v4 := config.CloneItem(c.Items[0])
	v4.Name = "second-v4"
	v4.Domains, v4.AddressList = nil, nil
	v6 := config.CloneItem(c.Items[0])
	v6.Name, v6.Source.Family = "v6", "ipv6"
	c.Items = append(c.Items, v4, v6)
	router, provider := newFakeROS(), &fakeProvider{}
	router.pollErrors[ros.IPv6] = errors.New("router IPv6 read unavailable")
	s, clock := testService(t, c, router, provider)
	snapshot, err := s.runOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ipv6") || s.Ready() {
		t.Fatalf("failed poll error/readiness: %v %v", err, s.Ready())
	}
	if snapshot.Items[0].Phase != "synced" || snapshot.Items[1].Phase != "synced" || snapshot.Items[2].Phase != "degraded" {
		t.Fatalf("failure affected healthy family: %#v", snapshot.Items)
	}
	if router.pollCalls[ros.IPv4] != 1 || router.pollCalls[ros.IPv6] != 1 {
		t.Fatalf("not one poll per family: %v", router.pollCalls)
	}
	for range 8 {
		clock.advance(time.Hour)
		if err := s.poller.pollAll(context.Background()); err == nil {
			t.Fatal("poll failure lost")
		}
	}
	if s.poller.families[1].backoff != c.Runtime.MaxBackoff.Duration || s.poller.families[0].backoff != 0 {
		t.Fatalf("family backoff: %#v", s.poller.families)
	}
	snapshot = s.Snapshot()
	if snapshot.Items[0].NextPollAt != clock.now().Add(time.Hour) || snapshot.Items[2].NextPollAt != clock.now().Add(24*time.Hour) {
		t.Fatal("poll deadlines did not reflect per-family backoff")
	}
	delete(router.pollErrors, ros.IPv6)
	if err := s.poller.pollAll(context.Background()); err != nil || !s.Ready() {
		t.Fatalf("successful empty recovery: error=%v ready=%v", err, s.Ready())
	}
	if s.Snapshot().Items[2].Phase != "waiting" || s.poller.families[1].backoff != 0 {
		t.Fatal("empty successful poll should recover to waiting and clear backoff")
	}
	// A failed poll retains the last address snapshot without selecting it again.
	router.pollErrors[ros.IPv4] = errors.New("connection lost")
	before := len(provider.recorded())
	snapshot, err = s.runOnce(context.Background())
	if err == nil || snapshot.Items[0].Phase != "degraded" || len(provider.recorded()) != before || !s.Ready() || len(s.poller.families[0].addresses) != 1 {
		t.Fatal("failed poll used a cached source, discarded it, or reset historical readiness")
	}
}

func awaitCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true before timeout")
		case <-ticker.C:
		}
	}
}

func startService(t *testing.T, s *Service) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() { cancel() })
	return cancel, done
}

func stopService(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run cancellation returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestWorkerCoalescesForceTriggersAndStops(t *testing.T) {
	c := testConfig()
	router, provider := newFakeROS(), &fakeProvider{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	provider.ensureFunc = func(ctx context.Context, _ dns.Record) error {
		once.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		return ctx.Err()
	}
	s, _ := testService(t, c, router, provider)
	cancel, done := startService(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("startup worker did not reconcile")
	}
	for range 100 {
		if !s.TriggerItem("home") {
			t.Fatal("manual trigger rejected enabled item")
		}
	}
	if len(s.items[0].wake) != 1 {
		t.Fatal("trigger channel did not coalesce")
	}
	close(release)
	awaitCondition(t, func() bool {
		router.mu.Lock()
		reads := router.listCalls
		router.mu.Unlock()
		return len(provider.recorded()) == 4 && reads == 2 && s.Snapshot().Items[0].Phase == "synced"
	})
	if !s.Ready() {
		t.Fatal("running service not ready after successful poll")
	}
	stopService(t, cancel, done)
	if len(provider.recorded()) != 4 {
		t.Fatal("coalesced force was lost or replayed repeatedly")
	}
}

func TestWorkerRetriesOnlyFailedBinding(t *testing.T) {
	c := testConfig()
	c.Runtime.PollInterval.Duration = 5 * time.Millisecond
	c.Runtime.MaxBackoff.Duration = 20 * time.Millisecond
	router, provider := newFakeROS(), &fakeProvider{}
	provider.fail("home.example.com", 1)
	s, _ := testService(t, c, router, provider)
	cancel, done := startService(t, s)
	awaitCondition(t, func() bool { return s.Snapshot().Items[0].Phase == "synced" })
	stopService(t, cancel, done)
	calls := provider.recorded()
	if len(calls) != 3 || calls[2].record.FQDN() != "home.example.com" || router.listCalls != 1 {
		t.Fatalf("timer retry rewrote successful bindings: %v listCalls=%d", calls, router.listCalls)
	}
}

func TestNewDesiredDoesNotWaitForRetryBackoff(t *testing.T) {
	router, provider := newFakeROS(), &fakeProvider{}
	provider.fail("home.example.com", 1)
	s, _ := testService(t, testConfig(), router, provider)
	cancel, done := startService(t, s)
	awaitCondition(t, func() bool {
		status := s.Snapshot().Items[0]
		return status.Phase == "degraded" && status.Domains[1].Value != "" && status.AddressList.UpdatedAt != (time.Time{})
	})
	router.setAddresses(ros.IPv4, address("203.0.113.11/24"))
	s.poller.reset() // fresh source observation, without forcing target caches
	awaitCondition(t, func() bool {
		status := s.Snapshot().Items[0]
		return status.Phase == "synced" && status.RecordValue == "203.0.113.11"
	})
	stopService(t, cancel, done)
	calls := provider.recorded()
	if len(calls) != 4 || calls[2].record.Value != "203.0.113.11" || calls[3].record.Value != "203.0.113.11" {
		t.Fatalf("new desired did not supersede a pending retry: %v", calls)
	}
}

func TestPeriodicVerificationForcesAllTargets(t *testing.T) {
	c := testConfig()
	c.Runtime.VerifyInterval.Duration = 10 * time.Millisecond
	router, provider := newFakeROS(), &fakeProvider{}
	s, _ := testService(t, c, router, provider)
	cancel, done := startService(t, s)
	awaitCondition(t, func() bool {
		router.mu.Lock()
		reads := router.listCalls
		router.mu.Unlock()
		return len(provider.recorded()) >= 4 && reads >= 2
	})
	stopService(t, cancel, done)
	calls := provider.recorded()
	if calls[0].record.FQDN() != calls[2].record.FQDN() || calls[1].record.FQDN() != calls[3].record.FQDN() || router.pollCalls[ros.IPv4] != 1 {
		t.Fatal("verification did not force all targets independently of polling")
	}
}

func TestManualTriggerResetsPollBackoff(t *testing.T) {
	c := testConfig()
	router := newFakeROS()
	router.pollErrors[ros.IPv4] = errors.New("router offline")
	s, _ := testService(t, c, router, &fakeProvider{})
	cancel, done := startService(t, s)
	awaitCondition(t, func() bool { return s.Snapshot().Items[0].Phase == "degraded" })
	// The next scheduled poll is an hour away, so this must be the manual wake.
	router.mu.Lock()
	delete(router.pollErrors, ros.IPv4)
	router.mu.Unlock()
	if !s.TriggerItem("home") {
		t.Fatal("manual trigger failed")
	}
	awaitCondition(t, func() bool { return s.Snapshot().Items[0].Phase == "synced" })
	stopService(t, cancel, done)
	if router.pollCalls[ros.IPv4] != 2 || s.poller.families[0].backoff != 0 {
		t.Fatal("manual trigger did not reset polling backoff")
	}
}

func TestCancellationInterruptsBlockedPollAndRejectsOverlappingRuns(t *testing.T) {
	router := newFakeROS()
	entered := make(chan struct{})
	router.addressFunc = func(ctx context.Context, _ ros.Family) ([]ros.Address, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s, _ := testService(t, testConfig(), router, &fakeProvider{})
	cancel, done := startService(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("poll did not start")
	}
	if _, err := s.runOnce(context.Background()); err == nil {
		t.Fatal("overlapping runOnce was accepted")
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("overlapping Run was accepted")
	}
	stopService(t, cancel, done)
	if s.Ready() {
		t.Fatal("canceled poll counted as successful")
	}
}

func TestReconciliationDeadlineIsSharedByAllTargets(t *testing.T) {
	c := testConfig()
	c.Runtime.ReconcileTimeout.Duration = 15 * time.Millisecond
	provider := &fakeProvider{ensureFunc: func(ctx context.Context, _ dns.Record) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("missing reconcile deadline")
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	router := newFakeROS()
	s, _ := testService(t, c, router, provider)
	snapshot, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatalf("per-item timeout escaped runOnce: %v", err)
	}
	status := snapshot.Items[0]
	if status.Phase != "degraded" || !strings.Contains(status.Domains[0].LastError, "deadline exceeded") || !strings.Contains(status.Domains[1].LastError, "deadline exceeded") || !strings.Contains(status.AddressList.LastError, "deadline exceeded") {
		t.Fatalf("deadline not propagated across targets: %#v", status)
	}
	if len(provider.recorded()) != 1 || router.listCalls != 0 {
		t.Fatal("started additional external calls after the shared deadline")
	}
}

func TestStateFailureDoesNotDiscardSuccessfulTargets(t *testing.T) {
	c := testConfig()
	c.State.Path = filepath.Join(t.TempDir(), "state.json")
	router, provider := newFakeROS(), &fakeProvider{}
	s, _ := testService(t, c, router, provider)
	if err := os.Mkdir(c.State.Path, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.runOnce(context.Background())
	if err != nil || snapshot.Items[0].Phase != "degraded" || !strings.Contains(snapshot.Items[0].LastError, "state:") {
		t.Fatalf("state failure was lost or fatal: snapshot=%#v err=%v", snapshot, err)
	}
	if err := os.Remove(c.State.Path); err != nil {
		t.Fatal(err)
	}
	if result := s.items[0].reconcile(context.Background(), false); result.err != nil {
		t.Fatal(result.err)
	}
	if s.Snapshot().Items[0].Phase != "synced" || len(provider.recorded()) != 2 || router.listCalls != 1 {
		t.Fatal("state recovery rewrote targets or failed to clear degradation")
	}
}

func TestBackoffBounds(t *testing.T) {
	delay := time.Duration(0)
	for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second} {
		delay = growBackoff(delay, 5*time.Second, 30*time.Second)
		if delay != want {
			t.Fatalf("step %d: got %v want %v", i, delay, want)
		}
		for range 100 {
			if jittered := jitterBackoff(delay, 30*time.Second); jittered < delay || jittered > min(delay+delay/10, 30*time.Second) {
				t.Fatalf("jitter out of bounds: %v", jittered)
			}
		}
	}
}

func TestInFlightResultDoesNotResetNewDesiredRevision(t *testing.T) {
	c := testConfig()
	c.Runtime.ReconcileTimeout.Duration = 10 * time.Second
	router, provider := newFakeROS(), &fakeProvider{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	provider.ensureFunc = func(ctx context.Context, _ dns.Record) error {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			select {
			case <-release:
				return errors.New("old attempt failed")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	s, _ := testService(t, c, router, provider)
	if err := s.poller.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	it := s.items[0]
	it.takeRequest()
	firstRevision := it.revision
	done := make(chan reconcileResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- it.reconcile(ctx, false) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("reconciliation did not start")
	}
	for _, value := range []string{"203.0.113.11/24", "203.0.113.10/24"} {
		router.setAddresses(ros.IPv4, address(value))
		if err := s.poller.pollAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	result := <-done
	if result.err == nil || result.desired != it.desired || result.revision != firstRevision || result.revision == it.revision {
		t.Fatalf("old result confused with latest desired revision: result=%#v revision=%d", result, it.revision)
	}
	if retry := it.reconcile(context.Background(), false); retry.err != nil {
		t.Fatal(retry.err)
	}
	if len(provider.recorded()) != 3 || s.Snapshot().Items[0].Phase != "synced" {
		t.Fatal("new desired revision failed to preserve partial successes")
	}
}

func TestReusedServicePollsAgainBeforeActing(t *testing.T) {
	router, provider := newFakeROS(), &fakeProvider{}
	s, _ := testService(t, testConfig(), router, provider)
	if _, err := s.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	router.addressFunc = func(ctx context.Context, _ ros.Family) ([]ros.Address, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.TriggerItem("home")
	cancel, done := startService(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("new Run did not start a fresh poll")
	}
	if s.Ready() {
		t.Fatal("new Run considered a previous session's poll ready")
	}
	stopService(t, cancel, done)
	if len(provider.recorded()) != 2 {
		t.Fatal("new Run acted on queued work before a fresh poll")
	}
}
