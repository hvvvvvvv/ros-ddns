package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"ros-ddns/internal/atomicfile"
)

// readOriginal rechecks the hash after candidate construction. An editor may
// have changed the file while factories were running. Bytes and mode come from
// the same open file, and backup/rollback always use these exact bytes.
func readOriginal(path, hash string) ([]byte, os.FileMode, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("config path must be a regular file")
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, 0, ErrConfigConflict
	}
	return raw, info.Mode().Perm(), nil
}

// replaceFile stages bytes in the destination directory, applies permissions,
// syncs and closes before renaming. It never truncates the destination in place.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	return atomicfile.Write(path, data, mode)
}
