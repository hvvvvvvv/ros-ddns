package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerRejectsPathCollisionsBeforeConstructionOrWrites(t *testing.T) {
	for _, target := range []string{"config", "backup", "lock"} {
		t.Run(target, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), "initial.lock")
			if err := os.WriteFile(lockPath, []byte("held lock sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			f := newFixture(t, Options{LockPath: lockPath})
			paths := map[string]string{"config": f.cfg.Path, "backup": f.cfg.Path + ".bak", "lock": lockPath}
			if err := os.WriteFile(f.cfg.Path+".bak", []byte("backup sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			original := f.m.svc
			hash, data := editDocument(t, f.m, func(d map[string]any) { d["state"].(map[string]any)["path"] = paths[target] })
			if err := f.m.ValidateConfig(data); err == nil || !strings.Contains(err.Error(), "same file") {
				t.Fatalf("validate: %v", err)
			}
			if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err == nil || !strings.Contains(err.Error(), "same file") {
				t.Fatalf("apply: %v", err)
			}
			if f.m.svc != original {
				t.Fatal("invalid edit replaced service")
			}
			assertFile(t, f.cfg.Path, f.raw)
			assertFile(t, f.cfg.Path+".bak", []byte("backup sentinel"))
			assertFile(t, lockPath, []byte("held lock sentinel"))
			cfg := cloneConfig(f.cfg)
			cfg.State.Path = paths[target]
			if _, err := NewManager(cfg, f.m.deps, Options{LockPath: lockPath}); err == nil {
				t.Fatal("constructor accepted collision")
			}
		})
	}
}
