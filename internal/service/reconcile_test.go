package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/ros"
)

func changeReconciliationSource(it *item, clock *fakeClock, mode string) {
	switch mode {
	case "poll failure":
		it.observe(nil, errors.New("router read failed"), clock.now(), time.Time{})
	case "empty source":
		it.observe(nil, nil, clock.now(), time.Time{})
	default:
		it.observe([]ros.Address{address("203.0.113.11/24")}, nil, clock.now(), time.Time{})
		if mode == "away and back" {
			it.observe([]ros.Address{address("203.0.113.10/24")}, nil, clock.now(), time.Time{})
		}
	}
}

func TestDNSReconciliationStopsAfterSourceInvalidation(t *testing.T) {
	for _, mode := range []string{"changed", "poll failure", "empty source", "away and back"} {
		for _, force := range []bool{false, true} {
			name := mode + "/normal"
			if force {
				name = mode + "/force"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					router, provider := newFakeROS(), &fakeProvider{}
					entered, release := make(chan struct{}), make(chan struct{})
					var first sync.Once
					provider.ensureFunc = func(ctx context.Context, _ dns.Record) error {
						first.Do(func() {
							close(entered)
							select {
							case <-release:
							case <-ctx.Done():
							}
						})
						return ctx.Err()
					}
					s, clock := testService(t, testConfig(), router, provider)
					if err := s.poller.pollAll(context.Background()); err != nil {
						t.Fatal(err)
					}
					it := s.items[0]
					it.takeRequest()
					done := make(chan reconcileResult, 1)
					go func() { done <- it.reconcile(context.Background(), force) }()
					<-entered
					changeReconciliationSource(it, clock, mode)
					close(release)
					if result := <-done; !errors.Is(result.err, errSourceChanged) {
						t.Fatalf("obsolete reconciliation returned %#v", result)
					}
					status := s.Snapshot().Items[0]
					if len(provider.recorded()) != 1 || router.listCalls != 0 {
						t.Fatal("obsolete DNS result allowed later target calls")
					}
					if !status.LastSyncAt.IsZero() {
						t.Fatal("obsolete result advanced LastSyncAt")
					}
					for _, domain := range status.Domains {
						if domain.Value != "" || domain.RecordID != "" || !domain.UpdatedAt.IsZero() {
							t.Fatalf("obsolete result published a confirmation: %#v", domain)
						}
					}
					if force && !it.pendingForce {
						t.Fatal("source invalidation lost the force request")
					}
					if mode == "poll failure" || mode == "empty source" {
						it.observe([]ros.Address{address("203.0.113.10/24")}, nil, clock.now(), time.Time{})
					}
					retryForce, usable := it.takeRequest()
					if !usable {
						t.Fatal("test source did not recover")
					}
					if result := it.reconcile(context.Background(), retryForce); result.err != nil {
						t.Fatal(result.err)
					}
					if len(provider.recorded()) != 3 || s.Snapshot().Items[0].Phase != "synced" {
						t.Fatal("source recovery did not retry all unconfirmed targets")
					}
					for _, call := range provider.recorded()[1:] {
						if call.record.Value != it.desired {
							t.Fatalf("retry wrote obsolete desired value: %#v", call)
						}
					}
				})
			})
		}
	}
}

