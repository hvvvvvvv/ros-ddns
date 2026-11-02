package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/service"
	"ros-ddns/internal/state"
)

type failingService struct {
	managedService
	run func(context.Context) error
}

func (s *failingService) Run(ctx context.Context) error { return s.run(ctx) }

func TestApplyFilesystemFailureKeepsRunningService(t *testing.T) {
	for _, stage := range []string{"backup", "rename"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, Options{})
			if stage == "backup" {
				if err := os.Mkdir(f.cfg.Path+".bak", 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				blocked := filepath.Join(filepath.Dir(f.cfg.Path), "cannot-replace-directory")
				if err := os.Mkdir(blocked, 0700); err != nil {
					t.Fatal(err)
				}
				f.m.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if path == f.cfg.Path {
						return replaceFile(blocked, data, mode)
					}
					return replaceFile(path, data, mode)
				}
			}
			running := startManager(t, f.m)
			await(t, f.m.Ready)
			old := f.m.svc
			hash, data := editDocument(t, f.m, renameItem)
			if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err == nil {
				t.Fatal("filesystem failure ignored")
			}
			if f.m.svc != old || !f.m.Ready() || !f.m.TriggerItem("home") {
				t.Fatal("write failure stopped or replaced the active service")
			}
			assertFile(t, f.cfg.Path, f.raw)
			if stage == "rename" {
				assertFile(t, f.cfg.Path+".bak", f.raw)
			}
			running.stop(t)
			leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(f.cfg.Path), ".*.tmp"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("failed writes left temporary files: %v, %v", leftovers, err)
			}
		})
	}
}

func TestApplyStartupFailureRollsBackAndRestarts(t *testing.T) {
	for _, failRebuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "rebuild", true: "reuse after rebuild failure"}[failRebuild], func(t *testing.T) {
			f := newFixture(t, Options{})
			startManager(t, f.m)
			await(t, f.m.Ready)
			old, oldRun := f.m.svc, f.m.run
			factory := f.m.newService
			builds := 0
			f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
				builds++
				if builds == 3 && failRebuild {
					return nil, errors.New("old factory unavailable")
				}
				svc, err := factory(cfg, deps)
				if err != nil || cfg.Items[0].Name != "replacement" {
					return svc, err
				}
				return &failingService{managedService: svc, run: func(context.Context) error {
					select {
					case <-oldRun.done:
					default:
						t.Error("candidate started before old workers exited")
					}
					return errors.New("startup failed with initial-router-secret and candidate-secret")
				}}, nil
			}
			hash, data := editDocument(t, f.m, func(d map[string]any) {
				renameItem(d)
				d["routeros"].(map[string]any)["password"] = "candidate-secret"
				d["log"].(map[string]any)["level"] = "error"
			})
			snapshot, warnings, err := f.m.ApplyConfig(context.Background(), hash, data)
			if err == nil || !strings.Contains(err.Error(), "startup failed") || strings.Contains(err.Error(), "initial-router-secret") || strings.Contains(err.Error(), "candidate-secret") {
				t.Fatalf("startup error is absent or not sanitized: %v", err)
			}
			if builds != 3 || len(warnings) != 0 || snapshot.Items[0].Name != "home" || f.m.Meta().LogLevel != "info" {
				t.Fatalf("rollback did not rebuild previous generation: builds=%d snapshot=%+v", builds, snapshot)
			}
			if (f.m.svc == old) != failRebuild {
				t.Fatal("rollback did not use the expected rebuilt/reused service")
			}
			assertFile(t, f.cfg.Path, f.raw)
			assertFile(t, f.cfg.Path+".bak", f.raw)
			await(t, f.m.Ready)
			if !f.m.TriggerItem("home") || f.m.TriggerItem("replacement") {
				t.Fatal("rollback left the candidate active")
			}
		})
	}
}

func TestApplyReportsRollbackWriteFailure(t *testing.T) {
	f := newFixture(t, Options{})
	startManager(t, f.m)
	await(t, f.m.Ready)
	factory := f.m.newService
	f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
		svc, err := factory(cfg, deps)
		if err != nil || cfg.Items[0].Name != "replacement" {
			return svc, err
		}
		return &failingService{managedService: svc, run: func(context.Context) error { return errors.New("startup failure") }}, nil
	}
	writes := 0
	f.m.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == f.cfg.Path {
			writes++
			if writes == 2 {
				return errors.New("restore disk failure")
			}
		}
		return replaceFile(path, data, mode)
	}
	hash, data := editDocument(t, f.m, renameItem)
	_, _, err := f.m.ApplyConfig(context.Background(), hash, data)
	if err == nil || !strings.Contains(err.Error(), "restore configuration file") || !strings.Contains(err.Error(), "startup failure") {
		t.Fatalf("rollback failure hidden: %v", err)
	}
	await(t, f.m.Ready)
	if f.m.Snapshot().Items[0].Name != "home" {
		t.Fatal("failed disk rollback prevented old runtime recovery")
	}
	assertFile(t, f.cfg.Path+".bak", f.raw)
}

