// Package atomicfile writes files via a temporary file in the destination directory.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write stages data in the destination directory, applies mode before writing,
// then syncs and closes the file before renaming it to path. It never truncates
// the destination in place. The parent directory must exist; mode 0 uses 0600.
func Write(path string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0600
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		_ = file.Close()
		// Windows cannot remove a read-only temporary file after a failed rename.
		_ = os.Chmod(name, 0600)
		_ = os.Remove(name)
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