func TestStaleDNSWriteInvalidatesFormerConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		router, provider := newFakeROS(), &fakeProvider{}
		c := testConfig()
		s, clock := testService(t, c, router, provider)
		before, err := s.runOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		it := s.items[0]
		it.observe([]ros.Address{address("203.0.113.11/24")}, nil, clock.now(), time.Time{})
		entered, release := make(chan struct{}), make(chan struct{})
		var first sync.Once
		provider.ensureFunc = func(ctx context.Context, _ dns.Record) error {
			first.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
			return ctx.Err()
		}
		done := make(chan reconcileResult, 1)
		go func() { done <- it.reconcile(context.Background(), false) }()
		<-entered
		it.observe([]ros.Address{address("203.0.113.10/24")}, nil, clock.now(), time.Time{})
		close(release)
		if result := <-done; !errors.Is(result.err, errSourceChanged) {
			t.Fatalf("obsolete DNS write succeeded: %#v", result)
		}
		status := s.Snapshot().Items[0]
		for i, got := range status.Domains {
			previous := before.Items[0].Domains[i]
			if got.Value != previous.Value || got.RecordID != previous.RecordID || !got.UpdatedAt.Equal(previous.UpdatedAt) {
				t.Fatalf("obsolete result replaced displayed confirmations: %#v", got)
			}
		}
		if it.domainValues[0] != "" || status.Domains[0].LastError == "" {
			t.Fatal("obsolete write did not invalidate the live and persisted skip caches")
		}
		c.Runtime.SyncOnStart = false
		restarted, err := New(c, Deps{
			NewROS:      func(config.RouterOSConfig) (ros.Client, error) { return router, nil },
			NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) { return provider, nil },
			Selector:    fakeSelector, Now: clock.now, State: s.store,
		})
		if err != nil {
			t.Fatal(err)
		}
		pollAndReconcile(t, restarted, false)
		calls := provider.recorded()
		if len(calls) != 4 || calls[3].record.FQDN() != "home.example.com" || calls[3].record.Value != "203.0.113.10" || restarted.Snapshot().Items[0].Phase != "synced" {
			t.Fatalf("former cache skipped the potentially changed remote record: %v", calls)
		}
	})
}

type blockingAddROS struct {
	*fakeROS
	entered chan struct{}
	release chan struct{}
	first   sync.Once
}

func (r *blockingAddROS) AddEntry(ctx context.Context, family ros.Family, entry ros.ListEntry) error {
	r.first.Do(func() {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.fakeROS.AddEntry(ctx, family, entry)
}

func TestListAddDoesNotPublishAfterSourceChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.Items[0].Domains = nil
		router := newFakeROS()
		s, clock := testService(t, c, router, &fakeProvider{})
		if err := s.poller.pollAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		blocking := &blockingAddROS{fakeROS: router, entered: make(chan struct{}), release: make(chan struct{})}
		s.client = blocking
		it := s.items[0]
		done := make(chan reconcileResult, 1)
		go func() { done <- it.reconcile(context.Background(), false) }()
		<-blocking.entered
		it.observe([]ros.Address{address("203.0.113.11/24")}, nil, clock.now(), time.Time{})
		close(blocking.release)
		if result := <-done; !errors.Is(result.err, errSourceChanged) {
			t.Fatalf("obsolete add returned %#v", result)
		}
		status := s.Snapshot().Items[0]
		if len(router.adds) != 1 || len(status.AddressList.Entries) != 0 || !status.AddressList.UpdatedAt.IsZero() || !status.LastSyncAt.IsZero() || it.listValue != "" || !it.listStale {
			t.Fatalf("obsolete add published its confirmation: %#v", status)
		}
		if result := it.reconcile(context.Background(), false); result.err != nil {
			t.Fatal(result.err)
		}
		if len(router.removes) != 1 || len(router.adds) != 2 || router.adds[1].Address != "203.0.113.11" || s.Snapshot().Items[0].Phase != "synced" {
			t.Fatalf("new source did not repair the obsolete in-flight add: %v", router.operations)
		}
	})
}

func TestListReadStopsAfterSourceInvalidation(t *testing.T) {
	for _, mode := range []string{"changed", "poll failure", "away and back"} {
		for _, listOnly := range []bool{false, true} {
			name := mode + "/normal"
			if listOnly {
				name = mode + "/list-only"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := testConfig()
					c.Items[0].Domains = nil
					router := newFakeROS()
					router.entries[ros.IPv4] = []ros.ListEntry{{ID: "old", List: "ddns", Address: "203.0.113.20", Comment: "ros-ddns:home"}}
					entered, release := make(chan struct{}), make(chan struct{})
					var first sync.Once
					router.listFunc = func(ctx context.Context, _ ros.Family, _ string) error {
						first.Do(func() {
							close(entered)
							select {
							case <-release:
							case <-ctx.Done():
							}
						})
						return ctx.Err()
					}
					s, clock := testService(t, c, router, &fakeProvider{})
					if err := s.poller.pollAll(context.Background()); err != nil {
						t.Fatal(err)
					}
					it := s.items[0]
					it.takeRequest()
					done := make(chan error, 1)
					go func() {
						if listOnly {
							it.listMu.Lock()
							defer it.listMu.Unlock()
							done <- it.reconcileList(context.Background(), "203.0.113.10")
						} else {
							done <- it.reconcile(context.Background(), true).err
						}
					}()
					<-entered
					changeReconciliationSource(it, clock, mode)
					close(release)
					if err := <-done; !errors.Is(err, errSourceChanged) {
						t.Fatalf("obsolete list read returned %v", err)
					}
					status := s.Snapshot().Items[0]
					if len(router.operations) != 0 || len(status.AddressList.Entries) != 0 || !status.AddressList.UpdatedAt.IsZero() || !status.LastSyncAt.IsZero() || !it.listStale {
						t.Fatalf("obsolete list read mutated or confirmed targets: status=%#v operations=%v", status, router.operations)
					}
					if mode == "poll failure" {
						it.observe([]ros.Address{address("203.0.113.10/24")}, nil, clock.now(), time.Time{})
					}
					if result := it.reconcile(context.Background(), false); result.err != nil {
						t.Fatal(result.err)
					}
					if len(router.adds) != 1 || router.adds[0].Address != it.desired || s.Snapshot().Items[0].Phase != "synced" {
						t.Fatal("source recovery did not retry the stale list")
					}
				})
			})
		}
	}
}

