package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/service"
)

type fakeSource struct {
	snapshot       service.Snapshot
	ready          bool
	accepted       map[string]bool
	triggered      []string
	snapshotCalls  int
	beforeSnapshot func()
}

func (s *fakeSource) Snapshot() service.Snapshot {
	s.snapshotCalls++
	if s.beforeSnapshot != nil {
		s.beforeSnapshot()
	}
	return s.snapshot
}

func (s *fakeSource) Ready() bool { return s.ready }

func (s *fakeSource) TriggerItem(name string) bool {
	s.triggered = append(s.triggered, name)
	return s.accepted[name]
}

func request(handler http.Handler, method, path, authorization string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, value any) {
	t.Helper()
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
	if err := json.Unmarshal(w.Body.Bytes(), value); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready=%t", ready), func(t *testing.T) {
			handler := NewHandler(Options{Source: &fakeSource{ready: ready}, Token: "api-secret"})
			for _, path := range []string{"/healthz", "/readyz"} {
				w := request(handler, http.MethodGet, path, "")
				wantCode, key, wantValue := http.StatusOK, "ok", true
				if path == "/readyz" {
					key, wantValue = "ready", ready
					if !ready {
						wantCode = http.StatusServiceUnavailable
					}
				}
				if w.Code != wantCode {
					t.Errorf("%s: status = %d, want %d", path, w.Code, wantCode)
				}
				var body map[string]bool
				decodeJSON(t, w, &body)
				if got, ok := body[key]; !ok || got != wantValue {
					t.Errorf("%s: body = %v, want %s=%t", path, body, key, wantValue)
				}
			}
		})
	}
}

func TestStatusRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	want := service.Snapshot{
		ConfigPath: "config.yaml", StartedAt: now,
		Items: []service.ItemStatus{{
			Name: "home-v6", Family: "ipv6", Interface: "wan", Phase: "degraded",
			SourceAddress: "2001:db8::1/64", RecordValue: "2001:db8::1", EUI64MAC: "00:1a:2b:3c:4d:5e",
			Domains: []service.DomainStatus{{
				Provider: "ali-main", FQDN: "home.example.com", Type: "AAAA", Value: "2001:db8::1",
				RecordID: "record-1", TTL: 600, Line: "default", UpdatedAt: now, LastError: "temporary failure",
			}},
			AddressList: &service.AddressListStatus{List: "ddns-home", Entries: []string{"2001:db8::1"}, UpdatedAt: now},
			LastError:   "temporary failure", SourceObservedAt: now, LastSyncAt: now, NextPollAt: now.Add(time.Minute),
		}, {
			Name: "disabled", Disabled: true, Phase: "disabled", Domains: []service.DomainStatus{},
		}},
	}
	source := &fakeSource{snapshot: want}
	w := request(NewHandler(Options{Source: source}), http.MethodGet, "/api/v1/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got service.Snapshot
	decodeJSON(t, w, &got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot = %+v, want %+v", got, want)
	}
	if source.snapshotCalls != 1 {
		t.Errorf("Snapshot calls = %d, want 1", source.snapshotCalls)
	}
}

func TestSync(t *testing.T) {
	source := &fakeSource{accepted: map[string]bool{"home-ipv4": true, "disabled": false}}
	handler := NewHandler(Options{Source: source})
	for _, name := range []string{"home-ipv4", "missing", "disabled"} {
		t.Run(name, func(t *testing.T) {
			w := request(handler, http.MethodPost, "/api/v1/items/"+name+"/sync", "")
			want := http.StatusNotFound
			if name == "home-ipv4" {
				want = http.StatusAccepted
			}
			if w.Code != want {
				t.Errorf("status = %d, want %d", w.Code, want)
			}
			var body map[string]string
			decodeJSON(t, w, &body)
			if want == http.StatusAccepted {
				if body["status"] != "accepted" || body["name"] != name {
					t.Errorf("body = %v, want accepted item %q", body, name)
				}
			} else if body["error"] != "item not found" {
				t.Errorf("body = %v, want item not found", body)
			}
		})
	}
	if want := []string{"home-ipv4", "missing", "disabled"}; !reflect.DeepEqual(source.triggered, want) {
		t.Errorf("TriggerItem names = %v, want %v", source.triggered, want)
	}
}

func TestBearerAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, authorization string
		allowed             bool
	}{
		{"missing", "", false},
		{"wrong", "Bearer api-secreu", false},
		{"short", "Bearer x", false},
		{"long", "Bearer api-secret-extra", false},
		{"wrong scheme", "Basic api-secret", false},
		{"empty", "Bearer ", false},
		{"extra space", "Bearer  api-secret", false},
		{"correct", "Bearer api-secret", true},
		{"case insensitive scheme", "bearer api-secret", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []struct{ method, path string }{
				{http.MethodGet, "/api/v1/status"},
				{http.MethodPost, "/api/v1/items/home/sync"},
			} {
				source := &fakeSource{accepted: map[string]bool{"home": true}}
				w := request(NewHandler(Options{Source: source, Token: "api-secret"}), route.method, route.path, tc.authorization)
				if tc.allowed {
					want := http.StatusOK
					if route.method == http.MethodPost {
						want = http.StatusAccepted
					}
					if w.Code != want {
						t.Errorf("%s: status = %d, want %d", route.path, w.Code, want)
					}
				} else {
					assertUnauthorized(t, w)
					if source.snapshotCalls != 0 || len(source.triggered) != 0 {
						t.Error("unauthorized request reached the service")
					}
				}
			}
		})
	}
}

func assertUnauthorized(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
	var body map[string]string
	decodeJSON(t, w, &body)
	if !reflect.DeepEqual(body, map[string]string{"error": "unauthorized"}) {
		t.Errorf("401 JSON = %v, want only an unauthorized error", body)
	}
}

