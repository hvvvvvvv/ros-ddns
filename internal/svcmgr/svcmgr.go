// Package svcmgr manages the operating-system service that hosts ros-ddns.
//
// It wraps github.com/kardianos/service so the command package does not depend
// on the library directly, and so platform branching and control semantics stay
// testable without a real service manager.
package svcmgr

import (
	"errors"
	"fmt"

	kservice "github.com/kardianos/service"
)

const (
	// Name is the service identifier registered with the OS service manager.
	Name = "ros-ddns"
	// DisplayName is the human readable service name.
	DisplayName = "ros-ddns"
	// Description documents the service in the OS service manager.
	Description = "RouterOS dynamic DNS daemon: maintains Aliyun DNS records and RouterOS address lists."
)

// Action values accepted by Control.
const (
	ActionInstall   = "install"
	ActionUninstall = "uninstall"
	ActionStart     = "start"
	ActionStop      = "stop"
	ActionRestart   = "restart"
	ActionStatus    = "status"
)

// Type aliases keep the kardianos/service types out of the command package.
type (
	Service = kservice.Service
	// Interface is implemented by the daemon adapter passed to New.
	Interface = kservice.Interface
	Status    = kservice.Status
	Logger    = kservice.Logger
)

// Status values used by StatusText.
const (
	StatusUnknown = kservice.StatusUnknown
	StatusRunning = kservice.StatusRunning
	StatusStopped = kservice.StatusStopped
)

// ErrNotInstalled reports that the service is not registered.
var ErrNotInstalled = kservice.ErrNotInstalled

// Options describes the service registration. Run control actions do not need
// any field; Install records the absolute paths in the service definition.
type Options struct {
	ConfigPath       string
	LockPath         string
	User             string
	WorkingDirectory string
}

// Platform returns the detected service manager, for example "linux-systemd"
// or "windows-service".
func Platform() string { return kservice.Platform() }

// Interactive reports whether the process was started from a console rather
// than by the OS service manager.
func Interactive() bool { return kservice.Interactive() }

// Arguments returns the command-line arguments recorded for the service. It
// contains no credentials: secrets stay in the configuration file.
func Arguments(opts Options) []string {
	if opts.ConfigPath == "" {
		return nil
	}
	args := []string{"-config", opts.ConfigPath}
	if opts.LockPath != "" {
		args = append(args, "-lock-file", opts.LockPath)
	}
	return args
}

// BuildConfig builds the service definition. platform selects the OS-specific
// extras so the result can be asserted without changing the detected system.
func BuildConfig(opts Options, platform string) *kservice.Config {
	cfg := &kservice.Config{
		Name:             Name,
		DisplayName:      DisplayName,
		Description:      Description,
		Arguments:        Arguments(opts),
		WorkingDirectory: opts.WorkingDirectory,
		UserName:         opts.User,
	}
	switch platform {
	case "linux-systemd":
		cfg.Dependencies = []string{"Wants=network-online.target", "After=network-online.target"}
		cfg.Option = kservice.KeyValue{"Restart": "on-failure"}
	case "windows-service":
		cfg.Option = kservice.KeyValue{"DelayedAutoStart": true}
	}
	return cfg
}

// New creates the service handle used to run the daemon under the service
// manager. program receives Start and Stop callbacks.
func New(program kservice.Interface, opts Options) (kservice.Service, error) {
	return kservice.New(program, BuildConfig(opts, kservice.Platform()))
}

// idleProgram is only used by control actions, which never call Run.
type idleProgram struct{}

func (idleProgram) Start(kservice.Service) error {
	return errors.New("service control program must not run")
}

func (idleProgram) Stop(kservice.Service) error { return nil }

// NewControl creates a service handle for install/uninstall/start/stop/restart/
// status actions. The program is unused.
func NewControl(opts Options) (kservice.Service, error) {
	return New(idleProgram{}, opts)
}

// StatusText renders the current status as running, stopped or not-installed.
func StatusText(s kservice.Service) (string, error) {
	status, err := s.Status()
	switch {
	case errors.Is(err, kservice.ErrNotInstalled):
		return "not-installed", nil
	case err != nil:
		return "", err
	case status == kservice.StatusRunning:
		return "running", nil
	case status == kservice.StatusStopped:
		return "stopped", nil
	default:
		return "unknown", nil
	}
}

// Control performs one service management action. Start and stop are
// idempotent; restart starts a stopped service; uninstall stops a running
// service before removing it.
func Control(s kservice.Service, action string) error {
	switch action {
	case ActionInstall:
		return s.Install()
	case ActionUninstall:
		if status, err := s.Status(); err == nil && status == kservice.StatusRunning {
			_ = s.Stop()
		}
		return s.Uninstall()
	case ActionStart:
		status, err := statusFor(s)
		if err != nil {
			return err
		}
		if status == kservice.StatusRunning {
			return nil
		}
		return s.Start()
	case ActionStop:
		status, err := statusFor(s)
		if err != nil {
			return err
		}
		if status == kservice.StatusStopped {
			return nil
		}
		return s.Stop()
	case ActionRestart:
		status, err := statusFor(s)
		if err != nil {
			return err
		}
		if status == kservice.StatusRunning {
			return s.Restart()
		}
		return s.Start()
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}

func statusFor(s kservice.Service) (kservice.Status, error) {
	status, err := s.Status()
	if errors.Is(err, kservice.ErrNotInstalled) {
		return kservice.StatusUnknown, fmt.Errorf("service %s is not installed: %w", Name, err)
	}
	return status, err
}
