package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/notify"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/service"
	"ros-ddns/internal/state"
)

type fakeROS struct {
	mu      sync.Mutex
	polls   int
	pollErr error
	poll    func(context.Context) ([]ros.Address, error)
}

func (r *fakeROS) Addresses(ctx context.Context, _ ros.Family) ([]ros.Address, error) {
	r.mu.Lock()
	r.polls++
	fn, err := r.poll, r.pollErr
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return []ros.Address{{Interface: "wan", Prefix: netip.MustParsePrefix("203.0.113.10/24")}}, err
}

func (*fakeROS) ListEntries(context.Context, ros.Family, string) ([]ros.ListEntry, error) {
	return nil, nil
}
func (*fakeROS) AddEntry(context.Context, ros.Family, ros.ListEntry) error { return nil }
func (*fakeROS) RemoveEntry(context.Context, ros.Family, string) error     { return nil }

type fakeProvider struct {
	mu      sync.Mutex
	ensure  func(context.Context, dns.Record) error
	err     error
	records []dns.Record
}

func (p *fakeProvider) Ensure(ctx context.Context, record dns.Record, _ string) (dns.EnsureResult, error) {
	p.mu.Lock()
	p.records = append(p.records, record)
	err, fn := p.err, p.ensure
	p.mu.Unlock()
	if fn != nil {
		err = fn(ctx, record)
	}
	return dns.EnsureResult{RecordID: "fake-id", Changed: true}, err
}

type fixture struct {
	m        *Manager
	cfg      *config.Config
	raw      []byte
	router   *fakeROS
	provider *fakeProvider
}

func newFixture(t *testing.T, opts Options) fixture {
	t.Helper()
	t.Setenv("APP_TEST_PASSWORD", "initial-router-secret")
	t.Setenv("APP_TEST_TOKEN", "initial-api-token")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte(fmt.Sprintf(`# original comments and formatting
log: {level: info, format: text}
runtime:
  pollInterval: 1h
  maxBackoff: 2h
  reconcileTimeout: 1s
  verifyInterval: 0s
state: {path: %q}
api: {listen: "127.0.0.1:8080", token: "${APP_TEST_TOKEN}"}
routeros:
  url: https://router.example
  username: ddns
  password: ${APP_TEST_PASSWORD}
dns:
  providers:
    test:
      type: aliyun
      accessKeyId: access-key-id
      accessKeySecret: access-key-secret
items:
  - name: home
    source: {interface: wan, family: ipv4, select: {scope: any}}
    domains:
      - {provider: test, domain: example.com, rr: home, ttl: 600}
`, filepath.Join(dir, "state.json")))
	if err := os.WriteFile(path, raw, 0640); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	router, provider := &fakeROS{}, &fakeProvider{}
	deps := service.Deps{
		NewROS:      func(config.RouterOSConfig) (ros.Client, error) { return router, nil },
		NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) { return provider, nil },
	}
	m, err := NewManager(cfg, deps, opts)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{m: m, cfg: cfg, raw: raw, router: router, provider: provider}
}

func editDocument(t *testing.T, m *Manager, edit func(map[string]any)) (string, []byte) {
	t.Helper()
	doc, err := m.ConfigDocument()
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(doc.Data)
	}
	encoded, err := json.Marshal(doc.Data)
	if err != nil {
		t.Fatal(err)
	}
	return doc.Hash, encoded
}

func renameItem(data map[string]any) {
	item := data["items"].([]any)[0].(map[string]any)
	item["name"] = "replacement"
	item["domains"].([]any)[0].(map[string]any)["rr"] = "replacement"
}

func await(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for !check() {
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for condition")
		case <-tick.C:
		}
	}
}

type runningManager struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startManager(t *testing.T, m *Manager) *runningManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	running := &runningManager{cancel: cancel, done: make(chan struct{})}
	go func() {
		running.err = m.Run(ctx)
		close(running.done)
	}()
	t.Cleanup(func() { running.stop(t) })
	await(t, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.run != nil || m.finished
	})
	return running
}

