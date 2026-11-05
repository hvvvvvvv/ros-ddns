package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/instancelock"
)

func prepareTestDaemonConfig(t *testing.T, path string, serviceManaged bool) (*daemonConfig, error) {
	t.Helper()
	if serviceManaged {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })
	}
	lockPath, err := daemonConfigLockPath(path, "", serviceManaged)
	if err != nil {
		return nil, err
	}
	lock, err := instancelock.Acquire(lockPath, instancelock.Info{PID: os.Getpid()})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = os.Remove(lockPath) })
	defer lock.Close()
	return prepareDaemonConfig(path, serviceManaged, lock)
}

func clearDefaultConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ROS_DDNS_LISTEN", "ROS_DDNS_API_TOKEN", "ROS_DDNS_ROUTEROS_URL",
		"ROS_DDNS_ROUTEROS_USERNAME", "ROS_PASSWORD", "ROS_DDNS_CA_FILE",
		"ALI_ACCESS_KEY_ID", "ALI_ACCESS_KEY_SECRET", "ALI_SECURITY_TOKEN",
		"ROS_DDNS_INTERFACE", "ROS_DDNS_DOMAIN", "ROS_DDNS_LOG_LEVEL",
		"ROS_DDNS_LOG_FORMAT", "ROS_DDNS_POLL_INTERVAL", "ROS_DDNS_STABLE_FOR",
		"ROS_DDNS_RECONCILE_TIMEOUT", "ROS_DDNS_VERIFY_INTERVAL", "ROS_DDNS_SYNC_ON_START",
		"ROS_DDNS_MAX_BACKOFF", "ROS_DDNS_STATE_PATH",
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

func TestWriteDefaultConfigCreatesDefault(t *testing.T) {
	clearDefaultConfigEnv(t)
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "config.yaml")
	if err := writeDefaultConfig(path); err != nil {
		t.Fatal(err)
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
	if want := filepath.ToSlash(filepath.Join(dir, "state.json")); cfg.State.Path != want {
		t.Fatalf("state.path = %q, want %q", cfg.State.Path, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("generated configuration permissions: %v, %v", info, err)
		}
	}
}

func TestGeneratedStatePathEnvOverride(t *testing.T) {
	clearDefaultConfigEnv(t)
	t.Setenv("ROS_DDNS_STATE_PATH", "custom/state.json")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeDefaultConfig(path); err != nil {
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

func TestPrepareDaemonConfigCreatesMissingDefault(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", serviceManaged), func(t *testing.T) {
			clearDefaultConfigEnv(t)
			dir := filepath.Join(t.TempDir(), "nested")
			path := filepath.Join(dir, "config.yaml")
			original, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			result, err := prepareTestDaemonConfig(t, path, serviceManaged)
			if err != nil {
				t.Fatal(err)
			}
			if !result.CreatedDefault || !result.Written || result.WriteErr != nil || len(result.Fixups) != 0 {
				t.Fatalf("startup = %+v", result)
			}
			cfg := result.Config
			if cfg.API.Listen != "127.0.0.1:8080" || len(cfg.Items) != 0 {
				t.Fatalf("loaded default = api %q items %d", cfg.API.Listen, len(cfg.Items))
			}
			if want := filepath.ToSlash(filepath.Join(dir, "state.json")); cfg.State.Path != want {
				t.Fatalf("state.path = %q, want %q", cfg.State.Path, want)
			}
			wantWD := original
			if serviceManaged {
				wantWD = dir
			}
			if wd, err := os.Getwd(); err != nil || wd != wantWD {
				t.Fatalf("working directory = %q (err %v), want %q", wd, err, wantWD)
			}
		})
	}
}

