// Package pathguard checks file identities without creating or modifying files.
package pathguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var errIdentity = errors.New("cannot resolve file identity")

// Canonical returns an absolute, symlink-resolved name, including for missing
// targets below existing parents. Windows names are folded for stable hashing;
// Unix names retain their case. Errors deliberately omit submitted paths.
// Hard links require SameFile in Validate; they do not have a unique path name.
func Canonical(path string) (string, error) {
	if path == "" {
		return "", errIdentity
	}
	name, err := resolve(path, 0)
	if err != nil {
		return "", errIdentity
	}
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
	}
	return name, nil
}

func resolve(path string, links int) (string, error) {
	if links > 255 {
		return "", errIdentity
	}
	// Resolve before cleaning: on Unix, link/../file must follow the link
	// before processing .., just as opening that path would.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Abs(resolved)
	} else if !os.IsNotExist(err) {
		return "", errIdentity
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// EvalSymlinks also fails for a dangling link. Resolve its destination
		// rather than treating the link itself as a missing ordinary file.
		target, err := os.Readlink(path)
		if err != nil {
			return "", errIdentity
		}
		if !filepath.IsAbs(target) {
			dir, _ := filepath.Split(path)
			target = dir + target
		}
		return resolve(target, links+1)
	}
	separators := string(os.PathSeparator)
	if runtime.GOOS == "windows" {
		separators += "/"
	}
	parent, base := filepath.Split(strings.TrimRight(path, separators))
	if base == "" || parent == path {
		return "", errIdentity
	}
	if parent == "" {
		parent = "."
	}
	resolved, err := resolve(parent, links)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, base), nil
}

// Validate rejects every pairwise collision between the config, its .bak,
// optional state, and effective instance lock. Empty paths are omitted (e.g.
// lock is not yet known during config parsing). It never opens files for write.
func Validate(configPath, statePath, lockPath string) error {
	backup := ""
	if configPath != "" {
		backup = configPath + ".bak"
	}
	paths := []struct {
		label, path, name string
		info              os.FileInfo
	}{
		{label: "config", path: configPath},
		{label: "config backup", path: backup},
		{label: "state.path", path: statePath},
		{label: "instance lock", path: lockPath},
	}
	for i := range paths {
		p := &paths[i]
		if p.path == "" {
			continue
		}
		var err error
		p.name, err = Canonical(p.path)
		if err == nil {
			p.info, err = os.Stat(p.path)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil {
			return fmt.Errorf("file paths: cannot resolve %s", p.label)
		}
		for _, previous := range paths[:i] {
			if previous.path == "" {
				continue
			}
			sameName := p.name == previous.name
			if runtime.GOOS == "windows" {
				sameName = strings.EqualFold(p.name, previous.name)
			}
			if sameName || (p.info != nil && previous.info != nil && os.SameFile(p.info, previous.info)) {
				return fmt.Errorf("file paths: %s and %s refer to the same file", previous.label, p.label)
			}
		}
	}
	return nil
}
