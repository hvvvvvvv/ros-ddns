package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"ros-ddns/internal/api"
	"ros-ddns/internal/app"
	"ros-ddns/internal/config"
	"ros-ddns/internal/service"
	webui "ros-ddns/web"
)

// daemonSession owns one daemon generation: the manager, the HTTP listener and
// the goroutines that serve until their context is canceled. It is shared by the
// interactive daemon and by the OS service manager adapter.
type daemonSession struct {
	cfg     *config.Config
	manager *app.Manager
	opts    app.Options

	mu      sync.Mutex
	apiDone chan error
	runDone chan error
}

func newDaemonSession(cfg *config.Config, deps service.Deps, opts app.Options) (*daemonSession, error) {
	manager, err := app.NewManager(cfg, deps, opts)
	if err != nil {
		return nil, err
	}
	return &daemonSession{cfg: cfg, manager: manager, opts: opts}, nil
}

// start binds the HTTP listener synchronously (so startup errors are reported
// before the service manager marks the daemon running) and then serves the API
// and the manager workers until the context is canceled.
func (s *daemonSession) start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	listener, err := api.Listen(ctx, s.cfg.API)
	if err != nil {
		cancel()
		return err
	}
	slog.InfoContext(ctx, "HTTP API listening", "address", listener.Addr().String(), "authRequired", s.cfg.API.Token != "")

	s.mu.Lock()
	s.apiDone = make(chan error, 1)
	s.runDone = make(chan error, 1)
	apiDone, runDone := s.apiDone, s.runDone
	s.mu.Unlock()

	meta := s.manager.Meta()
	go func() {
		apiDone <- api.Serve(ctx, s.cfg.API, api.Options{
			Source:        s.manager,
			Config:        s.manager,
			Logs:          s.opts.LogHub,
			Notifications: s.opts.Notifications,
			Meta:          s.manager,
			Token:         s.cfg.API.Token,
			Version:       s.opts.Version,
			StreamEpoch:   meta.StartedAt.Format(time.RFC3339Nano),
			UIFS:          webui.FS,
		}, listener)
		cancel()
	}()
	go func() {
		runDone <- s.manager.Run(ctx)
		cancel()
	}()
	return nil
}

// wait joins the API server and the manager. Both stop after the context is
// canceled; errors.Join preserves errors from both.
func (s *daemonSession) wait() error {
	s.mu.Lock()
	apiDone, runDone := s.apiDone, s.runDone
	s.mu.Unlock()
	if apiDone == nil || runDone == nil {
		return errors.New("daemon session has not been started")
	}
	runErr := <-runDone
	apiErr := <-apiDone
	return errors.Join(runErr, apiErr)
}
