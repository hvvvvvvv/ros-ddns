package notify

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestStoreHistoryOverflowAndFields(t *testing.T) {
	store := NewStore(3)
	defer store.Close()
	when := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for range 5 {
		store.Add(Event{ID: 999, Time: when, Kind: "item_error", Severity: "error", Item: "home", Phase: "degraded", Message: "failure"})
	}
	for _, tt := range []struct {
		limit int
		ids   []uint64
	}{{0, []uint64{3, 4, 5}}, {-1, []uint64{3, 4, 5}}, {10, []uint64{3, 4, 5}}, {2, []uint64{4, 5}}, {1, []uint64{5}}} {
		events := store.History(tt.limit)
		if got := eventIDs(events); !reflect.DeepEqual(got, tt.ids) {
			t.Fatalf("History(%d) = %v, want %v", tt.limit, got, tt.ids)
		}
		for _, event := range events {
			if event.Time != when || event.Kind != "item_error" || event.Severity != "error" || event.Item != "home" || event.Phase != "degraded" || event.Message != "failure" {
				t.Fatalf("event fields changed: %#v", event)
			}
		}
	}
	store.Add(Event{Kind: "item_recovery", Severity: "info", Message: "recovered"})
	latest := store.History(1)[0]
	if latest.ID != 6 || latest.Time.IsZero() || latest.Kind != "item_recovery" || latest.Severity != "info" {
		t.Fatalf("recovery event = %#v", latest)
	}
	data, err := json.Marshal(latest)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 || fields["id"] != float64(6) || fields["kind"] != "item_recovery" {
		t.Fatalf("JSON fields = %s", data)
	}
}

func TestStoreDefaultCapacityAndCopies(t *testing.T) {
	for _, capacity := range []int{0, -5} {
		store := NewStore(capacity)
		if got := store.History(0); len(got) != 0 {
			t.Fatalf("new store has history: %#v", got)
		}
		for range 205 {
			store.Add(Event{Message: "original"})
		}
		events := store.History(0)
		if len(events) != 200 || events[0].ID != 6 || events[199].ID != 205 {
			t.Fatalf("default ring history = %#v", events)
		}
		events[199].Message = "changed"
		if got := store.History(1)[0].Message; got != "original" {
			t.Fatalf("history aliases store: %q", got)
		}
		ch, cancel := store.Subscribe(204)
		event := receiveEvent(t, ch)
		event.Message = "subscriber mutation"
		if got := store.History(1)[0].Message; got != "original" {
			t.Fatalf("subscriber aliases store: %q", got)
		}
		cancel()
		store.Close()
	}
}

func TestStoreReplayLiveSlowCancelAndClose(t *testing.T) {
	store := NewStore(3)
	for range 5 {
		store.Add(Event{})
	}
	stale, stopStale := store.Subscribe(1)
	stopStale()
	var replay []Event
	for e := range stale {
		replay = append(replay, e)
	}
	if got := eventIDs(replay); !reflect.DeepEqual(got, []uint64{3, 4, 5}) {
		t.Fatalf("stale replay = %v", got)
	}
	ch, cancel := store.Subscribe(3)
	for _, id := range []uint64{4, 5} {
		if event := receiveEvent(t, ch); event.ID != id {
			t.Fatalf("replay ID = %d, want %d", event.ID, id)
		}
	}
	store.Add(Event{})
	if event := receiveEvent(t, ch); event.ID != 6 {
		t.Fatalf("live ID = %d", event.ID)
	}
	done := make(chan struct{})
	go func() {
		for range 5 {
			store.Add(Event{})
		}
		close(done)
	}()
	waitDone(t, done)
	cancel()
	cancel()
	var slow []Event
	for event := range ch {
		slow = append(slow, event)
	}
	if got := eventIDs(slow); !reflect.DeepEqual(got, []uint64{9, 10, 11}) {
		t.Fatalf("slow consumer IDs = %v", got)
	}
	live, stop := store.Subscribe(11)
	store.Close()
	store.Close()
	stop()
	assertEventsClosed(t, live)
	store.Add(Event{})
	if got := eventIDs(store.History(0)); !reflect.DeepEqual(got, []uint64{9, 10, 11}) {
		t.Fatalf("closed history changed: %v", got)
	}
	closed, stop := store.Subscribe(0)
	stop()
	assertEventsClosed(t, closed)
}

func TestStoreFutureSubscription(t *testing.T) {
	store := NewStore(2)
	defer store.Close()
	ch, cancel := store.Subscribe(2)
	defer cancel()
	for range 3 {
		store.Add(Event{})
	}
	if event := receiveEvent(t, ch); event.ID != 3 {
		t.Fatalf("future subscription ID = %d", event.ID)
	}
	select {
	case e := <-ch:
		t.Fatalf("unexpected event: %#v", e)
	default:
	}
}

func TestStoreConcurrentWritersAndSubscribers(t *testing.T) {
	const writers, perWriter = 4, 200
	store := NewStore(writers * perWriter)
	ch, cancel := store.Subscribe(0)
	defer cancel()
	read := make(chan struct{})
	go func() {
		defer close(read)
		var last uint64
		for event := range ch {
			if event.ID != last+1 || event.Message != "event" {
				t.Errorf("concurrent event = %#v after %d", event, last)
			}
			last = event.ID
		}
		if last != writers*perWriter {
			t.Errorf("received through %d, want %d", last, writers*perWriter)
		}
	}()
	var work sync.WaitGroup
	for range writers {
		work.Go(func() {
			for range perWriter {
				store.Add(Event{Kind: "item_error", Severity: "error", Message: "event"})
			}
		})
	}
	work.Go(func() {
		for i := range 200 {
			events := store.History(10)
			for j := range events {
				events[j].Message = "changed"
			}
			stream, stop := store.Subscribe(uint64(i))
			select {
			case event := <-stream:
				if event.ID <= uint64(i) {
					t.Errorf("replayed %d at cursor %d", event.ID, i)
				}
			default:
			}
			stop()
			stop()
		}
	})
	done := make(chan struct{})
	go func() { work.Wait(); store.Close(); close(done) }()
	waitDone(t, done)
	waitDone(t, read)
	events := store.History(0)
	if len(events) != writers*perWriter {
		t.Fatalf("history size = %d", len(events))
	}
	for i, event := range events {
		if event.ID != uint64(i+1) || event.Message != "event" {
			t.Fatalf("history corrupted: %#v", event)
		}
	}
}

func TestStoreConcurrentCloseCancelAdd(t *testing.T) {
	store := NewStore(4)
	ch, cancel := store.Subscribe(0)
	start := make(chan struct{})
	var work sync.WaitGroup
	for range 4 {
		work.Go(func() {
			<-start
			for range 100 {
				store.Add(Event{})
			}
		})
		work.Go(func() { <-start; cancel(); store.Close() })
	}
	close(start)
	done := make(chan struct{})
	go func() { work.Wait(); close(done) }()
	waitDone(t, done)
	for range ch {
	}
}

func eventIDs(events []Event) []uint64 {
	var out []uint64
	for _, event := range events {
		out = append(out, event.ID)
	}
	return out
}

func receiveEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed before event")
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func assertEventsClosed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed subscription")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription not closed")
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent operation blocked")
	}
}
