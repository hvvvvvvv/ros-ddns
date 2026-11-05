package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/notify"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/service"
	"ros-ddns/internal/state"
)

func TestNewManagerStateFailureTolerance(t *testing.T) {
	for _, tolerant := range []bool{false, true} {
		t.Run(map[bool]string{false: "strict", true: "tolerant"}[tolerant], func(t *testing.T) {
			f := newFixture(t, Options{})
			cfg := cloneConfig(f.cfg)
			// Reading a directory fails on every supported platform, and putting
			// a configured secret in its name exercises diagnostic redaction.
			cfg.State.Path = filepath.Join(t.TempDir(), cfg.RouterOS.Password)
			if err := os.Mkdir(cfg.State.Path, 0700); err != nil {
				t.Fatal(err)
			}
			deps := f.m.deps
			deps.State, deps.Tolerant = nil, tolerant
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			m, err := NewManager(cfg, deps, Options{})
			if !tolerant {
				if err == nil || !strings.Contains(err.Error(), "open service state") || strings.Contains(err.Error(), cfg.RouterOS.Password) {
					t.Fatalf("strict state failure was ignored or not sanitized: %v", err)
				}
				if output.Len() != 0 {
					t.Fatalf("strict state failure emitted a fallback warning: %s", output.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.deps.State == nil || !m.deps.Tolerant {
				t.Fatal("tolerant startup did not retain an available state store")
			}
			if err := m.deps.State.Put("probe", state.Item{SourceAddress: "203.0.113.10"}); err != nil {
				t.Fatalf("fallback state still tries to persist to the unusable path: %v", err)
			}
			if cached, ok := m.deps.State.Get("probe"); !ok || cached.SourceAddress != "203.0.113.10" {
				t.Fatal("fallback state did not retain an in-memory update")
			}
			log := output.String()
			if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "in-memory cache") || !strings.Contains(log, "[redacted]") || strings.Contains(log, cfg.RouterOS.Password) {
				t.Fatalf("state fallback warning is absent or not sanitized: %s", log)
			}
		})
	}
}

func TestTolerantManagerApplyConstructionFailureKeepsOldService(t *testing.T) {
	for _, factory := range []string{"router", "provider", "state"} {
		t.Run(factory, func(t *testing.T) {
			f := newFixture(t, Options{})
			deps := f.m.deps
			deps.Tolerant = true
			m, err := NewManager(f.cfg, deps, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if factory == "router" {
				m.deps.NewROS = func(cfg config.RouterOSConfig) (ros.Client, error) {
					return nil, errors.New("cannot connect using " + cfg.Password)
				}
			} else if factory == "provider" {
				m.deps.NewProvider = func(_ string, cfg config.ProviderConfig) (dns.Provider, error) {
					return nil, errors.New("cannot construct using " + cfg.AccessKeySecret)
				}
			}
			startManager(t, m)
			await(t, m.Ready)
			old := m.svc
			hash, data := editDocument(t, m, func(d map[string]any) {
				renameItem(d)
				if factory == "state" {
					d["state"].(map[string]any)["path"] = t.TempDir()
				}
			})
			if _, _, err := m.ApplyConfig(context.Background(), hash, data); err == nil {
				t.Fatal("startup tolerance allowed an invalid configuration candidate")
			} else if strings.Contains(err.Error(), f.cfg.RouterOS.Password) || strings.Contains(err.Error(), "access-key-secret") {
				t.Fatalf("candidate failure exposed credentials: %v", err)
			}
			if m.svc != old || !m.Ready() || !m.deps.Tolerant {
				t.Fatal("failed candidate changed the running service or its dependencies")
			}
			assertFile(t, f.cfg.Path, f.raw)
			if _, err := os.Stat(f.cfg.Path + ".bak"); !os.IsNotExist(err) {
				t.Fatal("failed candidate touched the configuration backup")
			}
		})
	}
}

func TestTolerantManagerApplyPublishesStrictDependencies(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "dormant", true: "running"}[running], func(t *testing.T) {
			f := newFixture(t, Options{})
			deps := f.m.deps
			deps.Tolerant = true
			m, err := NewManager(f.cfg, deps, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if running {
				startManager(t, m)
				await(t, m.Ready)
			}
			builds := 0
			factory := m.newService
			m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
				builds++
				if deps.Tolerant {
					t.Error("candidate service received tolerant dependencies")
				}
				return factory(cfg, deps)
			}
			hash, data := editDocument(t, m, renameItem)
			if _, _, err := m.ApplyConfig(context.Background(), hash, data); err != nil {
				t.Fatal(err)
			}
			wantBuilds := 1
			if running {
				wantBuilds = 2
			}
			if builds != wantBuilds || m.deps.Tolerant || m.deps.State != deps.State || m.Snapshot().Items[0].Name != "replacement" {
				t.Fatalf("apply did not publish strict dependencies with the existing cache: builds=%d tolerant=%t", builds, m.deps.Tolerant)
			}
		})
	}
}