func TestPrepareDaemonConfigRepairsRequiredValues(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", serviceManaged), func(t *testing.T) {
			raw := "# retain this comment\nstate: {path: \"\"}\nlog: {level: verbose}\napi: {listen: \"\"}\n"
			path := writeConfig(t, raw)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := prepareTestDaemonConfig(t, path, serviceManaged)
			if err != nil {
				t.Fatal(err)
			}
			if result.CreatedDefault || !result.Written || result.WriteErr != nil || len(result.Fixups) != 2 {
				t.Fatalf("startup = %+v", result)
			}
			if result.Config.Log.Level != "info" || result.Config.API.Listen != "127.0.0.1:8080" {
				t.Fatalf("effective config = %+v", result.Config)
			}
			want := map[string]string{"log.level": "info", "api.listen": "127.0.0.1:8080"}
			for _, fixup := range result.Fixups {
				if fixup.Value != want[fixup.Path] || !fixup.Write || fixup.Secret || fixup.Reason == "" {
					t.Fatalf("fixup = %+v", fixup)
				}
				delete(want, fixup.Path)
			}
			if len(want) != 0 {
				t.Fatalf("missing fixups: %v", want)
			}
			if backup, err := os.ReadFile(path + ".bak"); err != nil || string(backup) != raw {
				t.Fatalf("backup = %q, %v", backup, err)
			}
			updated, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(updated, []byte("# retain this comment")) {
				t.Fatalf("updated configuration = %q, %v", updated, err)
			}
			parsed, err := config.ParseStartup(updated, path)
			if err != nil || len(parsed.Fixups) != 0 {
				t.Fatalf("writeback still needs repair: %+v, %v", parsed, err)
			}
			for _, name := range []string{path, path + ".bak"} {
				if info, err := os.Stat(name); err != nil || info.Mode().Perm() != before.Mode().Perm() {
					t.Fatalf("writeback changed permissions of %s: %v, %v", name, info, err)
				}
			}
		})
	}
}

func TestPrepareDaemonConfigKeepsInvalidEnvironmentPlaceholders(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", serviceManaged), func(t *testing.T) {
			t.Setenv("STARTUP_LOG_LEVEL", "verbose")
			t.Setenv("STARTUP_LISTEN", "")
			raw := "state: {path: \"\"}\nlog: {level: '${STARTUP_LOG_LEVEL}'}\napi: {listen: '${STARTUP_LISTEN}'}\n"
			path := writeConfig(t, raw)
			result, err := prepareTestDaemonConfig(t, path, serviceManaged)
			if err != nil {
				t.Fatal(err)
			}
			if result.Written || result.CreatedDefault || result.WriteErr != nil || len(result.Fixups) != 2 {
				t.Fatalf("startup = %+v", result)
			}
			if result.Config.Log.Level != "info" || result.Config.API.Listen != "127.0.0.1:8080" {
				t.Fatalf("effective config = %+v", result.Config)
			}
			for _, fixup := range result.Fixups {
				if fixup.Write || fixup.Reason == "" {
					t.Fatalf("environment fixup = %+v", fixup)
				}
			}
			assertUnchangedStartupFile(t, path, raw)
		})
	}
}

func assertUnchangedStartupFile(t *testing.T, path, raw string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != raw {
		t.Fatalf("startup changed the config: %q, %v", got, err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("startup unexpectedly created a backup: %v", err)
	}
	if matches, err := filepath.Glob(path + ".invalid-*"); err != nil || len(matches) != 0 {
		t.Fatalf("startup quarantined the config: %v, %v", matches, err)
	}
}

func TestPrepareDaemonConfigRejectsMalformedConfiguration(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		for _, tc := range []struct{ name, raw, message string }{
			{"syntax", "routeros: [\n", "parse YAML"},
			{"unknown field", "runtime: {unknown: true}\n", "unknown"},
			{"type", "runtime: {syncOnStart: [true]}\n", "parse YAML"},
			{"duration type", "runtime: {pollInterval: wrong}\n", "duration"},
			{"listen", "api: {listen: invalid}\n", "api.listen"},
		} {
			t.Run(fmt.Sprintf("service=%t/%s", serviceManaged, tc.name), func(t *testing.T) {
				path := writeConfig(t, tc.raw)
				if _, err := prepareTestDaemonConfig(t, path, serviceManaged); err == nil || !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("startup error = %v, want %q", err, tc.message)
				}
				assertUnchangedStartupFile(t, path, tc.raw)
			})
		}
	}
}

func TestPrepareDaemonConfigGeneratesPublicAPIToken(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", serviceManaged), func(t *testing.T) {
			raw := "state: {path: \"\"}\napi: {listen: '0.0.0.0:8080', token: ''}\n"
			path := writeConfig(t, raw)
			result, err := prepareTestDaemonConfig(t, path, serviceManaged)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := hex.DecodeString(result.Config.API.Token)
			if err != nil || len(decoded) != 32 || !result.Written || result.WriteErr != nil {
				t.Fatalf("token bytes=%d error=%v startup=%+v", len(decoded), err, result)
			}
			if len(result.Fixups) != 1 || !result.Fixups[0].Secret || !result.Fixups[0].Write || result.Fixups[0].Value != result.Config.API.Token {
				t.Fatalf("token fixups = %+v", result.Fixups)
			}
			cfg, err := config.Load(path)
			if err != nil || cfg.API.Token != result.Config.API.Token {
				t.Fatalf("persisted token differs: %v", err)
			}
			if backup, err := os.ReadFile(path + ".bak"); err != nil || string(backup) != raw {
				t.Fatalf("backup = %q, %v", backup, err)
			}
		})
	}
}

