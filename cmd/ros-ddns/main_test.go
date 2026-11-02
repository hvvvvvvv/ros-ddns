package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/instancelock"
	"ros-ddns/internal/pathguard"
	"ros-ddns/internal/svcmgr"
	"ros-ddns/internal/version"
)

func commandConfig(routerURL, listen string) string {
	return fmt.Sprintf(`
state:
  path: ""
api:
  listen: %q
  token: ${W4_TEST_API_TOKEN:-}
routeros:
  url: %q
  username: test-user
  password: ${W4_TEST_PASSWORD}
  timeout: 1s
  tls:
    caFile: ${W4_TEST_CA_FILE:-}
items:
  - name: home-ipv4
    source:
      interface: wan
      family: ipv4
    addressList:
      list: ddns-home
`, listen, routerURL)
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	t.Setenv("W4_TEST_PASSWORD", "dummy-router-password")
	t.Setenv("W4_TEST_API_TOKEN", "dummy-api-token")
	t.Setenv("W4_TEST_CA_FILE", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	// Memory-only CLI tests still create a retained lock in os.TempDir(). Give
	// it the same lifetime as this unique test config, after run has returned.
	t.Cleanup(func() {
		lockPath, err := instanceLockPath("", "", path)
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove test lock: %v", err)
		}
	})
	return path
}

func TestRunWithHeldInstanceLock(t *testing.T) {
	var requests atomic.Int32
	router := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `[]`)
	}))
	defer router.Close()
	path := writeConfig(t, commandConfig(router.URL, "127.0.0.1:8080"))
	// A missing CA also proves the lock is checked before constructing a client.
	t.Setenv("W4_TEST_CA_FILE", filepath.ToSlash(filepath.Join(t.TempDir(), "missing-ca.pem")))
	lockPath := filepath.Join(t.TempDir(), "external.lock")
	lock, err := instancelock.Acquire(lockPath, instancelock.Info{
		PID: os.Getpid(), StartedAt: time.Now().UTC(), ConfigPath: path, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	{
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"-config", path, "-lock-file", lockPath}, &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "another instance") || !strings.Contains(stderr.String(), "lock") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", path, "-lock-file", lockPath, "-check"}, &stdout, &stderr)
	if code != 0 || !strings.HasPrefix(stdout.String(), "config: ") || stderr.Len() != 0 {
		t.Fatalf("-check: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if requests.Load() != 0 {
		t.Errorf("RouterOS requests = %d, want zero", requests.Load())
	}
}

func TestInstanceLockPath(t *testing.T) {
	absConfig := filepath.Join(t.TempDir(), "config.yaml")
	canonical, err := pathguard.Canonical(absConfig)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(canonical))
	for _, tc := range []struct{ name, explicit, state, want string }{
		{"explicit", "custom.lock", "state.json", "custom.lock"},
		{"state", "", filepath.Join("data", "state.json"), filepath.Join("data", "state.json.lock")},
		{"memory", "", "", filepath.Join(os.TempDir(), fmt.Sprintf("ros-ddns-%x.lock", hash[:8]))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := instanceLockPath(tc.explicit, tc.state, absConfig); err != nil || got != tc.want {
				t.Errorf("lock path = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestRunMemoryLockUsesAbsoluteConfigPath(t *testing.T) {
	path := writeConfig(t, commandConfig("https://127.0.0.1:1", "127.0.0.1:8080"))
	t.Chdir(filepath.Dir(path))
	lockPath, err := instanceLockPath("", "", path)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := instancelock.Acquire(lockPath, instancelock.Info{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", "config.yaml"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "another instance") {
		t.Fatalf("relative config: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestMemoryLockCanonicalConfigAliases(t *testing.T) {
	var requests atomic.Int32
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `[]`)
	}))
	defer router.Close()
	path := writeConfig(t, commandConfig(router.URL, "127.0.0.1:8080"))
	t.Chdir(filepath.Dir(path))
	want, err := instanceLockPath("", "", path)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, alias string) {
		t.Helper()
		got, err := instanceLockPath("", "", alias)
		if err != nil || got != want {
			t.Fatalf("alias lock = %q, %v; want %q", got, err, want)
		}
		lock, err := instancelock.Acquire(want, instancelock.Info{PID: os.Getpid()})
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"-config", alias}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "another instance") {
			t.Fatalf("alias bypassed lock: %d %s", code, stderr.String())
		}
	}
	for _, alias := range []string{"config.yaml", "./config.yaml", path} {
		check(t, alias)
	}
	t.Run("case", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("Windows case folding")
		}
		if _, err := os.Stat("CONFIG.yaml"); err != nil {
			t.Skipf("case-sensitive filesystem: %v", err)
		}
		check(t, "CONFIG.yaml")
	})
	t.Run("symlink", func(t *testing.T) {
		alias := filepath.Join(filepath.Dir(path), "alias.yaml")
		if err := os.Symlink(path, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		check(t, alias)
	})
	if requests.Load() != 0 {
		t.Fatal("lock alias bypass contacted the local RouterOS stub")
	}
}

