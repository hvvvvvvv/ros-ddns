package instancelock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAcquireLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "nested", "state.json.lock")
	info := Info{PID: os.Getpid(), StartedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"), Version: "test-version"}
	first, err := Acquire(path, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLock(t, first) })
	if first.Path() != path {
		t.Fatalf("Path() = %q, want %q", first.Path(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Info
	if err := json.Unmarshal(raw, &got); err != nil || got != info {
		t.Fatalf("info = %+v, error = %v, want %+v", got, err, info)
	}
	if runtime.GOOS != "windows" {
		for _, tc := range []struct {
			path string
			mode os.FileMode
		}{{path, 0600}, {filepath.Dir(path), 0700}} {
			stat, err := os.Stat(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if stat.Mode().Perm() != tc.mode {
				t.Errorf("mode of %q = %o, want %o", tc.path, stat.Mode().Perm(), tc.mode)
			}
		}
	}
	second, err := Acquire(path, Info{PID: -1})
	if second != nil {
		closeLock(t, second)
		t.Fatal("second Acquire succeeded while the first lock was held")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire error = %v, want ErrLocked", err)
	}
	for _, diagnostic := range []string{fmt.Sprintf("pid=%d", info.PID), info.StartedAt.Format(time.RFC3339Nano), fmt.Sprintf("%q", info.ConfigPath)} {
		if !strings.Contains(err.Error(), diagnostic) {
			t.Errorf("conflict %q does not contain %q", err, diagnostic)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("contender changed holder info: error=%v, data=%q", err, after)
	}
	closeLock(t, first)
	closeLock(t, first) // Idempotent; retained diagnostics are not an active lock.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Close removed the diagnostic file: %v", err)
	}
	info.ConfigPath, info.Version = "c", "v"
	third, err := Acquire(path, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLock(t, third) })
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &got); err != nil || got != info {
		t.Fatalf("replacement info = %+v, error = %v, want %+v (old JSON must be truncated)", got, err, info)
	}
}

func TestAcquireEmptyPath(t *testing.T) {
	lock, err := Acquire("", Info{})
	if lock != nil || err == nil || !strings.Contains(err.Error(), "path must not be empty") || errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire(empty) = %v, %v", lock, err)
	}
}

func TestAcquireWriteFailureReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	// time.Time outside JSON's supported year range fails after OS acquisition.
	lock, err := Acquire(path, Info{StartedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if lock != nil || err == nil || errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire(invalid info) = %v, %v", lock, err)
	}
	lock, err = Acquire(path, Info{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("failed acquisition leaked OS lock: %v", err)
	}
	closeLock(t, lock)
}

func TestAcquireAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process.lock")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLockSubprocess$")
	cmd.Env = append(os.Environ(), "ROS_DDNS_TEST_LOCK_HELPER_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	lock, err := Acquire(path, Info{PID: os.Getpid()})
	if lock != nil {
		closeLock(t, lock)
		t.Fatal("acquired a lock held by another process")
	}
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), fmt.Sprintf("pid=%d", cmd.Process.Pid)) {
		t.Fatalf("cross-process conflict = %v", err)
	}
	// EOF asks the child to exit without Close, as on abrupt process termination.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("helper exit: %v; stderr=%s", err, stderr.String())
	}
	lock, err = Acquire(path, Info{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("process exit did not release OS lock: %v", err)
	}
	closeLock(t, lock)
}

func TestLockSubprocess(t *testing.T) {
	path := os.Getenv("ROS_DDNS_TEST_LOCK_HELPER_PATH")
	if path == "" {
		return
	}
	lock, err := Acquire(path, Info{PID: os.Getpid(), StartedAt: time.Now().UTC(), ConfigPath: "child.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	runtime.KeepAlive(lock)
	os.Exit(0) // Deliberately bypass Close to test kernel cleanup.
}

func closeLock(t *testing.T, lock *Lock) {
	t.Helper()
	if err := lock.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
