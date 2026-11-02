package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/instancelock"
	"ros-ddns/internal/pathguard"
)

var errPrepareDefaultConfig = errors.New("prepare default configuration")

// configRecovery describes how a daemon start handled a configuration that
// could not be loaded.
type configRecovery struct {
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
	return recovery, nil
}

// daemonConfigLockPath checks every writable path before acquiring a lock or
// recovering configuration. An unloadable config cannot supply a lock path, so
// use the generated default's deterministic fallback, even if the environment
// will override its state path. The effective lock is checked again after load.
func daemonConfigLockPath(absConfigPath, explicit string, serviceManaged bool) (string, error) {
	if serviceManaged {
		if err := useConfigDirectory(absConfigPath, true); err != nil {
			return "", err
		}
	}
	cfg, loadErr := config.Load(absConfigPath)
	statePath := filepath.Join(filepath.Dir(absConfigPath), "state.json")
	if loadErr == nil {
		statePath = cfg.State.Path
	} else if _, statErr := os.Stat(absConfigPath); !os.IsNotExist(statErr) && !serviceManaged {
		return "", loadErr
	}
	lockPath, err := instanceLockPath(explicit, statePath, absConfigPath)
	if err != nil {
		return "", err
	}
	lockPath, err = pathguard.Canonical(daemonPath(lockPath, absConfigPath, serviceManaged))
	if err != nil {
		return "", fmt.Errorf("cannot resolve instance lock path")
	}
	if loadErr != nil {
		statePath = recoveryStatePath(absConfigPath, serviceManaged)
	}
	if err := pathguard.Validate(absConfigPath, statePath, lockPath); err != nil {
		return "", err
	}
	return lockPath, nil
}

// daemonPath resolves service-relative paths without requiring the directory
// to exist. Interactive paths keep their usual working-directory semantics.
func daemonPath(path, absConfigPath string, serviceManaged bool) string {
	if serviceManaged && path != "" && !filepath.IsAbs(path) {
		// Preserve symlink/.. traversal until pathguard resolves file identity.
		return filepath.Dir(absConfigPath) + string(os.PathSeparator) + path
	}
	return path
}

// recoveryStatePath mirrors the default template's expansion: a set variable,
// including an empty one, overrides the fallback; Config.Normalize trims it.
func recoveryStatePath(absConfigPath string, serviceManaged bool) string {
	statePath, set := os.LookupEnv("ROS_DDNS_STATE_PATH")
	if !set {
		statePath = filepath.Join(filepath.Dir(absConfigPath), "state.json")
	}
	return daemonPath(strings.TrimSpace(statePath), absConfigPath, serviceManaged)
}

func useConfigDirectory(absConfigPath string, allowMissing bool) error {
	err := os.Chdir(filepath.Dir(absConfigPath))
	if err != nil && !(allowMissing && os.IsNotExist(err)) {
		return fmt.Errorf("use configuration directory: %w", err)
	}
	return nil
}

// prepareDaemonConfig loads configuration while the caller holds the lock. A missing
// configuration file is replaced with the embedded default. A service-managed
// start additionally preserves an unloadable configuration and replaces it with
// the default; an interactive start reports the load error and exits, unless the
// file is missing. serviceManaged selects the path behavior, where
// relative configuration paths resolve against the configuration directory
// instead of the service manager's working directory.
func prepareDaemonConfig(absConfigPath string, serviceManaged bool, lock *instancelock.Lock) (*config.Config, *configRecovery, error) {
	if lock == nil {
		return nil, nil, fmt.Errorf("%w: instance lock is required", errPrepareDefaultConfig)
	}
	if serviceManaged {
		if err := useConfigDirectory(absConfigPath, true); err != nil {
			return nil, nil, err
		}
	}
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
		// Recheck immediately before writing: the configuration may have
		// changed since the initial read used to select the held lock.
		if err := pathguard.Validate(absConfigPath, recoveryStatePath(absConfigPath, serviceManaged), lock.Path()); err != nil {
			return nil, nil, err
		}
		var err error
		recovery, err = recoverConfig(absConfigPath, loadErr)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w", errPrepareDefaultConfig, err)
		}
		if serviceManaged {
			if err := useConfigDirectory(absConfigPath, false); err != nil {
				return nil, recovery, err
			}
		}
		cfg, loadErr = config.Load(absConfigPath)
	}
	if loadErr != nil {
		return nil, recovery, fmt.Errorf("load configuration: %w", loadErr)
	}
	return cfg, recovery, nil
}

// writeDefaultConfig atomically installs the embedded default configuration
// with mode 0600, creating the parent directory (0700) when necessary. The
// state path fallback is written as an absolute path below the configuration
// directory so the first start works from any working directory, and
// ROS_DDNS_STATE_PATH can override it at load time.
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

// defaultConfigWithStatePath substitutes the absolute state path as the
// fallback of ROS_DDNS_STATE_PATH in the embedded template. The template keeps
// state.json as its fallback so it stays parseable on its own (see
// config.TestDefaultConfigYAML); the generated file names the absolute path and
// still allows ROS_DDNS_STATE_PATH to override it.
func defaultConfigWithStatePath(statePath string) ([]byte, error) {
	marker := []byte("  path: ${ROS_DDNS_STATE_PATH:-state.json}\n")
	raw := config.DefaultConfigYAML()
	if !bytes.Contains(raw, marker) {
		return nil, fmt.Errorf("default configuration template is missing its state path")
	}
	fallback := "${ROS_DDNS_STATE_PATH:-" + filepath.ToSlash(statePath) + "}"
	return bytes.Replace(raw, marker, []byte("  path: "+strconv.Quote(fallback)+"\n"), 1), nil
}
