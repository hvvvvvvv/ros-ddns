package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ros-ddns/internal/atomicfile"
	"ros-ddns/internal/config"
	"ros-ddns/internal/instancelock"
	"ros-ddns/internal/pathguard"
)

var errPrepareDefaultConfig = errors.New("prepare default configuration")

// daemonConfig reports both the effective startup settings and any attempt to
// persist their fallbacks. WriteErr never prevents use of Config in memory.
type daemonConfig struct {
	Config         *config.Config
	Fixups         []config.Fixup
	CreatedDefault bool
	Written        bool
	WriteErr       error
}

// daemonConfigLockPath checks every writable path before acquiring a lock or
// repairing configuration. Only a missing config uses the generated default's
// deterministic fallback. Malformed existing files are fatal in both modes.
func daemonConfigLockPath(absConfigPath, explicit string, serviceManaged bool) (string, error) {
	if serviceManaged {
		if err := useConfigDirectory(absConfigPath, true); err != nil {
			return "", err
		}
	}
	raw, readErr := os.ReadFile(absConfigPath)
	statePath := filepath.Join(filepath.Dir(absConfigPath), "state.json")
	if readErr == nil {
		startup, err := config.ParseStartup(raw, absConfigPath)
		if err != nil {
			return "", err
		}
		statePath = startup.Config.State.Path
	} else if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("read config: %w", readErr)
	}
	lockPath, err := instanceLockPath(explicit, statePath, absConfigPath)
	if err != nil {
		return "", err
	}
	lockPath, err = pathguard.Canonical(daemonPath(lockPath, absConfigPath, serviceManaged))
	if err != nil {
		return "", fmt.Errorf("cannot resolve instance lock path")
	}
	if readErr != nil {
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

// prepareDaemonConfig loads and repairs configuration while the caller holds
// the instance lock. Only missing files are replaced with the embedded default.
// Service-relative paths resolve against the configuration directory.
func prepareDaemonConfig(absConfigPath string, serviceManaged bool, lock *instancelock.Lock) (*daemonConfig, error) {
	if lock == nil {
		return nil, fmt.Errorf("%w: instance lock is required", errPrepareDefaultConfig)
	}
	if serviceManaged {
		if err := useConfigDirectory(absConfigPath, true); err != nil {
			return nil, err
		}
	}
	result := &daemonConfig{}
	raw, err := os.ReadFile(absConfigPath)
	if os.IsNotExist(err) {
		// Recheck before creating the default, including environment overrides.
		if err := pathguard.Validate(absConfigPath, recoveryStatePath(absConfigPath, serviceManaged), lock.Path()); err != nil {
			return nil, err
		}
		if err := writeDefaultConfig(absConfigPath); err != nil {
			return nil, fmt.Errorf("%w: %w", errPrepareDefaultConfig, err)
		}
		result.CreatedDefault, result.Written = true, true
		if serviceManaged {
			if err := useConfigDirectory(absConfigPath, false); err != nil {
				return nil, err
			}
		}
		raw, err = os.ReadFile(absConfigPath)
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	startup, err := config.ParseStartup(raw, absConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}
	result.Config, result.Fixups = startup.Config, startup.Fixups
	if err := pathguard.Validate(absConfigPath, result.Config.State.Path, lock.Path()); err != nil {
		return nil, err
	}
	if startup.NeedsWrite() {
		result.WriteErr = writeStartupFixups(absConfigPath, raw, startup.Fixups)
		if result.WriteErr == nil {
			result.Written = true
		}
	}
	return result, nil
}

// writeStartupFixups is called under the instance lock. Re-read before backing
// up and again before replacement to preserve edits made by external tools.
func writeStartupFixups(path string, raw []byte, fixups []config.Fixup) error {
	unchanged := func() error {
		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("re-read configuration before writeback: %w", err)
		}
		if !bytes.Equal(current, raw) {
			return errors.New("configuration changed externally; skipped startup writeback")
		}
		return nil
	}
	if err := unchanged(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect configuration permissions: %w", err)
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0600
	}
	if err := atomicfile.Write(path+".bak", raw, mode); err != nil {
		return fmt.Errorf("back up configuration before writeback: %w", err)
	}
	updated, err := config.ApplyStartupFixups(raw, fixups)
	if err != nil {
		return fmt.Errorf("apply startup fixups: %w", err)
	}
	verified, err := config.ParseStartup(updated, path)
	if err != nil {
		return fmt.Errorf("verify startup fixups: %w", err)
	}
	if verified.NeedsWrite() {
		return errors.New("verify startup fixups: writable fallbacks remain")
	}
	if err := unchanged(); err != nil {
		return err
	}
	if err := atomicfile.Write(path, updated, mode); err != nil {
		return fmt.Errorf("write startup fixups: %w", err)
	}
	return nil
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
	if err := atomicfile.Write(path, content, 0600); err != nil {
		return fmt.Errorf("write default configuration: %w", err)
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
