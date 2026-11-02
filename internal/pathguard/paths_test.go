package pathguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateAllPairsWithoutWrites(t *testing.T) {
	labels := []string{"config", "backup", "state", "lock"}
	for _, pair := range [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}} {
		t.Run(labels[pair[0]]+"_"+labels[pair[1]], func(t *testing.T) {
			dir := t.TempDir()
			paths := []string{filepath.Join(dir, "config.yaml"), filepath.Join(dir, "config.yaml.bak"), filepath.Join(dir, "state.json"), filepath.Join(dir, "instance.lock")}
			for _, path := range paths {
				if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if pair == [2]int{0, 1} {
				if err := os.Remove(paths[1]); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(paths[0], paths[1]); err != nil {
					t.Skipf("hard links unavailable: %v", err)
				}
			} else {
				paths[pair[1]] = paths[pair[0]]
			}
			if err := Validate(paths[0], paths[2], paths[3]); err == nil || !strings.Contains(err.Error(), "same file") {
				t.Fatalf("collision accepted: %v", err)
			}
			for _, path := range paths {
				if data, err := os.ReadFile(path); err != nil || string(data) != "unchanged" {
					t.Fatalf("validation modified a target: %v", err)
				}
			}
		})
	}
}

func TestRelativeAndMissingIdentities(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	config := filepath.Join(dir, "config.yaml")
	for _, path := range []string{"config.yaml", "./config.yaml", "sub/../config.yaml"} {
		if err := Validate(config, path, ""); err == nil {
			t.Fatalf("relative collision accepted: %s", path)
		}
	}
	if err := Validate(config, "missing/state.json", "missing/./state.json"); err == nil {
		t.Fatal("missing path collision accepted")
	}
	if err := Validate(config, "missing/state.json", "missing/instance.lock"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("validation created files or directories: %v, %v", entries, err)
	}
}

func TestSymlinkIdentities(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, suffix := range []string{"config.yaml", "missing/state.json"} {
		if err := Validate(filepath.Join(real, suffix), filepath.Join(alias, suffix), ""); err == nil {
			t.Fatal("symlinked parent collision accepted")
		}
	}
	path := filepath.Join(real, "config.yaml")
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := Validate(path, link, ""); err == nil {
		t.Fatal("dangling link collision accepted")
	}
	if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Validate(path, link, ""); err == nil {
		t.Fatal("file symlink collision accepted")
	}
	if runtime.GOOS != "windows" {
		childLink := filepath.Join(dir, "child-link")
		if err := os.Symlink(filepath.Join(real, "child"), childLink); err != nil {
			t.Fatal(err)
		}
		// Deliberately do not filepath.Join away the .. before resolving links.
		if err := Validate(path, childLink+"/../config.yaml", ""); err == nil {
			t.Fatal("symlink followed by .. collision accepted")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("validation changed symlink target: %v", err)
	}
}

func TestCaseIdentity(t *testing.T) {
	dir := t.TempDir()
	lower, upper := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "CONFIG.yaml")
	err := Validate(lower, upper, "")
	if (err != nil) != (runtime.GOOS == "windows") {
		t.Fatalf("case collision on %s: %v", runtime.GOOS, err)
	}
	first, err := Canonical(lower)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Canonical(upper)
	if err != nil {
		t.Fatal(err)
	}
	if (first == second) != (runtime.GOOS == "windows") {
		t.Fatalf("canonical case handling on %s", runtime.GOOS)
	}
}

func TestIdentityErrorsDoNotExposePaths(t *testing.T) {
	const secret = "credential-in-path"
	err := Validate("", secret+"\x00", "")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe identity error: %v", err)
	}
}