func TestTolerantManagerApplyRebuildFailureRestoresOldDependencies(t *testing.T) {
	f := newFixture(t, Options{})
	deps := f.m.deps
	deps.Tolerant = true
	m, err := NewManager(f.cfg, deps, Options{})
	if err != nil {
		t.Fatal(err)
	}
	startManager(t, m)
	await(t, m.Ready)
	factory := m.newService
	builds := 0
	m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
		builds++
		if cfg.Items[0].Name == "replacement" {
			if deps.Tolerant {
				t.Error("candidate rebuild received tolerant dependencies")
			}
			if builds == 2 {
				return nil, errors.New("candidate rebuild failed")
			}
		} else if !deps.Tolerant {
			t.Error("rollback discarded the original tolerant dependencies")
		}
		return factory(cfg, deps)
	}
	hash, data := editDocument(t, m, renameItem)
	if _, _, err := m.ApplyConfig(context.Background(), hash, data); err == nil || !strings.Contains(err.Error(), "candidate rebuild failed") {
		t.Fatalf("candidate rebuild failure was ignored: %v", err)
	}
	if builds != 3 || !m.deps.Tolerant || m.deps.State != deps.State || m.Snapshot().Items[0].Name != "home" {
		t.Fatal("rollback did not restore the original generation and dependencies")
	}
	assertFile(t, f.cfg.Path, f.raw)
	assertFile(t, f.cfg.Path+".bak", f.raw)
	await(t, m.Ready)
}

func TestTolerantManagerRunsAndNotifiesStartupFailures(t *testing.T) {
	f := newFixture(t, Options{})
	cfg := cloneConfig(f.cfg)
	cfg.Runtime.PollInterval.Duration = 5 * time.Millisecond
	cfg.Runtime.MaxBackoff.Duration = 20 * time.Millisecond
	broken := config.CloneItem(cfg.Items[0])
	broken.Name, broken.Domains[0].RR = "broken", "broken"
	broken.Source.Select.Scope = cfg.RouterOS.Password
	cfg.Items = append([]config.ItemConfig{broken}, cfg.Items...)
	deps := f.m.deps
	deps.Tolerant = true
	deps.NewROS = func(router config.RouterOSConfig) (ros.Client, error) {
		return nil, errors.New("cannot construct RouterOS using " + router.Password)
	}
	store := notify.NewStore(10)
	m, err := NewManager(cfg, deps, Options{Notifications: store})
	if err != nil {
		t.Fatal(err)
	}
	if items := m.Snapshot().Items; len(items) != 2 || items[0].Phase != "error" {
		t.Fatalf("invalid startup item was not retained: %+v", items)
	}
	running := startManager(t, m)
	await(t, func() bool {
		item := m.Snapshot().Items[1]
		return item.Phase == "degraded" && !item.NextPollAt.IsZero()
	})
	nextPoll := m.Snapshot().Items[1].NextPollAt
	await(t, func() bool { return m.Snapshot().Items[1].NextPollAt.After(nextPoll) })

	// Exercise the real notification boundary without waiting for its periodic
	// ticker. The monitor started by Run may already have observed either error.
	m.pollNotifications(make(map[string]notificationState))
	for i, item := range m.Snapshot().Items {
		wantPhase := []string{"error", "degraded"}[i]
		if item.Phase != wantPhase || !strings.Contains(item.LastError, "[redacted]") || strings.Contains(item.LastError, cfg.RouterOS.Password) {
			t.Fatalf("startup failure changed or exposed credentials: %+v", item)
		}
	}
	events := make(map[string]notify.Event)
	for _, event := range store.History(0) {
		if event.Kind != "item_error" || event.Severity != "error" || !strings.Contains(event.Message, "[redacted]") || strings.Contains(event.Message, cfg.RouterOS.Password) {
			t.Fatalf("startup notification was lost or not sanitized: %+v", event)
		}
		events[event.Item] = event
	}
	if events["broken"].Phase != "error" || events["home"].Phase != "degraded" {
		t.Fatalf("missing static-error or polling-error notification: %+v", events)
	}
	if m.Ready() {
		t.Fatal("startup failures reported a ready manager")
	}
	select {
	case <-running.done:
		t.Fatalf("startup failures stopped Manager.Run: %v", running.err)
	default:
	}
	running.stop(t)
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	if len(f.provider.records) != 0 {
		t.Fatal("startup failure reached a DNS target")
	}
}