func TestListReadHonorsRenewalBecomingDueWhileBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.Items[0].Domains = nil
		c.Items[0].AddressList.Timeout = "2s"
		c.Runtime.ReconcileTimeout.Duration = 10 * time.Second
		router := newFakeROS()
		s, _ := testService(t, c, router, &fakeProvider{})
		if _, err := s.runOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		router.listFunc = func(ctx context.Context, _ ros.Family, _ string) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		it := s.items[0]
		done := make(chan reconcileResult, 1)
		go func() { done <- it.reconcile(context.Background(), true) }()
		<-entered
		time.Sleep(time.Second)
		it.mu.Lock()
		// This is the expiry marker set by runListRenewal before it waits
		// on listMu; the normal read already holds that lock here.
		it.listStale = true
		it.mu.Unlock()
		close(release)
		if result := <-done; result.err != nil {
			t.Fatal(result.err)
		}
		if len(router.removes) != 1 || len(router.adds) != 2 || it.listStale || s.Snapshot().Items[0].Phase != "synced" {
			t.Fatalf("normal list read consumed a concurrent renewal without refreshing lifetime: %v", router.operations)
		}
	})
}

type blockingRemoveROS struct {
	*fakeROS
	entered chan struct{}
	release chan struct{}
	first   sync.Once
}

func (r *blockingRemoveROS) RemoveEntry(ctx context.Context, family ros.Family, id string) error {
	r.first.Do(func() {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.fakeROS.RemoveEntry(ctx, family, id)
}

func TestListRemovalStopsBeforeFurtherMutationsAfterSourceChanges(t *testing.T) {
	for _, secondEntry := range []bool{false, true} {
		name := "before add"
		if secondEntry {
			name = "before next remove"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testConfig()
				c.Items[0].Domains = nil
				router := newFakeROS()
				s, clock := testService(t, c, router, &fakeProvider{})
				before, err := s.runOnce(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if secondEntry {
					router.entries[ros.IPv4] = append(router.entries[ros.IPv4], ros.ListEntry{ID: "other-old", List: "ddns", Address: "203.0.113.20", Comment: "ros-ddns:home"})
				}
				blocking := &blockingRemoveROS{fakeROS: router, entered: make(chan struct{}), release: make(chan struct{})}
				s.client = blocking
				it := s.items[0]
				it.observe([]ros.Address{address("203.0.113.11/24")}, nil, clock.now(), time.Time{})
				done := make(chan reconcileResult, 1)
				go func() { done <- it.reconcile(context.Background(), false) }()
				<-blocking.entered
				it.observe([]ros.Address{address("203.0.113.10/24")}, nil, clock.now(), time.Time{})
				close(blocking.release)
				if result := <-done; !errors.Is(result.err, errSourceChanged) {
					t.Fatalf("obsolete removal returned %#v", result)
				}
				if len(router.removes) != 1 || len(router.adds) != 1 || !it.listStale {
					t.Fatalf("obsolete removal continued with more mutations: %v", router.operations)
				}
				if got := s.Snapshot().Items[0].AddressList; !reflect.DeepEqual(got, before.Items[0].AddressList) {
					t.Fatalf("obsolete removal published a list result: %#v", got)
				}
				if result := it.reconcile(context.Background(), false); result.err != nil {
					t.Fatal(result.err)
				}
				if router.listCalls != 3 || len(router.adds) != 2 || router.adds[1].Address != "203.0.113.10" || s.Snapshot().Items[0].Phase != "synced" {
					t.Fatalf("former list confirmation skipped required recovery: %v", router.operations)
				}
			})
		})
	}
}

