package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/notify"
)

type fakeLogStore struct {
	history   func(uint64, int, slog.Level) ([]logging.Entry, uint64)
	subscribe func(uint64) (<-chan logging.Entry, func())
}

func (f *fakeLogStore) History(after uint64, limit int, level slog.Level) ([]logging.Entry, uint64) {
	if f.history == nil {
		return nil, after
	}
	return f.history(after, limit, level)
}

func (f *fakeLogStore) Subscribe(after uint64) (<-chan logging.Entry, func()) {
	return f.subscribe(after)
}

type fakeNotificationStore struct {
	history   func(int) []notify.Event
	subscribe func(uint64) (<-chan notify.Event, func())
}

func (f *fakeNotificationStore) History(limit int) []notify.Event {
	if f.history == nil {
		return nil
	}
	return f.history(limit)
}

func (f *fakeNotificationStore) Subscribe(after uint64) (<-chan notify.Event, func()) {
	return f.subscribe(after)
}

func TestLogHistory(t *testing.T) {
	for _, tc := range []struct {
		query string
		after uint64
		limit int
		level slog.Level
	}{
		{"", 0, 200, slog.LevelInfo},
		{"?after=42&limit=5&level=debug", 42, 5, slog.LevelDebug},
		{"?limit=5000&level=warn", 0, 1000, slog.LevelWarn},
		{"?limit=0&level=error", 0, 200, slog.LevelError},
		{"?limit=-1&level=info", 0, 200, slog.LevelInfo},
		{"?after=18446744073709551615", ^uint64(0), 200, slog.LevelInfo},
	} {
		t.Run(tc.query, func(t *testing.T) {
			want := []logging.Entry{{Seq: 48, Level: "error", Message: "retry failed"}}
			calls := 0
			store := &fakeLogStore{history: func(after uint64, limit int, level slog.Level) ([]logging.Entry, uint64) {
				calls++
				if after != tc.after || limit != tc.limit || level != tc.level {
					t.Errorf("History arguments = %d, %d, %s", after, limit, level)
				}
				return want, 50
			}}
			w := request(NewHandler(Options{Logs: store}), http.MethodGet, "/api/v1/logs"+tc.query, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var result struct {
				Entries []logging.Entry `json:"entries"`
				Next    uint64          `json:"next"`
			}
			decodeJSON(t, w, &result)
			if !reflect.DeepEqual(result.Entries, want) || result.Next != 50 || calls != 1 {
				t.Fatalf("history response = %+v, calls = %d", result, calls)
			}
		})
	}
	w := request(NewHandler(Options{Logs: &fakeLogStore{}}), http.MethodGet, "/api/v1/logs?after=3", "")
	if w.Body.String() != "{\"entries\":[],\"next\":3}\n" {
		t.Errorf("empty history = %s", w.Body.String())
	}
}

func TestNotificationHistory(t *testing.T) {
	for _, tc := range []struct {
		query string
		limit int
	}{{"", 200}, {"?limit=3", 3}, {"?limit=1001", 1000}} {
		want := []notify.Event{{ID: 7, Kind: "config_applied", Severity: "info", Message: "saved"}}
		store := &fakeNotificationStore{history: func(limit int) []notify.Event {
			if limit != tc.limit {
				t.Errorf("limit = %d, want %d", limit, tc.limit)
			}
			return want
		}}
		w := request(NewHandler(Options{Notifications: store}), http.MethodGet, "/api/v1/notifications"+tc.query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		var result struct {
			Events []notify.Event `json:"events"`
		}
		decodeJSON(t, w, &result)
		if !reflect.DeepEqual(result.Events, want) {
			t.Errorf("events = %+v", result.Events)
		}
	}
	w := request(NewHandler(Options{Notifications: &fakeNotificationStore{}}), http.MethodGet, "/api/v1/notifications", "")
	if w.Body.String() != "{\"events\":[]}\n" {
		t.Errorf("empty events = %s", w.Body.String())
	}
}

func TestInvalidHistoryAndStreamQueries(t *testing.T) {
	// Nil dependencies expose accidental parsing fallthrough as a 503, not 400.
	handler := NewHandler(Options{})
	for _, path := range []string{
		"/api/v1/logs?after=-1", "/api/v1/logs?after=18446744073709551616",
		"/api/v1/logs?limit=bad", "/api/v1/logs?limit=999999999999999999999", "/api/v1/logs?level=trace",
		"/api/v1/logs/stream?after=bad", "/api/v1/notifications?limit=bad", "/api/v1/notifications/stream?after=-1",
	} {
		assertJSONError(t, request(handler, http.MethodGet, path, ""), http.StatusBadRequest)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	wroteHeader          bool
	deadlineAfterHeaders bool
	deadlines            []time.Time
	flushes              int
}

func (w *deadlineRecorder) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseRecorder.WriteHeader(status)
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadlineAfterHeaders = w.wroteHeader
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineRecorder) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}

func assertStreamHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	for key, want := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "Connection": "keep-alive", "X-Accel-Buffering": "no"} {
		if got := headers.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

type receivedEvent struct {
	id, event string
	data      json.RawMessage
}

func readEvent(t *testing.T, reader *bufio.Reader) receivedEvent {
	t.Helper()
	var event receivedEvent
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return event
		}
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("malformed SSE line %q", line)
		}
		switch key {
		case "id":
			event.id = value
		case "event":
			event.event = value
		case "data":
			event.data = json.RawMessage(value)
		default:
			t.Fatalf("unknown SSE field %q", key)
		}
	}
}

