package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/instancelock"
)

func prepareTestDaemonConfig(t *testing.T, path string, serviceManaged bool) (*config.Config, *configRecovery, error) {
	t.Helper()
	lockPath, err := daemonConfigLockPath(path, "", serviceManaged)
	if err != nil {
		return nil, nil, err
	}
	lock, err := instancelock.Acquire(lockPath, instancelock.Info{PID: os.Getpid()})
	if err != nil {
		return nil, nil, err
	}
	defer lock.Close()
	return prepareDaemonConfig(path, serviceManaged, lock)
}

func clearDefaultConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ROS_DDNS_LISTEN",
		"ROS_DDNS_API_TOKEN",
		"ROS_DDNS_ROUTEROS_URL",
		"ROS_DDNS_ROUTEROS_USERNAME",
		"ROS_PASSWORD",
		"ROS_DDNS_CA_FILE",
		"ALI_ACCESS_KEY_ID",
		"ALI_ACCESS_KEY_SECRET",
		"ALI_SECURITY_TOKEN",
		"ROS_DDNS_INTERFACE",
		"ROS_DDNS_DOMAIN",
		"ROS_DDNS_LOG_LEVEL",
		"ROS_DDNS_LOG_FORMAT",
		"ROS_DDNS_POLL_INTERVAL",
		"ROS_DDNS_STABLE_FOR",
		"ROS_DDNS_RECONCILE_TIMEOUT",
		"ROS_DDNS_VERIFY_INTERVAL",
		"ROS_DDNS_SYNC_ON_START",
		"ROS_DDNS_MAX_BACKOFF",
		"ROS_DDNS_STATE_PATH",
	} {
		value, wasSet := os.LookupEnv(name)
		t.Cleanup(func() {
			var err error
			if wasSet {
				err = os.Setenv(name, value)
			} else {
				err = os.Unsetenv(name)
			}
			if err != nil {
				t.Errorf("restore environment variable %s: %v", name, err)
			}
		})
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset environment variable %s: %v", name, err)
		}
	}
}

func TestRecoverConfigCreatesDefault(t *testing.T) {
	clearDefaultConfigEnv(t)
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "config.yaml")
	recovery, err := recoverConfig(path, errors.New("read config: file does not exist"))
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Created || recovery.Quarantined != "" {
		t.Fatalf("recovery = %+v, want created without quarantine", recovery)
	}
	if recovery.LoadErr == nil {
		t.Fatal("recovery should keep the original load error for reporting")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, config.DefaultConfigYAML()) {
		t.Fatal("generated configuration should contain an absolute state path")
	}
	for _, marker := range []string{"${ROS_DDNS_LOG_LEVEL:-info}", "${ROS_DDNS_SYNC_ON_START:-true}", "${ROS_DDNS_STATE_PATH:-"} {
		if !bytes.Contains(raw, []byte(marker)) {
			t.Fatalf("generated configuration is missing %q", marker)
		}
	}
	cfg, err := config.Parse(raw, path)
	if err != nil {
		t.Fatalf("generated configuration does not load: %v", err)
	}
	wantState := filepath.ToSlash(filepath.Join(dir, "state.json"))
	if cfg.State.Path != wantState {
		t.Fatalf("generated state.path = %q, want %q", cfg.State.Path, wantState)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0600 {
			t.Fatalf("generated configuration mode = %04o, want 0600", mode)
		}
	}
}

func TestGeneratedStatePathEnvOverride(t *testing.T) {
	clearDefaultConfigEnv(t)
	t.Setenv("ROS_DDNS_STATE_PATH", "custom/state.json")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if _, err := recoverConfig(path, errors.New("read config: file does not exist")); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load generated configuration: %v", err)
	}
	if cfg.State.Path != "custom/state.json" {
		t.Fatalf("state.path = %q, want custom/state.json", cfg.State.Path)
	}
	if cfg.Log.Level != "info" || cfg.Runtime.PollInterval.Duration != 10*time.Second {
		t.Fatalf("defaults were not applied: log=%q poll=%s", cfg.Log.Level, cfg.Runtime.PollInterval)
	}
}