func TestApplySerializationAndDelegationDuringReload(t *testing.T) {
	f := newFixture(t, Options{})
	startManager(t, f.m)
	await(t, f.m.Ready)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var enteredOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	factory := f.m.newService
	f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return factory(cfg, deps)
	}
	hash, data := editDocument(t, f.m, renameItem)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := f.m.ApplyConfig(context.Background(), hash, data); first <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("candidate construction never started")
	}
	for range 20 {
		if f.m.Ready() || f.m.TriggerItem("home") || f.m.Snapshot().Items[0].Name != "home" {
			t.Fatal("reload gate or old snapshot delegation failed")
		}
		_ = f.m.Meta()
	}
	go func() { _, _, err := f.m.ApplyConfig(context.Background(), hash, data); second <- err }()
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first apply did not finish")
	}
	select {
	case err := <-second:
		if !errors.Is(err, ErrConfigConflict) {
			t.Fatalf("overlapping apply failed to recheck hash: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second apply did not finish")
	}
	assertFile(t, f.cfg.Path+".bak", f.raw)
}

func TestApplyDetectsExternalEditDuringConstruction(t *testing.T) {
	f := newFixture(t, Options{})
	factory := f.m.newService
	external := append(append([]byte(nil), f.raw...), []byte("# external edit\n")...)
	f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
		if err := os.WriteFile(cfg.Path, external, 0600); err != nil {
			return nil, err
		}
		return factory(cfg, deps)
	}
	hash, data := editDocument(t, f.m, renameItem)
	if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("external edit was overwritten: %v", err)
	}
	assertFile(t, f.cfg.Path, external)
	if f.m.Snapshot().Items[0].Name != "home" {
		t.Fatal("external conflict swapped service")
	}
}

func TestRequestCancellationAndDaemonCancellationDuringApply(t *testing.T) {
	for _, stage := range []string{"before apply", "after write", "after stop", "daemon shutdown"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, Options{})
			running := startManager(t, f.m)
			await(t, f.m.Ready)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before apply" {
				cancel()
			} else if stage == "after write" || stage == "daemon shutdown" {
				f.m.writeFile = func(path string, data []byte, mode os.FileMode) error {
					err := replaceFile(path, data, mode)
					if path == f.cfg.Path {
						if stage == "after write" {
							cancel()
						} else {
							running.cancel()
						}
					}
					return err
				}
			} else {
				factory := f.m.newService
				f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
					svc, err := factory(cfg, deps)
					if cfg.Items[0].Name != "replacement" || err != nil {
						return svc, err
					}
					return &failingService{managedService: svc, run: func(ctx context.Context) error {
						cancel()
						return svc.Run(ctx)
					}}, nil
				}
			}
			hash, data := editDocument(t, f.m, renameItem)
			_, _, err := f.m.ApplyConfig(ctx, hash, data)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled transaction returned %v", err)
			}
			assertFile(t, f.cfg.Path, f.raw)
			if f.m.Snapshot().Items[0].Name != "home" {
				t.Fatal("canceled transaction retained candidate")
			}
			if stage != "daemon shutdown" {
				await(t, f.m.Ready)
			}
		})
	}
}

func TestAppliedServiceOutlivesRequestContext(t *testing.T) {
	f := newFixture(t, Options{})
	startManager(t, f.m)
	await(t, f.m.Ready)
	hash, data := editDocument(t, f.m, renameItem)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := f.m.ApplyConfig(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	cancel()
	await(t, f.m.Ready)
	if f.m.runCtx.Err() != nil || !f.m.TriggerItem("replacement") {
		t.Fatal("request cancellation stopped the installed service")
	}
}

func TestManagerWaitsForBlockedWorkerOnShutdown(t *testing.T) {
	f := newFixture(t, Options{})
	entered := make(chan struct{})
	var exited atomic.Bool
	f.router.poll = func(ctx context.Context) ([]ros.Address, error) {
		close(entered)
		<-ctx.Done()
		exited.Store(true)
		return nil, ctx.Err()
	}
	running := startManager(t, f.m)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not start")
	}
	running.stop(t)
	if !exited.Load() || f.m.Ready() {
		t.Fatal("shutdown did not wait for blocked worker")
	}
}