func (r *runningManager) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
		if r.err != nil {
			t.Errorf("Run returned an error after cancellation: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not join its workers/monitor")
	}
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file %s differs from expected bytes (error %v)", path, err)
	}
}

func TestApplyConfigChangesServiceRuntimeStateAndLogging(t *testing.T) {
	level := &slog.LevelVar{}
	hub := logging.NewHub(30)
	store := notify.NewStore(30)
	f := newFixture(t, Options{Version: "w2-test", LogHub: hub, LogLevel: level, Notifications: store})
	startManager(t, f.m)
	await(t, func() bool { return f.m.Snapshot().Items[0].Phase == "synced" })
	before := f.m.Meta()
	oldMode, _ := os.Stat(f.cfg.Path)
	newState := filepath.Join(filepath.Dir(f.cfg.Path), "new-state.json")
	t.Setenv("APP_TEST_NEXT_PASSWORD", "new-router-password")
	hash, data := editDocument(t, f.m, func(data map[string]any) {
		renameItem(data)
		data["runtime"].(map[string]any)["pollInterval"] = "20ms"
		data["state"].(map[string]any)["path"] = newState
		data["log"].(map[string]any)["level"] = "debug"
		data["routeros"].(map[string]any)["password"] = "${APP_TEST_NEXT_PASSWORD}"
	})
	snapshot, warnings, err := f.m.ApplyConfig(context.Background(), hash, data)
	if err != nil || len(warnings) != 0 || snapshot.Items[0].Name != "replacement" {
		t.Fatalf("apply: snapshot=%+v warnings=%v err=%v", snapshot, warnings, err)
	}
	await(t, func() bool {
		item := f.m.Snapshot().Items[0]
		return item.Phase == "synced" && item.NextPollAt.Sub(item.SourceObservedAt) == 20*time.Millisecond
	})
	if !f.m.Ready() || f.m.TriggerItem("home") || !f.m.TriggerItem("replacement") {
		t.Fatal("delegation still uses the old service")
	}
	await(t, func() bool { _, err := os.Stat(newState); return err == nil })
	cache, err := state.Open(newState)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get("replacement"); !ok {
		t.Fatal("state.path change was ignored")
	}
	_, yamlBytes, err := config.ValidateStructured(data, f.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, f.cfg.Path, yamlBytes)
	assertFile(t, f.cfg.Path+".bak", f.raw)
	for _, path := range []string{f.cfg.Path, f.cfg.Path + ".bak"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != oldMode.Mode().Perm() {
			t.Fatalf("permissions changed: %s, %v", path, err)
		}
	}
	meta := f.m.Meta()
	if meta.Version != "w2-test" || meta.ConfigPath != f.cfg.Path || meta.StartedAt != before.StartedAt || meta.Uptime < before.Uptime || meta.LogLevel != "debug" || level.Level() != slog.LevelDebug {
		t.Fatalf("incorrect metadata/log level: %+v", meta)
	}
	logger := slog.New(logging.NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: level}), hub, level))
	logger.Debug("new-router-password", "credential", "new-router-password")
	history, _ := hub.History(0, 30, slog.LevelDebug)
	if len(history) != 1 || history[0].Message != "[redacted]" || history[0].Attrs["credential"] != "[redacted]" {
		t.Fatalf("new secrets are not redacted: %+v", history)
	}
	doc, err := f.m.ConfigDocument()
	if err != nil || doc.Hash == hash || doc.Data["routeros"].(map[string]any)["password"] != "${APP_TEST_NEXT_PASSWORD}" {
		t.Fatalf("document did not retain new environment reference: %+v, %v", doc, err)
	}
}

