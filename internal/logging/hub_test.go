package logging

import (
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"ros-ddns/internal/redact"
)

func TestHubRingHistoryFiltersAndCursor(t *testing.T) {
	hub := NewHub(4)
	defer hub.Close()
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError, slog.LevelInfo, slog.LevelWarn}
	for _, level := range levels {
		hub.publish(Entry{Message: "entry"}, level)
	}
	for _, tt := range []struct {
		name   string
		after  uint64
		limit  int
		level  slog.Level
		seqs   []uint64
		cursor uint64
	}{
		{"overflow", 0, 10, slog.LevelDebug, []uint64{3, 4, 5, 6}, 6},
		{"stale", 1, 10, slog.LevelDebug, []uint64{3, 4, 5, 6}, 6},
		{"after", 4, 10, slog.LevelDebug, []uint64{5, 6}, 6},
		{"limit", 0, 2, slog.LevelDebug, []uint64{3, 4}, 4},
		{"warn", 0, 10, slog.LevelWarn, []uint64{3, 4, 6}, 6},
		{"filtered page", 3, 1, slog.LevelWarn, []uint64{4}, 4},
		{"next filtered page", 4, 1, slog.LevelWarn, []uint64{6}, 6},
		{"filtered tail", 4, 10, slog.LevelError, nil, 6},
		{"latest", 6, 10, slog.LevelDebug, nil, 6},
		{"future", 20, 10, slog.LevelDebug, nil, 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entries, cursor := hub.History(tt.after, tt.limit, tt.level)
			if !reflect.DeepEqual(entrySeqs(entries), tt.seqs) || cursor != tt.cursor {
				t.Fatalf("History = %v, %d; want %v, %d", entrySeqs(entries), cursor, tt.seqs, tt.cursor)
			}
		})
	}
}

func TestHubDefaultsAndHistoryLimits(t *testing.T) {
	for _, capacity := range []int{0, -5} {
		hub := NewHub(capacity)
		for range 2005 {
			hub.publish(Entry{}, slog.LevelInfo)
		}
		for _, limit := range []int{0, -1, 1500} {
			entries, cursor := hub.History(0, limit, slog.LevelDebug)
			want := 200
			if limit > 1000 {
				want = 1000
			}
			if len(entries) != want || entries[0].Seq != 6 || cursor != uint64(want+5) {
				t.Fatalf("capacity %d limit %d: len=%d, cursor=%d", capacity, limit, len(entries), cursor)
			}
		}
		ch, cancel := hub.Subscribe(0)
		cancel()
		var replay []Entry
		for e := range ch {
			replay = append(replay, e)
		}
		if len(replay) != 2000 || replay[0].Seq != 6 || replay[1999].Seq != 2005 {
			t.Fatalf("default ring size/replay = %d", len(replay))
		}
		hub.Close()
	}
}

func TestHubRetainsNumericLevels(t *testing.T) {
	hub := NewHub(4)
	defer hub.Close()
	for _, level := range []slog.Level{-8, 1, 5, 12} {
		hub.publish(Entry{}, level)
	}
	entries, _ := hub.History(0, 4, slog.Level(-8))
	for i, name := range []string{"debug", "info", "warn", "error"} {
		if entries[i].Level != name {
			t.Fatalf("level = %q, want %q", entries[i].Level, name)
		}
	}
	filtered, _ := hub.History(0, 4, slog.Level(5))
	if got := entrySeqs(filtered); !reflect.DeepEqual(got, []uint64{3, 4}) {
		t.Fatalf("custom minimum level = %v", got)
	}
}

func TestHubReplayLiveSlowCancelAndClose(t *testing.T) {
	hub := NewHub(3)
	for range 5 {
		hub.publish(Entry{}, slog.LevelInfo)
	}
	ch, cancel := hub.Subscribe(3)
	for _, seq := range []uint64{4, 5} {
		if e := receiveEntry(t, ch); e.Seq != seq {
			t.Fatalf("replay seq = %d, want %d", e.Seq, seq)
		}
	}
	hub.publish(Entry{}, slog.LevelInfo)
	if e := receiveEntry(t, ch); e.Seq != 6 {
		t.Fatalf("live seq = %d", e.Seq)
	}
	done := make(chan struct{})
	go func() {
		for range 5 {
			hub.publish(Entry{}, slog.LevelInfo)
		}
		close(done)
	}()
	awaitDone(t, done)
	cancel()
	cancel()
	var retained []Entry
	for e := range ch {
		retained = append(retained, e)
	}
	if got := entrySeqs(retained); !reflect.DeepEqual(got, []uint64{9, 10, 11}) {
		t.Fatalf("slow consumer did not keep newest entries: %v", got)
	}
	live, stop := hub.Subscribe(11)
	hub.Close()
	hub.Close()
	stop()
	assertEntriesClosed(t, live)
	hub.publish(Entry{}, slog.LevelInfo)
	entries, cursor := hub.History(0, 10, slog.LevelDebug)
	if len(entries) != 3 || cursor != 11 {
		t.Fatalf("close changed retained history: %#v, %d", entries, cursor)
	}
	closed, stop := hub.Subscribe(0)
	stop()
	assertEntriesClosed(t, closed)
}

