package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"ros-ddns/internal/config"
)

// configRecovery describes how a daemon start handled a configuration that
// could not be loaded.
type configRecovery struct {
	Created     bool
	Quarantined string
	LoadErr     error
}

// recoverConfig makes path loadable for a daemon start.
//
// A missing file is created from the embedded default configuration. An
// existing but unloadable file is first preserved with an .invalid-<timestamp>
// suffix so operator data is never lost, then replaced by the default. The
// default contains only placeholders and no credentials.
func recoverConfig(path string, cause error) (*configRecovery, error) {
	recovery := &configRecovery{LoadErr: cause}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("configuration path %s is not a regular file", path)
		}
		preserved := path + ".invalid-" + time.Now().Format("20060102150405")
		for attempt := 1; ; attempt++ {
			if _, statErr := os.Lstat(preserved); os.IsNotExist(statErr) {
				break
			}
			preserved = fmt.Sprintf("%s.invalid-%s-%d", path, time.Now().Format("20060102150405"), attempt)
		}
		if err := os.Rename(path, preserved); err != nil {
			return nil, fmt.Errorf("preserve unloadable configuration: %w", err)
		}
		recovery.Quarantined = preserved
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("inspect configuration: %w", err)
	}
	if err := writeDefaultConfig(path); err != nil {
		return nil, err
	}
	recovery.Created = true
	return recovery, nil
}

// prepareDaemonConfig loads the configuration for any daemon start. A missing
// configuration file is replaced with the embedded default. A service-managed
// start additionally preserves an unloadable configuration and replaces it with
// the default; an interactive start reports the load error and exits, unless the
// file is missing. chdir selects the service-managed path behavior, where
// relative configuration paths resolve against the configuration directory
// instead of the service manager's working directory.
func prepareDaemonConfig(absConfigPath string, serviceManaged bool) (*config.Config, *configRecovery, error) {
	cfg, loadErr := config.Load(absConfigPath)
	var recovery *configRecovery
	if loadErr != nil {
		missing := false
		if _, statErr := os.Stat(absConfigPath); os.IsNotExist(statErr) {
			missing = true
		}
		if !missing && !serviceManaged {
			return nil, nil, loadErr
		}
		var err error
		recovery, err = recoverConfig(absConfigPath, loadErr)
		if err != nil {
			return nil, nil, fmt.Errorf("prepare default configuration: %w", err)
		}
		cfg, loadErr = config.Load(absConfigPath)
	}
	if loadErr != nil {
		return nil, recovery, fmt.Errorf("load configuration: %w", loadErr)
	}
	if serviceManaged {
		if err := os.Chdir(filepath.Dir(absConfigPath)); err != nil {
			return nil, recovery, fmt.Errorf("use configuration directory: %w", err)
		}
	}
	return cfg, recovery, nil
}

// writeDefaultConfig atomically installs the embedded default configuration
// with mode 0600, creating the parent directory (0700) when necessary. The
// state path is written as an absolute path below the configuration directory so
// the first start works from any working directory.
func writeDefaultConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	content, err := defaultConfigWithStatePath(filepath.Join(dir, "state.json"))
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".ros-ddns-default-*.tmp")
	if err != nil {
		return fmt.Errorf("create default configuration: %w", err)
	}
	name := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(name)
	}()
	if err := temp.Chmod(0600); err != nil {
		return fmt.Errorf("set default configuration mode: %w", err)
	}
	if _, err := temp.Write(content); err != nil {
		return fmt.Errorf("write default configuration: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync default configuration: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close default configuration: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("install default configuration: %w", err)
	}
	return nil
}

// defaultConfigWithStatePath substitutes the absolute state path into the
// embedded template. The template keeps a relative placeholder so it stays
// parseable on its own (see config.TestDefaultConfigYAML).
func defaultConfigWithStatePath(statePath string) ([]byte, error) {
	marker := []byte("  path: state.json\n")
	raw := config.DefaultConfigYAML()
	if !bytes.Contains(raw, marker) {
		return nil, fmt.Errorf("default configuration template is missing its state path")
	}
	quoted := strconv.Quote(filepath.ToSlash(statePath))
	return bytes.Replace(raw, marker, []byte("  path: "+quoted+"\n"), 1), nil
}