func TestApplyConfigRejectsEditsWithoutChangingServiceOrFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		hash string
		want error
	}{
		{name: "empty hash", hash: "empty", want: ErrConfigConflict},
		{name: "wrong hash", hash: "wrong", want: ErrConfigConflict},
		{name: "unknown field", edit: func(d map[string]any) { d["unknown"] = true }},
		{name: "invalid runtime", edit: func(d map[string]any) { d["runtime"].(map[string]any)["pollInterval"] = "0s" }},
		{name: "api listen", want: ErrRestartRequired, edit: func(d map[string]any) { d["api"].(map[string]any)["listen"] = "127.0.0.1:9090" }},
		{name: "api token", want: ErrRestartRequired, edit: func(d map[string]any) { d["api"].(map[string]any)["token"] = "replacement-api-token" }},
		{name: "log format", want: ErrRestartRequired, edit: func(d map[string]any) { d["log"].(map[string]any)["format"] = "json" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			startManager(t, f.m)
			await(t, f.m.Ready)
			original := f.m.svc
			hash, data := editDocument(t, f.m, tc.edit)
			if tc.hash == "empty" {
				hash = ""
			} else if tc.hash != "" {
				hash = tc.hash
			}
			snapshot, warnings, err := f.m.ApplyConfig(context.Background(), hash, data)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("got %v, expected %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "replacement-api-token") || len(warnings) != 0 || snapshot.Items[0].Name != "home" || f.m.svc != original || !f.m.Ready() || !f.m.TriggerItem("home") {
				t.Fatal("rejected edit changed the service, readiness or exposed a token")
			}
			assertFile(t, f.cfg.Path, f.raw)
			if _, err := os.Stat(f.cfg.Path + ".bak"); !os.IsNotExist(err) {
				t.Fatal("rejected edit touched the backup")
			}
		})
	}
}

func TestCandidateConstructionFailureKeepsOldService(t *testing.T) {
	for _, factory := range []string{"router", "provider", "state"} {
		t.Run(factory, func(t *testing.T) {
			store := notify.NewStore(20)
			f := newFixture(t, Options{Notifications: store})
			if factory == "router" {
				f.m.deps.NewROS = func(cfg config.RouterOSConfig) (ros.Client, error) {
					return nil, errors.New("cannot connect using " + cfg.Password)
				}
			} else if factory == "provider" {
				f.m.deps.NewProvider = func(_ string, cfg config.ProviderConfig) (dns.Provider, error) {
					return nil, errors.New("cannot construct using " + cfg.AccessKeySecret)
				}
			}
			startManager(t, f.m)
			await(t, f.m.Ready)
			old := f.m.svc
			hash, data := editDocument(t, f.m, func(d map[string]any) {
				renameItem(d)
				if factory == "state" {
					d["state"].(map[string]any)["path"] = t.TempDir()
				}
			})
			_, _, err := f.m.ApplyConfig(context.Background(), hash, data)
			if err == nil || strings.Contains(err.Error(), "initial-router-secret") || strings.Contains(err.Error(), "access-key-secret") || f.m.svc != old || !f.m.Ready() {
				t.Fatalf("unsafe or ineffective construction failure: %v", err)
			}
			assertFile(t, f.cfg.Path, f.raw)
			events := store.History(0)
			if len(events) != 1 || events[0].Kind != "config_error" {
				t.Fatalf("missing safe failure notification: %+v", events)
			}
		})
	}
}

