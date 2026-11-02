package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"ros-ddns/internal/svcmgr"
)

// serviceProgram adapts the daemon to the OS service manager callbacks.
// Start must return quickly; the daemon keeps running in the background and is
// stopped through Stop with a bounded graceful shutdown.
type serviceProgram struct {
	session daemonRunner
	logger  svcmgr.Logger
	exit    func(int)

	cancel   context.CancelFunc
	stopping atomic.Bool
	done     chan struct{}
}

// daemonRunner is the subset of daemonSession used by serviceProgram so tests
// can exercise the lifecycle without a real daemon.
type daemonRunner interface {
	start(context.Context) error
	wait() error
}

func (p *serviceProgram) Start(svcmgr.Service) error {
	if p.session == nil {
		return errors.New("daemon session is not configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	if err := p.session.start(ctx); err != nil {
		cancel()
		return err
	}
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		err := p.session.wait()
		if err == nil || p.stopping.Load() {
			return
		}
		if p.logger != nil {
			_ = p.logger.Error(err.Error())
		}
		slog.Error("daemon stopped unexpectedly", "error", err)
		if p.exit != nil {
			p.exit(1)
		}
	}()
	return nil
}

func (p *serviceProgram) Stop(svcmgr.Service) error {
	p.stopping.Store(true)
	if p.cancel == nil {
		return nil
	}
	p.cancel()
	if p.done == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for the daemon to stop")
	}
}

// serviceFatal reports a startup failure to stderr and, when the OS service
// manager provides one, to its event log, then returns the service failure code.
func serviceFatal(logger svcmgr.Logger, stderr io.Writer, format string, args ...any) int {
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(stderr, "ros-ddns: %s\n", message)
	if logger != nil {
		_ = logger.Error(message)
	}
	return 1
}

type serviceActionOptions struct {
	Action     string
	User       string
	ConfigPath string
	LockPath   string
	Check      bool
	Stdout     io.Writer
	Stderr     io.Writer
}

// newServiceControl is a seam so tests can exercise the CLI without touching
// the real service manager.
var newServiceControl = svcmgr.NewControl

func runServiceAction(o serviceActionOptions) int {
	switch o.Action {
	case svcmgr.ActionInstall, svcmgr.ActionUninstall, svcmgr.ActionStart,
		svcmgr.ActionStop, svcmgr.ActionRestart, svcmgr.ActionStatus:
	default:
		fmt.Fprintf(o.Stderr, "ros-ddns: unknown -service action %q (install|uninstall|start|stop|restart|status)\n", o.Action)
		return 2
	}
	if o.Check {
		fmt.Fprintln(o.Stderr, "ros-ddns: -service cannot be combined with -check")
		return 2
	}

	opts := svcmgr.Options{User: o.User}
	if o.Action == svcmgr.ActionInstall {
		absConfig, err := filepath.Abs(o.ConfigPath)
		if err != nil {
			fmt.Fprintln(o.Stderr, "ros-ddns: cannot resolve config path")
			return 2
		}
		opts.ConfigPath = absConfig
		opts.WorkingDirectory = filepath.Dir(absConfig)
		if o.LockPath != "" {
			absLock, err := filepath.Abs(o.LockPath)
			if err != nil {
				fmt.Fprintln(o.Stderr, "ros-ddns: cannot resolve lock path")
				return 2
			}
			opts.LockPath = absLock
		}
	}

	service, err := newServiceControl(opts)
	if err != nil {
		fmt.Fprintf(o.Stderr, "ros-ddns: initialize service manager: %v\n", err)
		return 1
	}

	if o.Action == svcmgr.ActionStatus {
		text, err := svcmgr.StatusText(service)
		if err != nil {
			fmt.Fprintf(o.Stderr, "ros-ddns: service status: %v\n", err)
			return 1
		}
		fmt.Fprintf(o.Stdout, "platform=%s name=%s status=%s\n", svcmgr.Platform(), svcmgr.Name, text)
		if text == "running" {
			return 0
		}
		return 1
	}

	if o.Action == svcmgr.ActionInstall {
		if _, err := os.Stat(opts.ConfigPath); err != nil {
			fmt.Fprintf(o.Stderr, "ros-ddns: warning: %s is not readable yet; the first service start will generate a default configuration\n", opts.ConfigPath)
		}
	}
	if err := svcmgr.Control(service, o.Action); err != nil {
		fmt.Fprintf(o.Stderr, "ros-ddns: %v\n", err)
		return 1
	}

	switch o.Action {
	case svcmgr.ActionInstall:
		fmt.Fprintf(o.Stdout, "installed service %s\n", svcmgr.Name)
		fmt.Fprintf(o.Stdout, "  platform: %s\n", svcmgr.Platform())
		fmt.Fprintf(o.Stdout, "  config: %s\n", opts.ConfigPath)
		if opts.LockPath != "" {
			fmt.Fprintf(o.Stdout, "  lock: %s\n", opts.LockPath)
		}
		if opts.User != "" {
			fmt.Fprintf(o.Stdout, "  user: %s\n", opts.User)
		}
		fmt.Fprintln(o.Stdout, "  next: ros-ddns -service start")
	case svcmgr.ActionUninstall:
		fmt.Fprintf(o.Stdout, "uninstalled service %s\n", svcmgr.Name)
	default:
		fmt.Fprintf(o.Stdout, "%s: %s\n", o.Action, svcmgr.Name)
	}
	return 0
}
