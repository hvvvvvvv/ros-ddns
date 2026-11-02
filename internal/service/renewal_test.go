package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"ros-ddns/internal/dns"
	"ros-ddns/internal/ros"
)

func TestDynamicListRenewsBeforeExpiryWithoutDNSOrSourcePoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.Items[0].AddressList.Timeout = "2s"
		c.Runtime.VerifyInterval.Duration = 10 * time.Minute
		router, provider := newFakeROS(), &fakeProvider{}
		s, _ := testService(t, c, router, provider)
		cancel, done := startService(t, s)
		defer cancel()
		synctest.Wait()
		if router.listCalls != 1 || len(router.adds) != 1 || s.Snapshot().Items[0].Phase != "synced" {
			t.Fatal("initial dynamic entry was not synced")
		}
		for generation := 1; generation <= 3; generation++ {
			expires := time.Now().Add(2 * time.Second)
			time.Sleep(time.Second - time.Nanosecond)
			synctest.Wait()
			if router.listCalls != generation {
				t.Fatalf("renewed early: reads=%d, generation=%d", router.listCalls, generation)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if !time.Now().Before(expires) || router.listCalls != generation+1 || len(router.adds) != generation+1 || len(router.removes) != generation {
				t.Fatalf("did not re-list and replace before expiry: reads=%d operations=%v", router.listCalls, router.operations)
			}
			if len(router.entries[ros.IPv4]) != 1 || router.entries[ros.IPv4][0].Disabled || router.entries[ros.IPv4][0].Timeout != "2s" || s.Snapshot().Items[0].Phase != "synced" {
				t.Fatal("renewal did not preserve an enabled dynamic target")
			}
		}
		if len(provider.recorded()) != 2 || router.pollCalls[ros.IPv4] != 1 {
			t.Fatalf("renewal forced other work: DNS=%v polls=%v", provider.recorded(), router.pollCalls)
		}
		stopService(t, cancel, done)
		reads := router.listCalls
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if router.listCalls != reads {
			t.Fatal("renewal continued after cancellation")
		}
	})
}

func TestStaticListHasNoRenewalTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		router, provider := newFakeROS(), &fakeProvider{}
		s, _ := testService(t, testConfig(), router, provider)
		cancel, done := startService(t, s)
		defer cancel()
		synctest.Wait()
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		if router.listCalls != 1 || len(router.adds) != 1 || len(provider.recorded()) != 2 || !s.items[0].listRenewAt.IsZero() {
			t.Fatalf("static target was periodically reconciled: reads=%d operations=%v", router.listCalls, router.operations)
		}
		stopService(t, cancel, done)
	})
}

func TestDynamicSynchronousRunDoesNotStartRenewalWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.State.Path = filepath.Join(t.TempDir(), "state.json")
		c.Items[0].AddressList.Timeout = "2s"
		router := newFakeROS()
		// The second service restores a stale cache with an immediate deadline.
		for run := 1; run <= 2; run++ {
			s, _ := testService(t, c, router, &fakeProvider{})
			if _, err := s.runOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Minute)
			synctest.Wait()
			if router.listCalls != run || len(router.adds) != run {
				t.Fatalf("runOnce started background renewal on run %d", run)
			}
		}
	})
}

func TestListRenewalFailureUsesBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.Items[0].AddressList.Timeout = "2s"
		router, provider := newFakeROS(), &fakeProvider{}
		s, _ := testService(t, c, router, provider)
		cancel, done := startService(t, s)
		defer cancel()
		synctest.Wait()
		router.mu.Lock()
		router.listError = errors.New("list temporarily unavailable")
		router.mu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		if router.listCalls != 2 || s.Snapshot().Items[0].Phase != "degraded" {
			t.Fatal("failed renewal did not invalidate synced status")
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if router.listCalls != 2 {
			t.Fatal("failed renewal bypassed retry backoff")
		}
		router.mu.Lock()
		router.listError = nil
		router.mu.Unlock()
		time.Sleep(1600 * time.Millisecond)
		synctest.Wait()
		if router.listCalls != 3 || len(router.adds) != 2 || len(provider.recorded()) != 2 || s.Snapshot().Items[0].Phase != "synced" {
			t.Fatal("renewal retry did not recover independently")
		}
		stopService(t, cancel, done)
	})
}

func TestRenewalWaitsForSourceRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.Items[0].AddressList.Timeout = "2s"
		router, provider := newFakeROS(), &fakeProvider{}
		s, _ := testService(t, c, router, provider)
		cancel, done := startService(t, s)
		defer cancel()
		synctest.Wait()
		router.setAddresses(ros.IPv4)
		s.poller.reset()
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if router.listCalls != 1 || len(router.removes) != 0 || len(router.adds) != 1 {
			t.Fatal("empty source did not preserve the address list during renewal")
		}
		router.setAddresses(ros.IPv4, address("203.0.113.10/24"))
		s.poller.reset()
		synctest.Wait()
		if router.listCalls != 2 || len(router.adds) != 2 || len(provider.recorded()) != 2 || s.Snapshot().Items[0].Phase != "synced" {
			t.Fatal("source recovery did not resume pending list renewal")
		}
		stopService(t, cancel, done)
	})
}

func TestDynamicCacheAndTimeoutChangesRequireFreshList(t *testing.T) {
	for _, timeout := range []string{"1h", "2h", ""} {
		t.Run("new timeout="+timeout, func(t *testing.T) {
			c := testConfig()
			c.State.Path = filepath.Join(t.TempDir(), "state.json")
			c.Items[0].AddressList.Timeout = "1h"
			router := newFakeROS()
			s, _ := testService(t, c, router, &fakeProvider{})
			if _, err := s.runOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			c.Runtime.SyncOnStart = false
			c.Items[0].AddressList.Timeout = timeout
			provider := &fakeProvider{}
			restarted, _ := testService(t, c, router, provider)
			pollAndReconcile(t, restarted, false)
			if router.listCalls != 2 || len(router.adds) != 2 || router.adds[1].Timeout != timeout || len(provider.recorded()) != 0 {
				t.Fatalf("dynamic cache/timeout change was missed: operations=%v DNS=%v", router.operations, provider.recorded())
			}
			if timeout == "" && !restarted.items[0].listRenewAt.IsZero() {
				t.Fatal("conversion to static retained renewal schedule")
			}
		})
	}
}

func TestListRenewalContinuesWhileDNSEnsureIsBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.Items[0].AddressList.Timeout = "2s"
		c.Runtime.ReconcileTimeout.Duration = 30 * time.Second
		router, provider := newFakeROS(), &fakeProvider{}
		s, _ := testService(t, c, router, provider)
		cancel, done := startService(t, s)
		defer cancel()
		synctest.Wait()
		if s.Snapshot().Items[0].Phase != "synced" || router.listCalls != 1 {
			t.Fatal("initial list was not synced")
		}
		entered, exited := make(chan struct{}, 1), make(chan struct{}, 1)
		provider.mu.Lock()
		provider.ensureFunc = func(ctx context.Context, _ dns.Record) error {
			entered <- struct{}{}
			<-ctx.Done()
			exited <- struct{}{}
			return ctx.Err()
		}
		provider.mu.Unlock()
		s.TriggerItem("home")
		synctest.Wait()
		if len(entered) != 1 || len(provider.recorded()) != 3 {
			t.Fatal("forced DNS Ensure did not block")
		}
		for generation := 1; generation <= 3; generation++ {
			time.Sleep(time.Second)
			synctest.Wait()
			if router.listCalls != generation+1 || len(router.adds) != generation+1 || len(router.removes) != generation {
				t.Fatalf("renewal waited behind DNS: reads=%d operations=%v", router.listCalls, router.operations)
			}
			if len(provider.recorded()) != 3 || len(exited) != 0 {
				t.Fatal("renewal called DNS or Ensure stopped blocking")
			}
		}
		stopService(t, cancel, done)
		if len(exited) != 1 {
			t.Fatal("Run did not join the blocked DNS worker")
		}
		reads := router.listCalls
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if router.listCalls != reads {
			t.Fatal("renewal continued after Run returned")
		}
	})
}