func TestAuthenticationNamespace(t *testing.T) {
	source := &fakeSource{}
	handler := NewHandler(Options{Source: source, Token: "api-secret"})
	for _, path := range []string{"/api/v1", "/api/v1/unknown", "/api/v1/status"} {
		assertUnauthorized(t, request(handler, http.MethodPut, path, ""))
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	r.Header.Add("Authorization", "Bearer api-secret")
	r.Header.Add("Authorization", "Bearer api-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assertUnauthorized(t, w)
	if source.snapshotCalls != 0 {
		t.Error("duplicate Authorization headers reached the service")
	}
	// Without a configured token, even an irrelevant Authorization header is ignored.
	w = request(NewHandler(Options{Source: source}), http.MethodGet, "/api/v1/status", "Basic ignored")
	if w.Code != http.StatusOK {
		t.Errorf("unauthenticated configuration: status = %d, want 200", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	source := &fakeSource{}
	handler := NewHandler(Options{Source: source, Token: "api-secret"})
	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodPost, "/healthz", "GET, HEAD"},
		{http.MethodPut, "/readyz", "GET, HEAD"},
		{http.MethodPost, "/api/v1/status", "GET, HEAD"},
		{http.MethodGet, "/api/v1/items/home/sync", "POST"},
		{http.MethodDelete, "/api/v1/config", "GET, HEAD, PUT"},
		{http.MethodGet, "/api/v1/config/validate", "POST"},
		{http.MethodPost, "/api/v1/meta", "GET, HEAD"},
		{http.MethodPost, "/api/v1/logs", "GET, HEAD"},
		{http.MethodPost, "/api/v1/logs/stream", "GET, HEAD"},
		{http.MethodPost, "/api/v1/notifications", "GET, HEAD"},
		{http.MethodPost, "/api/v1/notifications/stream", "GET, HEAD"},
		{http.MethodPost, "/", "GET, HEAD"},
		{http.MethodPost, "/assets/main.js", "GET, HEAD"},
	} {
		w := request(handler, tc.method, tc.path, "Bearer api-secret")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: status/Allow = %d/%q, want 405/%q", tc.method, tc.path, w.Code, w.Header().Get("Allow"), tc.allow)
		}
		var body map[string]string
		decodeJSON(t, w, &body)
		if body["error"] == "" {
			t.Error("missing JSON method error")
		}
	}
	if source.snapshotCalls != 0 || len(source.triggered) != 0 {
		t.Error("unsupported method reached the service")
	}
}

type closeNotifyingListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *closeNotifyingListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestServeGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config.APIConfig{Listen: "127.0.0.1:0"}
	listener, err := Listen(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	l := &closeNotifyingListener{Listener: listener, closed: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	releaseRequest := sync.OnceFunc(func() { close(release) })
	defer releaseRequest()
	source := &fakeSource{beforeSnapshot: func() {
		close(entered)
		<-release
	}}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, Options{Source: source}, l) }()
	responseDone := make(chan error, 1)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	go func() {
		response, err := client.Get("http://" + listener.Addr().String() + "/api/v1/status")
		if err == nil {
			defer response.Body.Close()
			_, err = io.Copy(io.Discard, response.Body)
			if err == nil && response.StatusCode != http.StatusOK {
				err = fmt.Errorf("status = %d, want 200", response.StatusCode)
			}
		}
		responseDone <- err
	}()
	waitSignal(t, entered)
	cancel()
	waitSignal(t, l.closed)
	select {
	case err := <-done:
		t.Fatalf("server returned before the in-flight request finished: %v", err)
	default:
	}
	releaseRequest()
	for _, result := range []<-chan error{responseDone, done} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for graceful shutdown")
		}
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server event")
	}
}

func TestRunListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = Run(ctx, config.APIConfig{Listen: listener.Addr().String()}, Options{Source: &fakeSource{}})
	if err == nil || !strings.Contains(err.Error(), "listen HTTP API") {
		t.Errorf("Run error = %v, want listener failure", err)
	}
}

func TestRunCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, config.APIConfig{Listen: "invalid"}, Options{Source: &fakeSource{}}); err != nil {
		t.Errorf("Run after cancellation = %v, want nil", err)
	}
}

func TestListenBindsSynchronously(t *testing.T) {
	listener, err := Listen(context.Background(), config.APIConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	other, err := Listen(context.Background(), config.APIConfig{Listen: listener.Addr().String()})
	if err == nil {
		other.Close()
		t.Fatal("Listen returned before reserving the address")
	}
	if !strings.Contains(err.Error(), "listen HTTP API") {
		t.Errorf("Listen error = %v, want bind failure", err)
	}
}

func TestListenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	listener, err := Listen(ctx, config.APIConfig{Listen: "127.0.0.1:0"})
	if listener != nil {
		listener.Close()
		t.Fatal("Listen opened a listener after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Listen error = %v, want context.Canceled", err)
	}
}

func TestServeClosesListenerAfterCancellation(t *testing.T) {
	listener, err := Listen(context.Background(), config.APIConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, config.APIConfig{}, Options{Source: &fakeSource{}}, listener); err != nil {
		t.Fatalf("Serve after cancellation: %v", err)
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("listener still accepts connections after cancellation")
	}
}

func TestServeListenerError(t *testing.T) {
	listener, err := Listen(context.Background(), config.APIConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	err = Serve(context.Background(), config.APIConfig{}, Options{Source: &fakeSource{}}, listener)
	if !errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "serve HTTP API") {
		t.Errorf("Serve error = %v, want listener failure", err)
	}
}
