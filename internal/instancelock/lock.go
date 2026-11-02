// Package instancelock prevents cooperating processes from running against the
// same lock file. The OS lock is authoritative; the JSON is diagnostic only.
package instancelock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrLocked means another instance already holds the requested lock.
var ErrLocked = errors.New("another instance holds the lock")

// Info describes the most recent holder. It must not contain credentials.
type Info struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"startedAt"`
	ConfigPath string    `json:"configPath"`
	Version    string    `json:"version"`
}

// Lock owns an open file and its OS lock. A Lock must not be copied.
type Lock struct {
	file *os.File
	path string
	once sync.Once
	err  error
}

// Path returns the path supplied to Acquire.
func (l *Lock) Path() string { return l.path }

// Close releases the OS lock and closes the file, even if unlocking fails.
// It is safe to call more than once, including concurrently. The file is never
// removed: unlinking it could let Unix processes lock different inodes at the
// same path. A process exit also releases the OS lock, regardless of the JSON.
func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.file != nil {
			l.err = errors.Join(unlockFile(l.file), l.file.Close())
		}
	})
	return l.err
}

// Acquire takes a nonblocking exclusive OS lock before replacing the diagnostic
// JSON. Existing JSON (including a stale PID) never prevents acquisition.
func Acquire(path string, info Info) (*Lock, error) {
	if path == "" {
		return nil, errors.New("instance lock path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create instance lock directory: %w", err)
	}
	// Do not use O_TRUNC: a contender must leave the holder's JSON intact.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock %q: %w", path, err)
	}
	if err := lockFile(file); err != nil {
		if errors.Is(err, ErrLocked) {
			err = conflictError(path, file)
		} else {
			err = fmt.Errorf("acquire instance lock %q: %w", path, err)
		}
		return nil, errors.Join(err, file.Close())
	}
	lock := &Lock{file: file, path: path}
	if err := file.Chmod(0600); err != nil {
		return nil, errors.Join(fmt.Errorf("chmod instance lock %q: %w", path, err), lock.Close())
	}
	if err := writeInfo(file, info); err != nil {
		return nil, errors.Join(fmt.Errorf("write instance lock %q: %w", path, err), lock.Close())
	}
	return lock, nil
}

func writeInfo(file *os.File, info Info) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.WriteAt(append(data, '\n'), 0); err != nil {
		return err
	}
	return file.Sync()
}

func conflictError(path string, file *os.File) error {
	var holder Info
	// A crash or a concurrent acquisition can leave incomplete JSON. Read only a
	// bounded diagnostic payload, and report contention even when it is unreadable.
	if err := json.NewDecoder(io.LimitReader(file, 64*1024)).Decode(&holder); err == nil {
		return fmt.Errorf("instance lock %q: %w (pid=%d, startedAt=%s, configPath=%q)",
			path, ErrLocked, holder.PID, holder.StartedAt.Format(time.RFC3339Nano), holder.ConfigPath)
	}
	return fmt.Errorf("instance lock %q: %w (holder info unavailable)", path, ErrLocked)
}