func TestConfigDocumentValidationAndDormantApply(t *testing.T) {
	f := newFixture(t, Options{})
	if f.router.polls != 0 || len(f.provider.records) != 0 {
		t.Fatal("constructor started work before Run")
	}
	document, err := f.m.ConfigDocument()
	if err != nil || len(document.Hash) != 64 || document.Data["routeros"].(map[string]any)["password"] != "${APP_TEST_PASSWORD}" || document.Data["api"].(map[string]any)["token"] != "${APP_TEST_TOKEN}" {
		t.Fatalf("raw config document: %+v, %v", document, err)
	}
	hash, data := editDocument(t, f.m, renameItem)
	if err := f.m.ValidateConfig(data); err != nil {
		t.Fatal(err)
	}
	if err := f.m.ValidateConfig([]byte("{}")); err != nil {
		t.Fatalf("empty configuration rejected: %v", err)
	}
	for _, invalid := range []string{"[]", `{"runtime":{"pollInterval":"0s"}}`, `{"typo":true}`, "{} {}"} {
		if err := f.m.ValidateConfig([]byte(invalid)); err == nil {
			t.Fatalf("ValidateConfig accepted %s", invalid)
		}
	}
	assertFile(t, f.cfg.Path, f.raw)
	if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err != nil {
		t.Fatal(err)
	}
	if f.router.polls != 0 || len(f.provider.records) != 0 || f.m.Snapshot().Items[0].Name != "replacement" {
		t.Fatal("dormant apply started workers or did not install the candidate")
	}
	startManager(t, f.m)
	await(t, func() bool { return f.m.Snapshot().Items[0].Phase == "synced" })
}

func TestManagerLifecycleAndConfigOwnership(t *testing.T) {
	if _, err := NewManager(nil, service.Deps{}, Options{}); err == nil {
		t.Fatal("nil config accepted")
	}
	f := newFixture(t, Options{})
	deps := f.m.deps
	deps.NewROS = func(config.RouterOSConfig) (ros.Client, error) { return nil, errors.New("factory failed") }
	if _, err := NewManager(f.cfg, deps, Options{}); err == nil {
		t.Fatal("initial construction failure ignored")
	}
	f.cfg.Items[0].Name = "caller-mutated"
	f.cfg.API.Token = "caller-mutated"
	if f.m.Snapshot().Items[0].Name != "home" || f.m.cfg.API.Token == "caller-mutated" {
		t.Fatal("manager retained mutable caller-owned configuration")
	}
	running := startManager(t, f.m)
	await(t, f.m.Ready)
	if err := f.m.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("overlapping Run not rejected: %v", err)
	}
	running.stop(t)
	if f.m.Ready() || f.m.TriggerItem("home") {
		t.Fatal("stopped manager still accepts work")
	}
	if err := f.m.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "new manager") {
		t.Fatalf("reuse not explicitly rejected: %v", err)
	}
	hash, data := editDocument(t, f.m, nil)
	if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err == nil {
		t.Fatal("apply after shutdown accepted")
	}
}

func TestStatePathChangeReleasesInjectedStore(t *testing.T) {
	f := newFixture(t, Options{})
	deps := f.m.deps
	deps.State = &state.Store{}
	m, err := NewManager(f.cfg, deps, Options{})
	if err != nil {
		t.Fatal(err)
	}
	startManager(t, m)
	await(t, func() bool { _, ok := deps.State.Get("home"); return ok })
	newPath := filepath.Join(filepath.Dir(f.cfg.Path), "changed-state.json")
	hash, data := editDocument(t, m, func(d map[string]any) {
		renameItem(d)
		d["state"].(map[string]any)["path"] = newPath
	})
	if _, _, err := m.ApplyConfig(context.Background(), hash, data); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { _, err := os.Stat(newPath); return err == nil })
	if _, ok := deps.State.Get("replacement"); ok {
		t.Fatal("new service still writes to the injected old state store")
	}
}

func TestLogLevels(t *testing.T) {
	f := newFixture(t, Options{LogLevel: &slog.LevelVar{}})
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelWarn, slog.LevelError, slog.LevelInfo} {
		hash, data := editDocument(t, f.m, func(d map[string]any) {
			d["log"].(map[string]any)["level"] = level.String()
		})
		if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err != nil {
			t.Fatal(err)
		}
		if f.m.opts.LogLevel.Level() != level || f.m.Meta().LogLevel != strings.ToLower(level.String()) {
			t.Fatalf("level update failed for %s", level)
		}
	}
	_, data := editDocument(t, f.m, nil)
	before := f.m.Snapshot()
	if err := f.m.ValidateConfig(data); err != nil || !reflect.DeepEqual(before, f.m.Snapshot()) {
		t.Fatal("validation changed runtime state")
	}
}