func TestRestoredListRenewsWhileInitialDNSEnsureIsBlocked(t *testing.T) {
	for _, syncOnStart := range []bool{true, false} {
		for _, entryKind := range []string{"owned dynamic", "foreign dynamic", "expired foreign"} {
			t.Run(fmt.Sprintf("syncOnStart=%v/%s", syncOnStart, entryKind), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := testConfig()
					c.State.Path = filepath.Join(t.TempDir(), "state.json")
					c.Runtime.SyncOnStart = syncOnStart
					c.Runtime.ReconcileTimeout.Duration = 30 * time.Second
					c.Items[0].Domains = c.Items[0].Domains[:1]
					router := newFakeROS()
					foreign := ros.ListEntry{ID: "foreign", List: "ddns", Address: "203.0.113.10", Comment: "manual", Dynamic: true, Timeout: "2s"}
					if entryKind == "owned dynamic" {
						c.Items[0].AddressList.Timeout = "2s"
					} else {
						router.entries[ros.IPv4] = []ros.ListEntry{foreign}
					}
					original, _ := testService(t, c, router, &fakeProvider{})
					cached, err := original.runOnce(context.Background())
					if err != nil || cached.Items[0].Phase != "synced" {
						t.Fatalf("could not seed restored state: %#v, %v", cached, err)
					}
					if entryKind == "expired foreign" {
						router.entries[ros.IPv4] = nil
					}
					if !syncOnStart {
						c.Items[0].Domains[0].TTL = 120
					}
					entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
					listRelease := make(chan struct{})
					provider := &fakeProvider{ensureFunc: func(ctx context.Context, _ dns.Record) error {
						close(entered)
						defer close(exited)
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}}
					// Make the first list read finish only after startup DNS is blocked.
					router.listFunc = func(ctx context.Context, _ ros.Family, _ string) error {
						select {
						case <-entered:
						case <-ctx.Done():
							return ctx.Err()
						}
						select {
						case <-listRelease:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					restarted, clock := testService(t, c, router, provider)
					clock.advance(time.Minute)
					if due := restarted.items[0].listRenewAt; due.IsZero() || due.After(time.Now()) {
						t.Fatal("restored list has no immediate renewal deadline")
					}
					cancel, done := startService(t, restarted)
					defer cancel()
					synctest.Wait()
					if len(provider.recorded()) != 1 || router.listCalls != 2 {
						t.Fatalf("initial list read waited for DNS: reads=%d DNS=%v", router.listCalls, provider.recorded())
					}
					if status := restarted.Snapshot().Items[0]; status.Phase == "synced" || !status.LastSyncAt.Equal(cached.Items[0].LastSyncAt) {
						t.Fatalf("pending initial list read falsely reported sync: %#v", status)
					}
					close(listRelease)
					synctest.Wait()
					if entryKind == "owned dynamic" {
						if len(router.adds) != 2 || len(router.removes) != 1 || len(router.entries[ros.IPv4]) != 1 {
							t.Fatalf("owned entry was not renewed exactly once: %v", router.operations)
						}
					} else if entryKind == "foreign dynamic" {
						if len(router.operations) != 0 || !reflect.DeepEqual(router.entries[ros.IPv4], []ros.ListEntry{foreign}) {
							t.Fatal("startup changed the live foreign entry")
						}
						// The router expires its foreign entry before our scheduled recheck.
						router.entries[ros.IPv4] = nil
						time.Sleep(time.Second)
						synctest.Wait()
						if router.listCalls != 3 {
							t.Fatal("foreign lifetime was not rechecked while DNS was blocked")
						}
					}
					if entryKind != "owned dynamic" && (len(router.adds) != 1 || len(router.removes) != 0 || len(router.entries[ros.IPv4]) != 1) {
						t.Fatalf("expired foreign entry was not replaced exactly once: %v", router.operations)
					}
					status := restarted.Snapshot().Items[0]
					if status.Phase == "synced" || !status.LastSyncAt.Equal(cached.Items[0].LastSyncAt) || !status.AddressList.UpdatedAt.Equal(clock.now()) {
						t.Fatalf("list-only renewal falsely confirmed startup DNS: %#v", status)
					}
					select {
					case <-exited:
						t.Fatal("DNS Ensure stopped blocking before list renewal")
					default:
					}
					operations, reads := append([]string(nil), router.operations...), router.listCalls
					close(release)
					synctest.Wait()
					if restarted.Snapshot().Items[0].Phase != "synced" || router.listCalls != reads || !reflect.DeepEqual(router.operations, operations) || len(provider.recorded()) != 1 {
						t.Fatalf("DNS completion duplicated list operations or failed to sync: reads=%d operations=%v", router.listCalls, router.operations)
					}
					stopService(t, cancel, done)
				})
			})
		}
	}
}

