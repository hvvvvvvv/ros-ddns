package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWrite(t *testing.T) {
	for _, mode := range []os.FileMode{0, 0640, 0400} {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%04o/existing=%t", mode, exists), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.yaml")
				// A successful read-only write must also be removable on Windows.
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
				if exists {
					if err := os.WriteFile(path, []byte("longer original contents"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := Write(path, []byte("replacement\n"), mode); err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != "replacement\n" {
					t.Fatalf("contents = %q, error = %v", got, err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				want := mode
				if want == 0 {
					want = 0600
				}
				got := info.Mode().Perm()
				if runtime.GOOS == "windows" {
					// Windows Chmod controls only the read-only attribute.
					got, want = got&0200, want&0200
				}
				if got != want {
					t.Errorf("mode = %04o, want %04o", got, want)
				}
				assertOnlyDestination(t, dir, filepath.Base(path))
			})
		}
	}
}

func TestWriteRenameFailureCleansReadOnlyTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "original")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	err := Write(path, []byte("replacement"), 0400)
	var renameErr *os.LinkError
	if !errors.As(err, &renameErr) || renameErr.Op != "rename" {
		t.Fatalf("error = %v, want rename failure", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("original destination changed: contents = %q, error = %v", got, err)
	}
	assertOnlyDestination(t, dir, filepath.Base(path))
}

func TestWriteRequiresExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "missing")
	err := Write(filepath.Join(parent, "config.yaml"), []byte("contents"), 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want missing-directory error", err)
	}
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("parent directory created: %v", err)
	}
}

func assertOnlyDestination(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("unexpected files after Write: %v", entries)
	}
}
