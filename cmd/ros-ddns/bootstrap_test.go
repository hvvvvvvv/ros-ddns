package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ros-ddns/internal/config"
)

func TestRecoverConfigCreatesDefault(t *testing.T) {
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

func TestRecoverConfigQuarantinesUnloadableFile(t *testing.T) {
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
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	cfg, recovery, err := prepareDaemonConfig(path, false)
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
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	if _, _, err := prepareDaemonConfig(path, true); err != nil {
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

	if _, _, err := prepareDaemonConfig(path, false); err == nil {
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

	cfg, recovery, err := prepareDaemonConfig(path, true)
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