func TestRecoverConfigQuarantinesUnloadableFile(t *testing.T) {
	clearDefaultConfigEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte("routeros: [\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	recovery, err := recoverConfig(path, errors.New("parse YAML: invalid"))
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Quarantined == "" || !recovery.Created {
		t.Fatalf("recovery = %+v, want quarantined and created", recovery)
	}
	if !strings.HasPrefix(filepath.Base(recovery.Quarantined), "config.yaml.invalid-") {
		t.Fatalf("quarantine path = %q", recovery.Quarantined)
	}
	preserved, err := os.ReadFile(recovery.Quarantined)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(preserved, original) {
		t.Fatalf("quarantined bytes = %q, want %q", preserved, original)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(current, original) {
		t.Fatal("configuration was not replaced by the default")
	}
	if _, err := config.Parse(current, path); err != nil {
		t.Fatalf("replacement configuration does not load: %v", err)
	}
}

func TestRecoverConfigRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := recoverConfig(dir, errors.New("read config")); err == nil {
		t.Fatal("recovering a directory path must fail")
	}
}

func TestPrepareDaemonConfigInteractiveKeepsWorkingDirectory(t *testing.T) {
	clearDefaultConfigEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	cfg, recovery, err := prepareTestDaemonConfig(t, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if recovery == nil || !recovery.Created || recovery.Quarantined != "" {
		t.Fatalf("recovery = %+v", recovery)
	}
	if cfg.API.Listen != "127.0.0.1:8080" || len(cfg.Items) != 1 {
		t.Fatalf("loaded default = api %q items %d", cfg.API.Listen, len(cfg.Items))
	}
	wantState := filepath.ToSlash(filepath.Join(dir, "state.json"))
	if cfg.State.Path != wantState {
		t.Fatalf("state.path = %q, want %q", cfg.State.Path, wantState)
	}
	if wd, err := os.Getwd(); err != nil || wd != original {
		t.Fatalf("interactive start changed working directory to %q (err %v)", wd, err)
	}
}

func TestPrepareDaemonConfigServiceModeChangesWorkingDirectory(t *testing.T) {
	clearDefaultConfigEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	if _, _, err := prepareTestDaemonConfig(t, path, true); err != nil {
		t.Fatal(err)
	}
	if wd, err := os.Getwd(); err != nil || wd != dir {
		t.Fatalf("working directory = %q (err %v), want %q", wd, err, dir)
	}
}

func TestPrepareDaemonConfigInteractiveRejectsInvalidConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("routeros: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	if _, _, err := prepareTestDaemonConfig(t, path, false); err == nil {
		t.Fatal("interactive daemon start must not replace an unloadable configuration")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "routeros: [\n" {
		t.Fatalf("interactive daemon start modified the existing configuration: %q", raw)
	}
	if matches, _ := filepath.Glob(path + ".invalid-*"); len(matches) != 0 {
		t.Fatalf("interactive daemon start quarantined the configuration: %v", matches)
	}
}

func TestPrepareDaemonConfigServiceModeQuarantinesInvalidConfiguration(t *testing.T) {
	clearDefaultConfigEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("routeros: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	cfg, recovery, err := prepareTestDaemonConfig(t, path, true)
	if err != nil {
		t.Fatal(err)
	}
	if recovery == nil || recovery.Quarantined == "" || !recovery.Created {
		t.Fatalf("recovery = %+v", recovery)
	}
	if _, err := os.Stat(recovery.Quarantined); err != nil {
		t.Fatalf("quarantined configuration missing: %v", err)
	}
	if cfg == nil || len(cfg.Items) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestPrepareDaemonConfigRequiresLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	if _, _, err := prepareDaemonConfig(path, false, nil); err == nil {
		t.Fatal("configuration recovery accepted a missing instance lock")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("configuration recovery created a directory without a lock: %v", err)
	}
}

func TestPrepareDaemonConfigRechecksRecoveryPathsUnderLock(t *testing.T) {
	clearDefaultConfigEnv(t)
	path := writeConfig(t, commandConfig("https://router.invalid", "127.0.0.1:8080"))
	t.Chdir(t.TempDir())
	lockPath := filepath.Join(filepath.Dir(path), "instance.lock")
	// This variable does not affect the existing config, but will affect the
	// default if that config becomes unloadable after the initial read.
	t.Setenv("ROS_DDNS_STATE_PATH", lockPath)
	selected, err := daemonConfigLockPath(path, lockPath, true)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := instancelock.Acquire(selected, instancelock.Info{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	broken := []byte("routeros: [\n")
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareDaemonConfig(path, true, lock); err == nil || !strings.Contains(err.Error(), "same file") {
		t.Fatalf("recovery under a conflicting held lock: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, broken) {
		t.Fatalf("recovery changed the config: %q, %v", got, err)
	}
	if matches, err := filepath.Glob(path + ".invalid-*"); err != nil || len(matches) != 0 {
		t.Fatalf("recovery quarantined the config: %v, %v", matches, err)
	}
}

func TestDaemonConfigLockPathPreservesSymlinkTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink/.. traversal follows different Windows path semantics")
	}
	clearDefaultConfigEnv(t)
	base := t.TempDir()
	dir, target := filepath.Join(base, "service"), filepath.Join(base, "target", "nested")
	for _, name := range []string{dir, target} {
		if err := os.MkdirAll(name, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Chdir(base)
	path := filepath.Join(dir, "config.yaml")
	got, err := daemonConfigLockPath(path, "link/../instance.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "target", "instance.lock")
	if got != want {
		t.Fatalf("lock path = %q, want symlink-resolved %q", got, want)
	}
}