func TestStaleListRenewalWaitsForUsableSourceAndObservationWake(t *testing.T) {
	for _, scheduled := range []bool{true, false} {
		t.Run(fmt.Sprintf("deadline already scheduled=%v", scheduled), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testConfig()
				c.Runtime.SyncOnStart = false
				router, provider := newFakeROS(), &fakeProvider{}
				s, clock := testService(t, c, router, provider)
				if _, err := s.runOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				it := s.items[0]
				it.listStale = true
				if scheduled {
					it.listRenewAt = time.Now()
					it.observe(nil, nil, clock.now(), time.Time{})
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { defer close(done); it.runListRenewal(ctx) }()
				synctest.Wait()
				time.Sleep(10 * time.Second)
				synctest.Wait()
				if router.listCalls != 1 || len(router.adds) != 1 {
					t.Fatal("renewal did not wait for an eligible source/deadline")
				}
				it.observe([]ros.Address{address("203.0.113.10/24")}, nil, clock.now(), time.Time{})
				synctest.Wait()
				if router.listCalls != 2 || len(router.operations) != 1 || len(provider.recorded()) != 2 || it.listStale {
					t.Fatal("observation did not wake list-only renewal")
				}
				if !scheduled && len(it.wake) != 0 {
					t.Fatal("scheduling a stale list on an already usable source queued normal reconciliation")
				}
				cancel()
				<-done
			})
		})
	}
}

func TestListRenewalDeadlineAndCancellation(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testConfig()
				c.Items[0].AddressList.Timeout = "2s"
				c.Runtime.ReconcileTimeout.Duration = 3 * time.Second
				router, provider := newFakeROS(), &fakeProvider{}
				s, _ := testService(t, c, router, provider)
				cancel, done := startService(t, s)
				defer cancel()
				synctest.Wait()
				deadlines, exited := make(chan time.Time, 2), make(chan struct{}, 2)
				router.mu.Lock()
				router.listFunc = func(ctx context.Context, _ ros.Family, _ string) error {
					deadline, _ := ctx.Deadline()
					deadlines <- deadline
					<-ctx.Done()
					exited <- struct{}{}
					return ctx.Err()
				}
				router.mu.Unlock()
				time.Sleep(time.Second)
				synctest.Wait()
				if len(deadlines) != 1 || router.listCalls != 2 {
					t.Fatal("renewal did not start a list read")
				}
				if deadline := <-deadlines; !deadline.Equal(time.Now().Add(c.Runtime.ReconcileTimeout.Duration)) {
					t.Fatalf("renewal deadline = %v", deadline)
				}
				if mode == "deadline" {
					time.Sleep(c.Runtime.ReconcileTimeout.Duration)
					synctest.Wait()
					if len(exited) != 1 || !strings.Contains(s.Snapshot().Items[0].AddressList.LastError, "deadline exceeded") {
						t.Fatal("renewal did not respect its own reconciliation timeout")
					}
				}
				stopService(t, cancel, done)
				if len(exited) != 1 || router.listCalls != 2 {
					t.Fatal("Run did not cancel and join the renewal read")
				}
			})
		})
	}
}

func TestListRenewalSerializesWithNormalReconciliation(t *testing.T) {
	// Mutex contention is not a durable block for synctest.Wait. Use real
	// channels/timers for this test while the other renewal tests use fake time.
	c := testConfig()
	c.Items[0].AddressList.Timeout = "1s"
	c.Runtime.ReconcileTimeout.Duration = 10 * time.Second
	router, provider := newFakeROS(), &fakeProvider{}
	s, _ := testService(t, c, router, provider)
	cancel, done := startService(t, s)
	defer cancel()
	awaitCondition(t, func() bool { return s.Snapshot().Items[0].Phase == "synced" })
	entered, exited := make(chan struct{}, 2), make(chan struct{}, 2)
	router.mu.Lock()
	router.listFunc = func(ctx context.Context, _ ros.Family, _ string) error {
		entered <- struct{}{}
		<-ctx.Done()
		exited <- struct{}{}
		return ctx.Err()
	}
	router.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal did not start")
	}
	s.TriggerItem("home")
	awaitCondition(t, func() bool { return len(provider.recorded()) == 4 })
	select {
	case <-entered:
		t.Fatal("normal list read overlapped the renewal read")
	case <-time.After(50 * time.Millisecond):
	}
	stopService(t, cancel, done)
	if len(exited) != 1 || router.listCalls != 2 {
		t.Fatal("Run did not cancel and join the serialized workers")
	}
}