func TestHubFutureSubscription(t *testing.T) {
	hub := NewHub(2)
	defer hub.Close()
	ch, cancel := hub.Subscribe(2)
	defer cancel()
	for range 3 {
		hub.publish(Entry{}, slog.LevelInfo)
	}
	if e := receiveEntry(t, ch); e.Seq != 3 {
		t.Fatalf("future subscription = %d", e.Seq)
	}
	select {
	case e := <-ch:
		t.Fatalf("unexpected entry: %#v", e)
	default:
	}
}

func TestHubAttributeCopiesAreIndependent(t *testing.T) {
	hub := NewHub(2)
	defer hub.Close()
	live, stopLive := hub.Subscribe(0)
	defer stopLive()
	input := map[string]any{"nested": map[string]any{"slice": []any{"original"}}}
	hub.publish(Entry{Attrs: input}, slog.LevelInfo)
	change := func(attrs map[string]any) { attrs["nested"].(map[string]any)["slice"].([]any)[0] = "changed" }
	change(input)
	replay, stopReplay := hub.Subscribe(0)
	defer stopReplay()
	change(receiveEntry(t, live).Attrs)
	got := receiveEntry(t, replay)
	if got.Attrs["nested"].(map[string]any)["slice"].([]any)[0] != "original" {
		t.Fatalf("subscriber maps alias: %#v", got)
	}
	change(got.Attrs)
	history, _ := hub.History(0, 1, slog.LevelDebug)
	change(history[0].Attrs)
	again, _ := hub.History(0, 1, slog.LevelDebug)
	if again[0].Attrs["nested"].(map[string]any)["slice"].([]any)[0] != "original" {
		t.Fatalf("history maps alias: %#v", again)
	}
}

func TestHubConcurrentWritersSubscribersAndRedactorUpdates(t *testing.T) {
	const writers, perWriter = 4, 200
	hub := NewHub(writers * perWriter)
	r1, r2 := redact.New([]string{"shared-secret"}), redact.New([]string{"shared-secret", "another-secret"})
	hub.SetRedactor(r1)
	logger := slog.New(NewHandler(slog.NewJSONHandler(io.Discard, nil), hub, nil)).With("secret", "shared-secret")
	ch, cancel := hub.Subscribe(0)
	defer cancel()
	read := make(chan struct{})
	go func() {
		defer close(read)
		var last uint64
		for e := range ch {
			if e.Seq != last+1 || e.Message != "[redacted]" || e.Attrs["secret"] != "[redacted]" {
				t.Errorf("concurrent entry = %#v after %d", e, last)
			}
			e.Attrs["secret"] = "reader mutation"
			last = e.Seq
		}
		if last != writers*perWriter {
			t.Errorf("received through %d, want %d", last, writers*perWriter)
		}
	}()
	var work sync.WaitGroup
	for range writers {
		work.Go(func() {
			for range perWriter {
				logger.Info("shared-secret")
			}
		})
	}
	work.Go(func() {
		for i := range 200 {
			if i%2 == 0 {
				hub.SetRedactor(r1)
			} else {
				hub.SetRedactor(r2)
			}
			entries, _ := hub.History(0, 5, slog.LevelInfo)
			for _, e := range entries {
				e.Attrs["secret"] = "history mutation"
			}
			stream, stop := hub.Subscribe(uint64(i))
			select {
			case e := <-stream:
				if e.Seq <= uint64(i) {
					t.Errorf("replayed %d at cursor %d", e.Seq, i)
				}
			default:
			}
			stop()
			stop()
		}
	})
	done := make(chan struct{})
	go func() { work.Wait(); hub.Close(); close(done) }()
	awaitDone(t, done)
	awaitDone(t, read)
	entries, cursor := hub.History(0, 1000, slog.LevelInfo)
	if len(entries) != writers*perWriter || cursor != writers*perWriter {
		t.Fatalf("concurrent history count = %d, cursor = %d", len(entries), cursor)
	}
	for i, e := range entries {
		if e.Seq != uint64(i+1) || e.Attrs["secret"] != "[redacted]" {
			t.Fatalf("history altered by consumer: %#v", e)
		}
	}
}

func TestHubConcurrentCloseCancelPublish(t *testing.T) {
	hub := NewHub(4)
	ch, cancel := hub.Subscribe(0)
	start := make(chan struct{})
	var work sync.WaitGroup
	for range 4 {
		work.Go(func() {
			<-start
			for range 100 {
				hub.publish(Entry{}, slog.LevelInfo)
			}
		})
		work.Go(func() { <-start; cancel(); hub.Close() })
	}
	close(start)
	done := make(chan struct{})
	go func() { work.Wait(); close(done) }()
	awaitDone(t, done)
	for range ch {
	}
}

func entrySeqs(entries []Entry) []uint64 {
	var out []uint64
	for _, e := range entries {
		out = append(out, e.Seq)
	}
	return out
}

func receiveEntry(t *testing.T, ch <-chan Entry) Entry {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed before entry")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for entry")
		return Entry{}
	}
}

func assertEntriesClosed(t *testing.T, ch <-chan Entry) {
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

func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent operation blocked")
	}
}