func TestPrepareDaemonConfigWriteFailureIsNonFatal(t *testing.T) {
	path := writeConfig(t, "state: {path: \"\"}\nlog: {level: verbose}\n")
	if err := os.Mkdir(path+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	result, err := prepareTestDaemonConfig(t, path, false)
	if err != nil || result.WriteErr == nil || result.Written || result.Config.Log.Level != "info" {
		t.Fatalf("write failure must retain in-memory defaults: %+v, %v", result, err)
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Contains(raw, []byte("verbose")) {
		t.Fatalf("write failure changed config: %q, %v", raw, err)
	}
}

func TestWriteStartupFixupsPreservesConcurrentEdit(t *testing.T) {
	raw := "state: {path: \"\"}\nlog: {level: verbose}\n"
	path := writeConfig(t, raw)
	startup, err := config.ParseStartup([]byte(raw), path)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := instancelock.Acquire(filepath.Join(filepath.Dir(path), "instance.lock"), instancelock.Info{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	edited := raw + "# external edit\n"
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeStartupFixups(path, []byte(raw), startup.Fixups); err == nil || !strings.Contains(err.Error(), "changed externally") {
		t.Fatalf("concurrent edit was not detected: %v", err)
	}
	assertUnchangedStartupFile(t, path, edited)
}

func TestPrepareDaemonConfigRequiresLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	if _, err := prepareDaemonConfig(path, false, nil); err == nil {
		t.Fatal("configuration writeback accepted a missing instance lock")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("configuration writeback created a directory without a lock: %v", err)
	}
}

func TestPrepareDaemonConfigRejectsDirectory(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", serviceManaged), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareTestDaemonConfig(t, path, serviceManaged); err == nil {
				t.Fatal("configuration directory was accepted")
			}
			if files, err := os.ReadDir(path); err != nil || len(files) != 0 {
				t.Fatalf("configuration directory changed: %v, %v", files, err)
			}
		})
	}
}

func TestPrepareDaemonConfigRechecksMissingFilePathsUnderLock(t *testing.T) {
	clearDefaultConfigEnv(t)
	path := writeConfig(t, "state: {path: ''}\n")
	t.Chdir(t.TempDir())
	lockPath := filepath.Join(filepath.Dir(path), "instance.lock")
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
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDaemonConfig(path, true, lock); err == nil || !strings.Contains(err.Error(), "same file") {
		t.Fatalf("default write with conflicting state/held lock: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("conflicting default was written: %v", err)
	}
}

func TestWriteStartupFixupsRejectsIncompleteRepair(t *testing.T) {
	raw := "state: {path: ''}\nlog: {level: verbose}\n"
	path := writeConfig(t, raw)
	lock, err := instancelock.Acquire(filepath.Join(filepath.Dir(path), "instance.lock"), instancelock.Info{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// An incomplete repair must never replace the original file even if its
	// generated YAML is well formed and ParseStartup could repair it again.
	fixups := []config.Fixup{{Path: "log.level", Value: "still-invalid", Write: true}}
	if err := writeStartupFixups(path, []byte(raw), fixups); err == nil || !strings.Contains(err.Error(), "writable fallbacks remain") {
		t.Fatalf("incomplete repair verification: %v", err)
	}
	for _, name := range []string{path, path + ".bak"} {
		if got, err := os.ReadFile(name); err != nil || string(got) != raw {
			t.Fatalf("original bytes lost in %s: %q, %v", name, got, err)
		}
	}
}

func TestPrepareDaemonConfigRechecksConfigurationUnderLock(t *testing.T) {
	for _, serviceManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", serviceManaged), func(t *testing.T) {
			path := writeConfig(t, "state: {path: \"\"}\n")
			t.Chdir(t.TempDir())
			selected, err := daemonConfigLockPath(path, filepath.Join(filepath.Dir(path), "instance.lock"), serviceManaged)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := instancelock.Acquire(selected, instancelock.Info{PID: os.Getpid()})
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			broken := "routeros: [\n"
			if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareDaemonConfig(path, serviceManaged, lock); err == nil || !strings.Contains(err.Error(), "parse YAML") {
				t.Fatalf("malformed config under lock: %v", err)
			}
			assertUnchangedStartupFile(t, path, broken)
		})
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
	if want := filepath.Join(base, "target", "instance.lock"); got != want {
		t.Fatalf("lock path = %q, want symlink-resolved %q", got, want)
	}
}