func TestRunRejectsPathCollisionsBeforeLockWrites(t *testing.T) {
	var requests atomic.Int32
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `[]`)
	}))
	defer router.Close()
	for _, collision := range []string{"lock-config", "lock-backup", "lock-state", "state-config", "state-backup"} {
		for _, mode := range [][]string{nil, {"-check"}} {
			t.Run(collision+fmt.Sprint(mode), func(t *testing.T) {
				path := writeConfig(t, commandConfig(router.URL, "127.0.0.1:8080"))
				statePath, lockPath := filepath.Join(filepath.Dir(path), "state.json"), filepath.Join(filepath.Dir(path), "instance.lock")
				switch collision {
				case "lock-config":
					lockPath = path
				case "lock-backup":
					lockPath = path + ".bak"
				case "lock-state":
					lockPath = statePath
				case "state-config":
					statePath = path
				case "state-backup":
					statePath = path + ".bak"
				}
				raw := []byte(strings.Replace(commandConfig(router.URL, "127.0.0.1:8080"), `path: ""`, fmt.Sprintf("path: %q", statePath), 1))
				files := map[string][]byte{path: raw, path + ".bak": []byte("backup sentinel"), filepath.Join(filepath.Dir(path), "state.json"): []byte("state sentinel")}
				for name, data := range files {
					if err := os.WriteFile(name, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				var stdout, stderr bytes.Buffer
				args := append([]string{"-config", path, "-lock-file", lockPath}, mode...)
				code := run(context.Background(), args, &stdout, &stderr)
				if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "same file") {
					t.Fatalf("collision not rejected as config error: %d %s", code, stderr.String())
				}
				for name, want := range files {
					if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, want) {
						t.Fatalf("target changed: %s, %v", name, err)
					}
				}
				if _, exists := files[lockPath]; !exists {
					if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
						t.Fatalf("lock created: %v", err)
					}
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatal("rejected config contacted the local RouterOS stub")
	}
}

type fakeDaemonService struct {
	fakeControlService
	program svcmgr.Interface
	onRun   func() error
}

func (f *fakeDaemonService) Run() error {
	f.record("Run")
	if f.onRun != nil {
		return f.onRun()
	}
	return nil
}

func withDaemonMode(t *testing.T, serviceManaged bool) *fakeDaemonService {
	t.Helper()
	fake := &fakeDaemonService{}
	interactive, newService := isInteractive, newDaemonService
	isInteractive = func() bool { return !serviceManaged }
	newDaemonService = func(program svcmgr.Interface, _ svcmgr.Options) (svcmgr.Service, error) {
		fake.program = program
		return fake, nil
	}
	t.Cleanup(func() {
		isInteractive, newDaemonService = interactive, newService
	})
	// Restore cwd before TempDir cleanup when service startup changes it.
	t.Chdir(t.TempDir())
	return fake
}

func TestRunServiceRecoveryWaitsForInstanceLock(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			clearDefaultConfigEnv(t)
			fake := withDaemonMode(t, true)
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			lockPath := filepath.Join(dir, "state.json.lock")
			args := []string{"-config", path}
			if explicit {
				lockPath = filepath.Join(dir, "custom.lock")
				args = append(args, "-lock-file", "custom.lock")
			}
			files := map[string][]byte{
				path: []byte("routeros: [\n"), path + ".bak": []byte("backup sentinel"),
				filepath.Join(dir, "state.json"): []byte("state sentinel"),
			}
			for name, raw := range files {
				if err := os.WriteFile(name, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := instancelock.Acquire(lockPath, instancelock.Info{PID: os.Getpid(), ConfigPath: path})
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			files[lockPath], err = os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "another instance") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			for name, want := range files {
				if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("contender changed %s: %q, %v", name, got, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != len(files) {
				t.Fatalf("contender added files (including quarantine): %v, %v", entries, err)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("contender started the daemon: %v", fake.calls)
			}
		})
	}
}

func TestRunMissingConfigRejectsPathCollisionsWithoutWrites(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		for _, collision := range []string{"config-state", "config-default-lock", "lock-config", "lock-backup", "lock-state", "env-config", "env-backup", "env-lock"} {
			t.Run(fmt.Sprintf("service=%t/%s", serviceManaged, collision), func(t *testing.T) {
				clearDefaultConfigEnv(t)
				fake := withDaemonMode(t, serviceManaged)
				dir := filepath.Join(t.TempDir(), "missing")
				path, statePath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.json")
				var explicit string
				switch collision {
				case "config-state":
					path = statePath
				case "config-default-lock":
					path = statePath + ".lock"
				case "lock-config":
					explicit = path
				case "lock-backup":
					explicit = path + ".bak"
				case "lock-state":
					explicit = statePath
				case "env-config":
					t.Setenv("ROS_DDNS_STATE_PATH", path)
				case "env-backup":
					t.Setenv("ROS_DDNS_STATE_PATH", path+".bak")
				case "env-lock":
					t.Setenv("ROS_DDNS_STATE_PATH", statePath+".lock")
				}
				args := []string{"-config", path}
				if explicit != "" {
					args = append(args, "-lock-file", explicit)
				}
				var stdout, stderr bytes.Buffer
				wantCode := 2
				if serviceManaged {
					wantCode = 1
				}
				if code := run(context.Background(), args, &stdout, &stderr); code != wantCode || !strings.Contains(stderr.String(), "same file") {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("collision created files or directories: %v", err)
				}
				if len(fake.calls) != 0 {
					t.Fatalf("invalid paths started the daemon: %v", fake.calls)
				}
			})
		}
	}
}

func TestRunServiceConfigCannotNameHeldLock(t *testing.T) {
	clearDefaultConfigEnv(t)
	fake := withDaemonMode(t, true)
	path := filepath.Join(t.TempDir(), "state.json.lock")
	lock, err := instancelock.Acquire(path, instancelock.Info{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", path}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "same file") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("config recovery changed the lock: %q, %v", got, err)
	}
	if matches, err := filepath.Glob(path + ".invalid-*"); err != nil || len(matches) != 0 || len(fake.calls) != 0 {
		t.Fatalf("lock file was recovered or daemon started: matches=%v calls=%v err=%v", matches, fake.calls, err)
	}
}

func TestRunServiceFirstStartHoldsLock(t *testing.T) {
	clearDefaultConfigEnv(t)
	fake := withDaemonMode(t, true)
	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	lockPath := filepath.Join(filepath.Dir(path), "state.json.lock")
	fake.onRun = func() error {
		if _, err := config.Load(path); err != nil {
			t.Fatalf("default config not ready at service start: %v", err)
		}
		contender, err := instancelock.Acquire(lockPath, instancelock.Info{PID: -1})
		if contender != nil {
			_ = contender.Close()
		}
		if !errors.Is(err, instancelock.ErrLocked) {
			t.Fatalf("service started without its lock: %v", err)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", path}, &stdout, &stderr); code != 0 || len(fake.calls) != 1 {
		t.Fatalf("code=%d calls=%v stdout=%q stderr=%q", code, fake.calls, stdout.String(), stderr.String())
	}
	lock, err := instancelock.Acquire(lockPath, instancelock.Info{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("startup did not release the lock on exit: %v", err)
	}
	defer lock.Close()
}

func TestRunRecoveryRejectsChangedLockPath(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		for _, state := range []string{"other-state.json", ""} {
			t.Run(fmt.Sprintf("service=%t/state=%q", serviceManaged, state), func(t *testing.T) {
				clearDefaultConfigEnv(t)
				fake := withDaemonMode(t, serviceManaged)
				path := filepath.Join(t.TempDir(), "config.yaml")
				if serviceManaged {
					if err := os.WriteFile(path, []byte("routeros: [\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("ROS_DDNS_STATE_PATH", state)
				var stdout, stderr bytes.Buffer
				wantCode := 2
				if serviceManaged {
					wantCode = 1
				}
				code := run(context.Background(), []string{"-config", path}, &stdout, &stderr)
				if code != wantCode || !strings.Contains(stderr.String(), "changed the instance lock path") || !strings.Contains(stderr.String(), "-lock-file") {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				cfg, err := config.Load(path)
				if err != nil {
					t.Fatalf("first recovery should write a loadable default: %v", err)
				}
				wantDefault, err := defaultConfigWithStatePath(filepath.Join(filepath.Dir(path), "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, wantDefault) {
					t.Fatalf("unexpected recovery output: %q, %v", got, err)
				}
				otherLock, err := instanceLockPath("", cfg.State.Path, path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(otherLock); !os.IsNotExist(err) {
					t.Fatalf("startup acquired a second lock: %v", err)
				}
				wantQuarantines := 0
				if serviceManaged {
					wantQuarantines = 1
				}
				matches, err := filepath.Glob(path + ".invalid-*")
				if err != nil || len(matches) != wantQuarantines || len(fake.calls) != 0 {
					t.Fatalf("repeated recovery or daemon start: matches=%v calls=%v err=%v", matches, fake.calls, err)
				}
				if serviceManaged {
					if got, err := os.ReadFile(matches[0]); err != nil || string(got) != "routeros: [\n" {
						t.Fatalf("original invalid config not preserved: %q, %v", got, err)
					}
				}
			})
		}
	}
}

func TestRunServiceRelativePathsUseConfigDirectory(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			clearDefaultConfigEnv(t)
			fake := withDaemonMode(t, true)
			dir := filepath.Join(t.TempDir(), "nested")
			path := filepath.Join(dir, "config.yaml")
			args := []string{"-config", path}
			lockName := "relative-state.json.lock"
			if missing {
				t.Setenv("ROS_DDNS_STATE_PATH", "relative-state.json")
				lockName = "custom.lock"
				args = append(args, "-lock-file", lockName)
			} else {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				raw := strings.Replace(commandConfig("https://router.invalid", "127.0.0.1:8080"), `path: ""`, `path: relative-state.json`, 1)
				t.Setenv("W4_TEST_PASSWORD", "dummy-router-password")
				t.Setenv("W4_TEST_API_TOKEN", "")
				t.Setenv("W4_TEST_CA_FILE", "")
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			startupDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			fake.onRun = func() error {
				if wd, err := os.Getwd(); err != nil || wd != dir {
					t.Fatalf("service cwd=%q err=%v, want %q", wd, err, dir)
				}
				return nil
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != 0 || len(fake.calls) != 1 {
				t.Fatalf("code=%d calls=%v stdout=%q stderr=%q", code, fake.calls, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
				t.Fatalf("service lock not in configuration directory: %v", err)
			}
			if _, err := os.Stat(filepath.Join(startupDir, lockName)); !os.IsNotExist(err) {
				t.Fatalf("service lock written in startup cwd: %v", err)
			}
		})
	}
}

func TestRunCheckNeverInitializesServiceOrRecoversConfig(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			fake := withDaemonMode(t, true)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if !missing {
				if err := os.WriteFile(path, []byte("routeros: [\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{"-config", path, "-check"}, &stdout, &stderr); code != 2 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if fake.program != nil {
				t.Fatal("-check initialized the service manager")
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			wantEntries := 1
			if missing {
				wantEntries = 0
			}
			if err != nil || len(entries) != wantEntries {
				t.Fatalf("-check wrote files: %v, %v", entries, err)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	code := run(context.Background(), []string{"-config", missing, "-version"}, &stdout, &stderr)
	if code != 0 || stdout.String() != version.String()+"\n" || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunCheck(t *testing.T) {
	path := writeConfig(t, commandConfig("https://router.invalid", "127.0.0.1:8080"))
	// Loading a config must not construct the REST client or read its CA file.
	t.Setenv("W4_TEST_CA_FILE", filepath.ToSlash(filepath.Join(t.TempDir(), "missing-ca.pem")))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", path, "-check"}, &stdout, &stderr)
	if code != 0 || stdout.String() != config.Summary(cfg) || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, secret := range []string{"dummy-router-password", "dummy-api-token"} {
		if strings.Contains(stdout.String(), secret) {
			t.Errorf("summary exposed %q", secret)
		}
	}
	lockPath, err := instanceLockPath("", cfg.State.Path, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("-check created an instance lock: %v", err)
	}
}

func TestRunConfigErrors(t *testing.T) {
	for _, tc := range []struct{ name, contents, message string }{
		{"missing", "", "read config"},
		{"invalid YAML", "routeros: [", "parse YAML"},
		{"invalid fields", "log: {level: unsupported}", "invalid config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.yaml")
			if tc.contents != "" {
				path = writeConfig(t, tc.contents)
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"-config", path, "-check"}, &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.message) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if tc.contents == "" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("-check must not create a missing configuration: %v", err)
				}
			}
		})
	}
}

func TestRemovedFlagsRejected(t *testing.T) {
	for _, removed := range []string{"-once", "-dry-run"} {
		t.Run(removed, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{removed}, &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "flag provided but not defined") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunHTTPWarning(t *testing.T) {
	path := writeConfig(t, commandConfig("http://router.invalid", "127.0.0.1:8080")+"\nlog: {level: warn, format: json}\n")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", path, "-check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var entry struct{ Level, Msg string }
	if err := json.Unmarshal(stderr.Bytes(), &entry); err != nil {
		t.Fatalf("stderr is not a JSON log: %v; %s", err, stderr.String())
	}
	if entry.Level != "WARN" || !strings.Contains(entry.Msg, "http://") {
		t.Errorf("warning = %+v", entry)
	}
	if !strings.HasPrefix(stdout.String(), "config: ") || strings.Contains(stderr.String(), "dummy-router-password") {
		t.Errorf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestLoggerLevelsAndFormats(t *testing.T) {
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for i, level := range []string{"debug", "info", "warn", "error"} {
		for _, format := range []string{"text", "json"} {
			t.Run(level+"/"+format, func(t *testing.T) {
				var output bytes.Buffer
				logger := newLogger(config.LogConfig{Level: level, Format: format}, &output)
				for _, value := range levels {
					logger.Log(context.Background(), value, value.String())
				}
				lines := strings.Split(strings.TrimSpace(output.String()), "\n")
				if len(lines) != len(levels)-i {
					t.Fatalf("log lines = %d, want %d: %s", len(lines), len(levels)-i, output.String())
				}
				for j, line := range lines {
					want := levels[i+j].String()
					if format == "json" {
						var entry struct{ Level, Msg string }
						if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Level != want || entry.Msg != want {
							t.Errorf("JSON log = %q, want level/message %q", line, want)
						}
					} else if !strings.Contains(line, "level="+want) || !strings.Contains(line, "msg="+want) {
						t.Errorf("text log = %q, want level/message %q", line, want)
					}
				}
			})
		}
	}
}

func TestRunInitializationError(t *testing.T) {
	path := writeConfig(t, commandConfig("https://router.invalid", "127.0.0.1:8080"))
	t.Setenv("W4_TEST_CA_FILE", filepath.ToSlash(filepath.Join(t.TempDir(), "missing-ca.pem")))
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", path}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "initialize service") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunDaemonListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var requests atomic.Int32
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	defer router.Close()
	path := writeConfig(t, commandConfig(router.URL, listener.Addr().String()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"-config", path}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "listen HTTP API") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("RouterOS requests = %d, want zero before API bind succeeds", got)
	}
	if ctx.Err() != nil {
		t.Errorf("bind failure did not return before the test deadline: %v", ctx.Err())
	}
}

func TestRunDaemonCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var polls atomic.Int32
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		fmt.Fprint(w, `[]`)
		cancel()
	}))
	defer router.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, commandConfig(router.URL, address))
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"-config", path}, &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 || polls.Load() == 0 {
		t.Fatalf("code=%d polls=%d stdout=%q stderr=%q", code, polls.Load(), stdout.String(), stderr.String())
	}
}

func TestRunInvalidAPIPort(t *testing.T) {
	var requests atomic.Int32
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `[]`)
	}))
	defer router.Close()
	for _, port := range []string{"-1", "0", "65536", "http", "8080 "} {
		t.Run(port, func(t *testing.T) {
			path := writeConfig(t, commandConfig(router.URL, "127.0.0.1:"+port))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			code := run(ctx, []string{"-config", path}, &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid config") || strings.Contains(stderr.String(), "listen HTTP API") {
				t.Fatalf("code=%d stdout=%q stderr=%q, want config error before binding", code, stdout.String(), stderr.String())
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("RouterOS requests = %d, want zero for invalid config", got)
			}
		})
	}
}