func TestStreamsReplayAndClose(t *testing.T) {
	for _, kind := range []string{"log", "notification"} {
		t.Run(kind, func(t *testing.T) {
			canceled := 0
			cancel := func() { canceled++ }
			var opts Options
			var path string
			var want any
			if kind == "log" {
				entry := logging.Entry{Seq: 9, Level: "debug", Message: "first\nsecond", Attrs: map[string]any{"retry": true}}
				ch := make(chan logging.Entry, 1)
				ch <- entry
				close(ch)
				opts.Logs = &fakeLogStore{subscribe: func(after uint64) (<-chan logging.Entry, func()) {
					if after != 8 {
						t.Errorf("after = %d", after)
					}
					return ch, cancel
				}}
				path, want = "/api/v1/logs/stream?after=8", entry
			} else {
				event := notify.Event{ID: 9, Kind: "item_recovered", Severity: "info", Message: "back\nonline"}
				ch := make(chan notify.Event, 1)
				ch <- event
				close(ch)
				opts.Notifications = &fakeNotificationStore{subscribe: func(after uint64) (<-chan notify.Event, func()) {
					if after != 8 {
						t.Errorf("after = %d", after)
					}
					return ch, cancel
				}}
				path, want = "/api/v1/notifications/stream?after=8", event
			}
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			NewHandler(opts).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			assertStreamHeaders(t, w.Header())
			if w.Code != http.StatusOK || canceled != 1 || w.flushes != 2 || !w.deadlineAfterHeaders || len(w.deadlines) < 2 {
				t.Fatalf("stream lifecycle: code=%d cancels=%d flushes=%d deadlines=%v afterHeaders=%t", w.Code, canceled, w.flushes, w.deadlines, w.deadlineAfterHeaders)
			}
			now := time.Now()
			for _, deadline := range w.deadlines {
				if deadline.IsZero() || deadline.Before(now) || deadline.After(now.Add(streamWriteTimeout+5*time.Second)) {
					t.Fatalf("unbounded stream write deadline: %v", deadline)
				}
			}
			reader := bufio.NewReader(w.Body)
			got := readEvent(t, reader)
			encoded, _ := json.Marshal(want)
			if got.id != "9" || got.event != kind || string(got.data) != string(encoded) {
				t.Errorf("event = %+v, want %s", got, encoded)
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Error("unexpected duplicate replay data")
			}
		})
	}
}