func TestRestoredStaticCacheRevalidatesForeignDynamicEntry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired before restart=%v", expired), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testConfig()
				c.State.Path = filepath.Join(t.TempDir(), "state.json")
				c.Runtime.SyncOnStart = false
				c.Runtime.VerifyInterval.Duration = 0
				router := newFakeROS()
				foreign := ros.ListEntry{ID: "foreign", List: "ddns", Address: "203.0.113.10", Comment: "manual", Dynamic: true, Timeout: "2s"}
				router.entries[ros.IPv4] = []ros.ListEntry{foreign}
				s, _ := testService(t, c, router, &fakeProvider{})
				if _, err := s.runOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				cached, _ := s.store.Get("home")
				if cached.AddressList.Timeout != "" || s.items[0].listRenewAt.IsZero() || len(router.adds) != 0 {
					t.Fatal("test did not cache a foreign dynamic entry under static configuration")
				}
				if expired {
					router.entries[ros.IPv4] = nil
				}
				provider := &fakeProvider{}
				restarted, _ := testService(t, c, router, provider)
				if !restarted.items[0].listStale || restarted.Snapshot().Items[0].Phase == "synced" {
					t.Fatal("restored static cache was trusted before revalidation")
				}
				cancel, done := startService(t, restarted)
				defer cancel()
				synctest.Wait()
				if router.listCalls != 2 || restarted.Snapshot().Items[0].Phase != "synced" || len(provider.recorded()) != 0 {
					t.Fatal("first source poll did not revalidate the list independently of DNS caches")
				}
				if !expired {
					if restarted.items[0].listRenewAt.IsZero() || len(router.adds) != 0 || !reflect.DeepEqual(router.entries[ros.IPv4], []ros.ListEntry{foreign}) {
						t.Fatal("observed foreign lifetime did not restore renewal")
					}
					router.mu.Lock()
					router.entries[ros.IPv4] = nil // the foreign entry expires on the router
					router.mu.Unlock()
					time.Sleep(time.Second)
					synctest.Wait()
				}
				if len(router.adds) != 1 || router.adds[0].Timeout != "" || len(router.removes) != 0 ||
					len(provider.recorded()) != 0 || restarted.Snapshot().Items[0].Phase != "synced" || !restarted.items[0].listRenewAt.IsZero() {
					t.Fatalf("expired foreign entry was not replaced with the configured static target: %v", router.operations)
				}
				stopService(t, cancel, done)
			})
		})
	}
}

func TestForeignDynamicUnknownLifetimeGetsBoundedRecheck(t *testing.T) {
	for _, timeout := range []string{"", "unknown", "0s"} {
		t.Run("timeout="+timeout, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				router, provider := newFakeROS(), &fakeProvider{}
				foreign := ros.ListEntry{ID: "foreign", List: "ddns", Address: "203.0.113.10", Comment: "manual", Dynamic: true, Timeout: timeout}
				router.entries[ros.IPv4] = []ros.ListEntry{foreign}
				s, _ := testService(t, testConfig(), router, provider)
				cancel, done := startService(t, s)
				defer cancel()
				synctest.Wait()
				if s.items[0].listRenewAt.IsZero() || s.Snapshot().Items[0].Phase != "synced" {
					t.Fatal("foreign dynamic entry with unknown expiry had no recheck")
				}
				time.Sleep(time.Minute - time.Nanosecond)
				synctest.Wait()
				if router.listCalls != 1 {
					t.Fatal("unknown expiry caused a tight recheck loop")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if router.listCalls != 2 || len(router.operations) != 0 || !reflect.DeepEqual(router.entries[ros.IPv4], []ros.ListEntry{foreign}) {
					t.Fatal("bounded recheck missed or modified the foreign entry")
				}
				router.mu.Lock()
				router.entries[ros.IPv4] = nil
				router.mu.Unlock()
				time.Sleep(time.Minute)
				synctest.Wait()
				if router.listCalls != 3 || len(router.adds) != 1 || len(router.removes) != 0 || len(provider.recorded()) != 2 ||
					s.Snapshot().Items[0].Phase != "synced" || !s.items[0].listRenewAt.IsZero() {
					t.Fatal("expired foreign dynamic target was not replaced independently")
				}
				stopService(t, cancel, done)
			})
		})
	}
}
