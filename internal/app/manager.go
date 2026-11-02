// Package app owns the daemon service and transactional configuration reloads.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/notify"
	"ros-ddns/internal/pathguard"
	"ros-ddns/internal/redact"
	"ros-ddns/internal/service"
	"ros-ddns/internal/state"
)

// ErrConfigConflict means an edit was based on a missing or stale file hash.
var ErrConfigConflict = errors.New("configuration changed; read it again before applying")

// ErrRestartRequired means an edit changes a setting that cannot be reloaded.
var ErrRestartRequired = errors.New("configuration change requires a daemon restart")

type Options struct {
	LogHub        *logging.Hub
	LogLevel      *slog.LevelVar
	Notifications *notify.Store
	Version       string
	LockPath      string // Effective startup lock; it does not move on reload.
}

type Meta struct {
	Version    string        `json:"version"`
	ConfigPath string        `json:"configPath"`
	StartedAt  time.Time     `json:"startedAt"`
	Uptime     time.Duration `json:"uptime"`
	LogLevel   string        `json:"logLevel"`
}

type managedService interface {
	Run(context.Context) error
	Snapshot() service.Snapshot
	Ready() bool
	TriggerItem(string) bool
}

// Manager is safe for concurrent API callers. It serializes reloads and
// lifecycle changes separately from snapshot readers. Run is single-use;
// construct another Manager after it stops.
type Manager struct {
	applyMu  sync.Mutex
	mu       sync.RWMutex
	cfg      *config.Config
	deps     service.Deps
	opts     Options
	svc      managedService
	started  time.Time
	lockPath string

	reloading bool
	runCalled bool
	finished  bool
	runCtx    context.Context
	run       *serviceRun
	exited    chan struct{}

	// Private seams let tests exercise startup and filesystem failures without
	// changing the service package or depending on platform-specific ACLs.
	newService func(*config.Config, service.Deps) (managedService, error)
	writeFile  func(string, []byte, os.FileMode) error
}

// NewManager constructs the initial service without starting goroutines. cfg
// must be normalized and validated. A supplied State is retained until
// state.path changes, when the manager opens the newly configured cache.
func NewManager(cfg *config.Config, deps service.Deps, opts Options) (*Manager, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	cfg = cloneConfig(cfg)
	if err := pathguard.Validate(cfg.Path, cfg.State.Path, opts.LockPath); err != nil {
		return nil, err
	}
	var err error
	deps, err = withState(cfg, deps)
	if err != nil {
		return nil, err
	}
	svc, err := service.New(cfg, deps)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg: cfg, deps: deps, opts: opts, svc: svc, started: time.Now(), lockPath: opts.LockPath,
		exited: make(chan struct{}, 1), writeFile: replaceFile,
		newService: func(cfg *config.Config, deps service.Deps) (managedService, error) {
			return service.New(cfg, deps)
		},
	}
	m.updateLogging(cfg)
	return m, nil
}

// Run starts one service and one notification monitor, and joins both on
// cancellation. Cancellation returns nil. A concurrent or subsequent Run returns
// a clear error; the manager cannot be reused after shutdown.
func (m *Manager) Run(ctx context.Context) error {
	m.applyMu.Lock()
	m.mu.Lock()
	if m.runCalled {
		m.mu.Unlock()
		m.applyMu.Unlock()
		return errors.New("manager Run has already been called; create a new manager")
	}
	m.runCalled = true
	runCtx, cancel := context.WithCancel(ctx)
	m.runCtx = runCtx
	svc := m.svc
	m.mu.Unlock()
	run, err := m.startService(runCtx, svc)
	if err != nil {
		cancel()
		closeIdleConnections(svc)
		m.mu.Lock()
		m.finished = true
		m.mu.Unlock()
		m.applyMu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		return m.safeError(err)
	}
	m.mu.Lock()
	m.run = run
	m.mu.Unlock()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		m.monitor(runCtx)
	}()
	m.applyMu.Unlock()