func TestStreamsLiveAndClientCancellation(t *testing.T) {
	for _, kind := range []string{"log", "notification"} {
		t.Run(kind, func(t *testing.T) {
			unsubscribed := make(chan struct{})
			var opts Options
			var path string
			var publish func(uint64)
			if kind == "log" {
				ch := make(chan logging.Entry, 2)
				publish = func(id uint64) { ch <- logging.Entry{Seq: id, Level: "info", Message: "entry"} }
				opts.Logs = &fakeLogStore{subscribe: func(after uint64) (<-chan logging.Entry, func()) { return ch, func() { close(unsubscribed) } }}
				path = "/api/v1/logs/stream"
			} else {
				ch := make(chan notify.Event, 2)
				publish = func(id uint64) { ch <- notify.Event{ID: id, Kind: "config_applied", Message: "event"} }
				opts.Notifications = &fakeNotificationStore{subscribe: func(after uint64) (<-chan notify.Event, func()) { return ch, func() { close(unsubscribed) } }}
				path = "/api/v1/notifications/stream"
			}
			publish(1)
			server := httptest.NewServer(NewHandler(opts))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			assertStreamHeaders(t, response.Header)
			reader := bufio.NewReader(response.Body)
			for id := uint64(1); id <= 2; id++ {
				got := readEvent(t, reader)
				if got.event != kind || got.id != strconv.FormatUint(id, 10) || !json.Valid(got.data) {
					t.Fatalf("event = %+v", got)
				}
				if id == 1 {
					publish(2)
				}
			}
			cancel()
			waitSignal(t, unsubscribed)
		})
	}
}

func TestStreamHeartbeatAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan logging.Entry)
		canceled := false
		store := &fakeLogStore{subscribe: func(uint64) (<-chan logging.Entry, func()) { return ch, func() { canceled = true } }}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		go NewHandler(Options{Logs: store}).ServeHTTP(w, r)
		synctest.Wait()
		time.Sleep(15 * time.Second) // Virtual time; no wall-clock sleep.
		synctest.Wait()
		cancel()
		synctest.Wait()
		if w.Body.String() != ": ping\n\n" || !w.Flushed || !canceled {
			t.Fatalf("heartbeat=%q flushed=%t canceled=%t", w.Body.String(), w.Flushed, canceled)
		}
	})
}

func TestStreamHeadReturnsImmediately(t *testing.T) {
	ch := make(chan logging.Entry)
	canceled := false
	store := &fakeLogStore{subscribe: func(uint64) (<-chan logging.Entry, func()) { return ch, func() { canceled = true } }}
	w := request(NewHandler(Options{Logs: store}), http.MethodHead, "/api/v1/logs/stream", "")
	assertStreamHeaders(t, w.Header())
	if w.Body.Len() != 0 || !canceled {
		t.Fatal("HEAD did not release subscription")
	}
}

func TestServeStopsStreams(t *testing.T) {
	for _, listenerFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(listenerFailure), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := config.APIConfig{Listen: "127.0.0.1:0", Token: "server-token"}
			listener, err := Listen(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ch := make(chan logging.Entry)
			unsubscribed := make(chan struct{})
			store := &fakeLogStore{subscribe: func(uint64) (<-chan logging.Entry, func()) { return ch, func() { close(unsubscribed) } }}
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, cfg, Options{Logs: store}, listener) }()
			client := &http.Client{Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			r, _ := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/api/v1/logs/stream", nil)
			r.Header.Set("Authorization", "Bearer server-token")
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if listenerFailure {
				_ = listener.Close()
			} else {
				cancel()
			}
			waitSignal(t, unsubscribed)
			select {
			case err := <-done:
				if listenerFailure && !errors.Is(err, net.ErrClosed) {
					t.Fatalf("error = %v", err)
				}
				if !listenerFailure && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server did not stop its stream")
			}
		})
	}
}