func TestWorkerRotatesPastProviderThatConsumesEveryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.Items[0].AddressList = nil
		c.Items[0].Domains[0].Provider = "slow"
		c.DNS.Providers["slow"] = config.ProviderConfig{Type: "aliyun"}
		c.Runtime.PollInterval.Duration = time.Second
		c.Runtime.MaxBackoff.Duration = time.Second
		c.Runtime.ReconcileTimeout.Duration = 3 * time.Second
		healthy := &fakeProvider{}
		slow := &fakeProvider{ensureFunc: func(ctx context.Context, _ dns.Record) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		s, _ := testService(t, c, newFakeROS(), healthy)
		s.providers["slow"] = slow
		cancel, done := startService(t, s)
		defer cancel()
		synctest.Wait()
		if len(slow.recorded()) != 1 || len(healthy.recorded()) != 0 {
			t.Fatal("test did not start with the slow provider blocking")
		}
		for retry := 1; retry <= 2; retry++ {
			time.Sleep(4 * time.Second)
			synctest.Wait()
			status := s.Snapshot().Items[0]
			if len(slow.recorded()) != retry+1 || len(healthy.recorded()) != 1 || status.Domains[1].Value != "203.0.113.10" || status.Domains[1].LastError != "" {
				t.Fatalf("retry %d starved or repeated the healthy target: slow=%d healthy=%d status=%#v", retry, len(slow.recorded()), len(healthy.recorded()), status)
			}
		}
		stopService(t, cancel, done)
	})
}

func TestForceRetainsUnvisitedCachedTargetsAcrossInterruptedRounds(t *testing.T) {
	for _, mode := range []string{"deadline exceeded", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testConfig()
				c.Items[0].AddressList = nil
				c.Items[0].Domains[0].Provider = "slow"
				c.DNS.Providers["slow"] = config.ProviderConfig{Type: "aliyun"}
				c.Runtime.ReconcileTimeout.Duration = 3 * time.Second
				healthy, slow := &fakeProvider{}, &fakeProvider{}
				s, _ := testService(t, c, newFakeROS(), healthy)
				s.providers["slow"] = slow
				before, err := s.runOnce(context.Background())
				if err != nil || before.Items[0].Phase != "synced" {
					t.Fatalf("could not seed target confirmations: %#v, %v", before, err)
				}
				slow.ensureFunc = func(ctx context.Context, _ dns.Record) error {
					<-ctx.Done()
					return ctx.Err()
				}
				it := s.items[0]
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan reconcileResult, 1)
				go func() { done <- it.reconcile(ctx, true) }()
				synctest.Wait()
				if mode == "canceled" {
					cancel()
				} else {
					time.Sleep(c.Runtime.ReconcileTimeout.Duration)
				}
				if result := <-done; result.err == nil {
					t.Fatal("interrupted force lost the failure")
				}
				status := s.Snapshot().Items[0]
				for i, domain := range status.Domains {
					previous := before.Items[0].Domains[i]
					if domain.Value != previous.Value || domain.RecordID != previous.RecordID || !domain.UpdatedAt.Equal(previous.UpdatedAt) || !strings.Contains(domain.LastError, mode) {
						t.Fatalf("interruption lost the visible confirmation or error: %#v", domain)
					}
				}
				if len(healthy.recorded()) != 1 {
					t.Fatal("interrupted round started the healthy provider after cancellation")
				}
				for range 2 {
					if result := it.reconcile(context.Background(), false); result.err == nil {
						t.Fatal("retry lost the still-failing provider")
					}
					if len(healthy.recorded()) != 2 || s.Snapshot().Items[0].Domains[1].LastError != "" {
						t.Fatal("retry skipped the unvisited force target or repeated its confirmation")
					}
				}
				if len(slow.recorded()) != 4 {
					t.Fatal("failed provider stopped receiving retries")
				}
			})
		})
	}
}