wait:
	for {
		select {
		case <-runCtx.Done():
			break wait
		case <-m.exited:
			// A canceled old generation also signals exited. Wait for any
			// transaction to finish, then inspect only the current generation.
			m.applyMu.Lock()
			m.mu.RLock()
			current := m.run
			m.mu.RUnlock()
			if result, stopped := current.result(); stopped {
				err = result
				if err == nil {
					err = errors.New("service stopped unexpectedly")
				}
			}
			m.applyMu.Unlock()
			if err != nil {
				break wait
			}
		}
	}
	cancel() // Also interrupts an apply currently waiting for startup.
	m.applyMu.Lock()
	m.mu.RLock()
	run = m.run
	m.mu.RUnlock()
	run.stop()
	closeIdleConnections(m.svc)
	<-monitorDone
	m.mu.Lock()
	m.finished = true
	m.mu.Unlock()
	m.applyMu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		slog.Error("daemon service stopped unexpectedly")
		m.addEvent(notify.Event{Kind: "service_error", Severity: "error", Message: "后台服务意外停止。"})
	}
	return m.safeError(err)
}

func (m *Manager) Snapshot() service.Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.svc.Snapshot()
}

func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.reloading && !m.finished && (!m.runCalled || m.runCtx.Err() == nil) && m.svc.Ready()
}

func (m *Manager) TriggerItem(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.reloading || m.finished || (m.runCalled && m.runCtx.Err() != nil) {
		return false
	}
	return m.svc.TriggerItem(name)
}

func (m *Manager) ConfigDocument() (config.Document, error) {
	m.mu.RLock()
	path, token := m.cfg.Path, m.cfg.API.Token
	m.mu.RUnlock()
	document, err := config.ReadDocument(path)
	if err != nil {
		return document, err
	}
	document.APITokenValue = token
	return document, nil
}

// ValidateConfig checks the full structured configuration without constructing
// a service or writing files. Restart-only comparisons belong to ApplyConfig.
func (m *Manager) ValidateConfig(data []byte) error {
	m.mu.RLock()
	path := m.cfg.Path
	m.mu.RUnlock()
	cfg, _, err := config.ValidateStructured(data, path)
	if err != nil {
		return err
	}
	return pathguard.Validate(path, cfg.State.Path, m.lockPath)
}

func (m *Manager) Meta() Meta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	level := m.cfg.Log.Level
	if m.opts.LogLevel != nil {
		level = strings.ToLower(m.opts.LogLevel.Level().String())
	}
	return Meta{
		Version: m.opts.Version, ConfigPath: m.cfg.Path, StartedAt: m.started,
		Uptime: time.Since(m.started), LogLevel: level,
	}
}

func (m *Manager) updateLogging(cfg *config.Config) {
	if m.opts.LogLevel != nil {
		var level slog.Level
		// Config has already passed Parse/Validate's debug|info|warn|error check.
		if err := level.UnmarshalText([]byte(cfg.Log.Level)); err == nil {
			m.opts.LogLevel.Set(level)
		}
	}
	if m.opts.LogHub != nil {
		m.opts.LogHub.SetRedactor(redact.FromConfig(cfg))
	}
}

func (m *Manager) addEvent(event notify.Event) {
	if m.opts.Notifications != nil {
		m.opts.Notifications.Add(event)
	}
}

func (m *Manager) safeError(err error) error {
	if err == nil {
		return nil
	}
	m.mu.RLock()
	r := redact.FromConfig(m.cfg)
	m.mu.RUnlock()
	return fmt.Errorf("service lifecycle: %s", r.Replace(err.Error()))
}

// Reuse the synchronized cache across generations with the same state.path.
// Besides retaining the latest in-memory updates, this avoids racing a live
// writer's rename when opening the cache on Windows.
func withState(cfg *config.Config, deps service.Deps) (service.Deps, error) {
	if cfg != nil && deps.State == nil {
		store, err := state.Open(cfg.State.Path)
		if err != nil {
			return deps, fmt.Errorf("open service state: %s", redact.FromConfig(cfg).Replace(err.Error()))
		}
		deps.State = store
	}
	return deps, nil
}

func cloneConfig(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	copy := *cfg
	copy.DNS.Providers = make(map[string]config.ProviderConfig, len(cfg.DNS.Providers))
	for name, provider := range cfg.DNS.Providers {
		copy.DNS.Providers[name] = provider
	}
	copy.Items = append([]config.ItemConfig(nil), cfg.Items...)
	for i := range copy.Items {
		item := &copy.Items[i]
		item.Domains = append([]config.DomainConfig(nil), item.Domains...)
		item.Source.EUI64MACAddr = append(item.Source.EUI64MACAddr[:0:0], item.Source.EUI64MACAddr...)
		if item.Source.Select.PrefixNet != nil {
			prefix := *item.Source.Select.PrefixNet
			item.Source.Select.PrefixNet = &prefix
		}
		if item.AddressList != nil {
			list := *item.AddressList
			item.AddressList = &list
		}
	}
	return &copy
}
