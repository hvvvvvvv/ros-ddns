//go:build windows

package instancelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(file *os.File) error {
	// Windows locks deny other handles access to the locked bytes. Reserve a
	// single byte at offset 1<<32, beyond the JSON, so contenders can still read
	// holder info. Locking beyond EOF does not extend the file.
	overlapped := windows.Overlapped{OffsetHigh: 1}
	err := windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

func unlockFile(file *os.File) error {
	overlapped := windows.Overlapped{OffsetHigh: 1}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
