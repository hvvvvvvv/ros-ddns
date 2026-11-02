//go:build !windows

package instancelock

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(file *os.File) error {
	err := flock(file, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrLocked
	}
	return err
}

func unlockFile(file *os.File) error { return flock(file, syscall.LOCK_UN) }

func flock(file *os.File, operation int) error {
	for {
		err := syscall.Flock(int(file.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