func TestReloadReadsInvalidationAfterOldWorkersJoin(t *testing.T) {
	for _, stateAlias := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared store", true: "state path spelling changed"}[stateAlias], func(t *testing.T) {
			f := newFixture(t, Options{})
			hash, data := editDocument(t, f.m, func(d map[string]any) {
				runtime := d["runtime"].(map[string]any)
				runtime["syncOnStart"], runtime["verifyInterval"], runtime["reconcileTimeout"] = false, "0s", "30s"
			})
			if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err != nil {
				t.Fatal(err)
			}
			running := startManager(t, f.m)
			await(t, func() bool {
				cached, ok := f.m.deps.State.Get("home")
				return ok && !cached.LastSyncAt.IsZero() && f.m.Snapshot().Items[0].Phase == "synced"
			})
			forced, repairing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var attempts atomic.Int32
			f.provider.mu.Lock()
			f.provider.ensure = func(ctx context.Context, _ dns.Record) error {
				switch attempts.Add(1) {
				case 1:
					close(forced)
					<-ctx.Done() // oldRun.stop must cancel and join this forced repair.
					return ctx.Err()
				case 2:
					close(repairing)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				default:
					return errors.New("unexpected extra DNS operation")
				}
			}
			f.provider.mu.Unlock()
			if !f.m.TriggerItem("home") {
				t.Fatal("force rejected")
			}
			select {
			case <-forced:
			case <-time.After(5 * time.Second):
				t.Fatal("force did not start")
			}
			oldRun, factory := f.m.run, f.m.newService
			builds := 0
			f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
				builds++
				if cfg.Runtime.SyncOnStart || cfg.Runtime.VerifyInterval.Duration != 0 {
					t.Error("test must disable startup force and periodic verification")
				}
				svc, err := factory(cfg, deps)
				if err != nil {
					return nil, err
				}
				status := svc.Snapshot().Items[0].Domains[0]
				if builds == 1 {
					select {
					case <-oldRun.done:
						t.Error("old run stopped before preliminary construction")
					default:
					}
					if status.LastError != "" || status.Value == "" {
						t.Error("preliminary cache was not confirmed")
					}
				} else {
					select {
					case <-oldRun.done:
					default:
						t.Error("fresh construction preceded old worker exit")
					}
					if status.LastError == "" {
						t.Error("fresh candidate missed invalidation")
					}
					disk, err := state.Open(cfg.State.Path)
					if err != nil {
						return nil, err
					}
					cached, ok := disk.Get("home")
					if !ok || len(cached.Domains) != 1 {
						t.Error("missing persisted invalidation")
					}
					for _, domain := range cached.Domains {
						if domain.LastError == "" {
							t.Error("invalidation not persisted before handoff")
						}
					}
				}
				return svc, nil
			}
			hash, data = editDocument(t, f.m, func(d map[string]any) {
				d["log"].(map[string]any)["level"] = "debug"
				if stateAlias {
					d["state"].(map[string]any)["path"] = filepath.Dir(f.cfg.State.Path) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(f.cfg.State.Path)
				}
			})
			if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err != nil {
				t.Fatal(err)
			}
			if builds != 2 {
				t.Fatalf("candidate constructions = %d, want 2", builds)
			}
			select {
			case <-repairing:
			case <-time.After(5 * time.Second):
				t.Fatal("new generation trusted stale confirmation instead of repairing")
			}
			if f.m.Snapshot().Items[0].Phase == "synced" {
				t.Fatal("reported synced before repair completed")
			}
			close(release)
			await(t, func() bool { return f.m.Snapshot().Items[0].Phase == "synced" })
			running.stop(t)
			if attempts.Load() != 2 {
				t.Fatalf("DNS attempts = %d, want old failure and new repair", attempts.Load())
			}
		})
	}
}

func TestFreshCandidateConstructionFailureRollsBack(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "rebuild", true: "reuse"}[reuse], func(t *testing.T) {
			f := newFixture(t, Options{})
			startManager(t, f.m)
			await(t, f.m.Ready)
			old, oldRun, factory := f.m.svc, f.m.run, f.m.newService
			builds := 0
			f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
				builds++
				if builds == 2 {
					select {
					case <-oldRun.done:
					default:
						t.Error("old service not joined")
					}
					// Match service.New's typed nil when converted to managedService.
					return (*service.Service)(nil), errors.New("late construction failure with initial-router-secret")
				}
				if builds == 3 && reuse {
					return nil, errors.New("rollback factory unavailable")
				}
				return factory(cfg, deps)
			}
			hash, data := editDocument(t, f.m, renameItem)
			_, _, err := f.m.ApplyConfig(context.Background(), hash, data)
			if err == nil || !strings.Contains(err.Error(), "rebuild candidate") || strings.Contains(err.Error(), "initial-router-secret") {
				t.Fatalf("missing or unsafe construction error: %v", err)
			}
			assertFile(t, f.cfg.Path, f.raw)
			assertFile(t, f.cfg.Path+".bak", f.raw)
			await(t, f.m.Ready)
			if builds != 3 || (f.m.svc == old) != reuse || f.m.Snapshot().Items[0].Name != "home" {
				t.Fatal("late failure did not restore previous generation")
			}
		})
	}
}
