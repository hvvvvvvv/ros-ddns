package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ros-ddns/internal/notify"
	"ros-ddns/internal/service"
)

type snapshotService struct {
	snapshot service.Snapshot
}

func (s *snapshotService) Snapshot() service.Snapshot { return s.snapshot }
func (*snapshotService) Ready() bool                  { return true }
func (*snapshotService) TriggerItem(string) bool      { return true }
func (*snapshotService) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func TestNotificationSignaturesTransitionsAndDedupe(t *testing.T) {
	store := notify.NewStore(50)
	f := newFixture(t, Options{Notifications: store})
	svc := &snapshotService{snapshot: service.Snapshot{Items: []service.ItemStatus{{
		Name: "home", Phase: "synced", Domains: []service.DomainStatus{{}}, AddressList: &service.AddressListStatus{},
	}}}}
	f.m.svc = svc
	previous := make(map[string]notificationState)
	poll := func(want int, kind string) {
		t.Helper()
		f.m.pollNotifications(previous)
		events := store.History(0)
		if len(events) != want {
			t.Fatalf("got %d notifications, want %d: %+v", len(events), want, events)
		}
		if want > 0 {
			last := events[len(events)-1]
			if last.Kind != kind || last.Item != "home" || last.Phase == "" || last.Message == "" {
				t.Fatalf("invalid notification: %+v", last)
			}
			severity := "error"
			if kind == "item_recovery" {
				severity = "info"
			}
			if last.Severity != severity || strings.Contains(last.Message, "initial-router-secret") {
				t.Fatalf("notification severity/redaction failure: %+v", last)
			}
		}
	}
	poll(0, "")
	item := &svc.snapshot.Items[0]
	item.Phase, item.LastError = "degraded", "source failed using initial-router-secret"
	poll(1, "item_error")
	poll(1, "item_error")
	item.Domains[0].LastError = "DNS failed"
	poll(2, "item_error")
	item.AddressList.LastError = "address-list failed"
	poll(3, "item_error")
	item.Phase = "error"
	poll(4, "item_error")
	item.SourceObservedAt, item.NextPollAt = time.Now(), time.Now().Add(time.Hour)
	item.Domains[0].UpdatedAt = time.Now()
	poll(4, "item_error")
	item.LastError, item.Domains[0].LastError, item.AddressList.LastError = "", "", ""
	// An error phase alone still counts as an error.
	poll(5, "item_error")
	item.Phase = "synced"
	poll(6, "item_recovery")
	poll(6, "item_recovery")
	item.Domains[0].LastError = "domain-only failure"
	poll(7, "item_error")
	item.Domains[0].LastError = ""
	item.AddressList.LastError = "list-only failure"
	poll(8, "item_error")
	f.m.reloading = true
	item.AddressList.LastError = ""
	poll(8, "item_error")
	f.m.reloading = false
	poll(9, "item_recovery")
	item.LastError = "temporary"
	poll(10, "item_error")
	item.Disabled = true
	poll(10, "item_error")
	item.Disabled = false
	poll(11, "item_error")
	svc.snapshot.Items = nil
	f.m.pollNotifications(previous)
	if len(previous) != 0 || len(store.History(0)) != 11 {
		t.Fatal("removed/disabled items retained state or emitted false recovery")
	}
}

func TestNotificationMonitorObservesRealErrorAndRecovery(t *testing.T) {
	store := notify.NewStore(30)
	f := newFixture(t, Options{Notifications: store})
	f.provider.err = errors.New("DNS rejected initial-router-secret")
	running := startManager(t, f.m)
	await(t, func() bool {
		events := store.History(0)
		return len(events) > 0 && events[0].Kind == "item_error"
	})
	first := store.History(0)[0]
	if strings.Contains(first.Message, "initial-router-secret") || !strings.Contains(first.Message, "[redacted]") {
		t.Fatalf("monitor published a credential: %+v", first)
	}
	f.provider.mu.Lock()
	f.provider.err = nil
	f.provider.mu.Unlock()
	if !f.m.TriggerItem("home") {
		t.Fatal("recovery trigger failed")
	}
	await(t, func() bool {
		events := store.History(0)
		return len(events) == 2 && events[1].Kind == "item_recovery"
	})
	running.stop(t)
	if len(store.History(0)) != 2 {
		t.Fatal("notification monitor did not deduplicate unchanged snapshots")
	}
}

func TestNotificationStoreRemainsCallerOwned(t *testing.T) {
	store := notify.NewStore(10)
	f := newFixture(t, Options{Notifications: store})
	running := startManager(t, f.m)
	running.stop(t)
	store.Add(notify.Event{Kind: "caller", Severity: "info", Message: "still open"})
	if events := store.History(0); len(events) != 1 || events[0].Kind != "caller" {
		t.Fatalf("manager closed the caller's notification store: %+v", events)
	}
}
