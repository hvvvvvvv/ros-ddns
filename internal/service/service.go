// Package service polls RouterOS and reconciles each item's independent targets.
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/redact"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/state"
)

type Deps struct {
	NewROS      func(config.RouterOSConfig) (ros.Client, error)
	NewProvider func(name string, cfg config.ProviderConfig) (dns.Provider, error)
	Selector    func([]ros.Address, ros.SelectOptions) (ros.Selection, error) // must be concurrency-safe
	Now         func() time.Time                                              // nil => time.Now; injected clocks must be concurrency-safe
	State       *state.Store
}

type Snapshot struct {
	ConfigPath string       `json:"configPath"`
	StartedAt  time.Time    `json:"startedAt"`
	Items      []ItemStatus `json:"items"`
}

type ItemStatus struct {
	Name             string             `json:"name"`
	Disabled         bool               `json:"disabled"`
	Family           string             `json:"family"`
	Interface        string             `json:"interface"`
	Phase            string             `json:"phase"`
	SourceAddress    string             `json:"sourceAddress,omitempty"`
	EUI64MAC         string             `json:"eui64Mac,omitempty"`
	RecordValue      string             `json:"recordValue,omitempty"`
	Domains          []DomainStatus     `json:"domains"`
	AddressList      *AddressListStatus `json:"addressList,omitempty"`
	LastError        string             `json:"lastError,omitempty"`
	SourceObservedAt time.Time          `json:"sourceObservedAt,omitempty"`
	LastSyncAt       time.Time          `json:"lastSyncAt,omitempty"`
	NextPollAt       time.Time          `json:"nextPollAt,omitempty"`
}

type DomainStatus struct {
	Provider  string    `json:"provider"`
	FQDN      string    `json:"fqdn"`
	Type      string    `json:"type"`
	Value     string    `json:"value,omitempty"`
	RecordID  string    `json:"recordId,omitempty"`
	TTL       int       `json:"ttl"`
	Line      string    `json:"line"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

type AddressListStatus struct {
	List      string    `json:"list"`
	Entries   []string  `json:"entries"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

// Service owns one shared client and provider instance per referenced name.
// Snapshot, Ready and TriggerItem are safe to use concurrently with Run.
type Service struct {
	runtime     config.RuntimeConfig
	pollTimeout time.Duration
	configPath  string
	startedAt   time.Time
	now         func() time.Time
	selector    func([]ros.Address, ros.SelectOptions) (ros.Selection, error)
	client      ros.Client
	providers   map[string]dns.Provider
	store       *state.Store
	redactor    *redact.Redactor
	items       []*item // enabled items, in config order
	ordered     []statusSlot
	byName      map[string]*item
	poller      *routerPoller
	lifecycleMu sync.Mutex
	active      bool
}

type statusSlot struct {
	item     *item
	disabled ItemStatus
}

// New accepts a normalized, validated config (normally from config.Load).
// NewROS defaults to ros.NewRESTClient. NewProvider is required only when an
// enabled item has DNS bindings. A nil State opens cfg.State.Path.
func New(cfg *config.Config, deps Deps) (*Service, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	r := cfg.Runtime
	if r.PollInterval.Duration <= 0 || r.ReconcileTimeout.Duration <= 0 ||
		r.MaxBackoff.Duration < r.PollInterval.Duration || r.StableFor.Duration < 0 || r.VerifyInterval.Duration < 0 {
		return nil, errors.New("invalid runtime intervals; load and validate the config before creating the service")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Selector == nil {
		deps.Selector = ros.Select
	}
	if deps.NewROS == nil {
		deps.NewROS = ros.NewRESTClient
	}
	s := &Service{
		runtime: r, pollTimeout: cfg.RouterOS.Timeout.Duration,
		configPath: cfg.Path, startedAt: deps.Now(),
		now: deps.Now, selector: deps.Selector, store: deps.State,
		providers: make(map[string]dns.Provider), byName: make(map[string]*item),
		redactor: redact.FromConfig(cfg),
	}
	if s.pollTimeout <= 0 {
		s.pollTimeout = r.ReconcileTimeout.Duration
	}
	if s.store == nil {
		var err error
		s.store, err = state.Open(cfg.State.Path)
		if err != nil {
			return nil, fmt.Errorf("open service state: %s", s.errorText(err))
		}
	}
	seenNames := make(map[string]bool)
	for _, original := range cfg.Items {
		c := config.CloneItem(original)
		if c.Name == "" || seenNames[c.Name] {
			return nil, fmt.Errorf("empty or duplicate item name %q", c.Name)
		}
		seenNames[c.Name] = true
		if c.Disabled {
			s.ordered = append(s.ordered, statusSlot{disabled: initialStatus(c)})
			continue
		}
		if !ros.Family(c.Source.Family).Valid() {
			return nil, fmt.Errorf("item %s: invalid address family %q", c.Name, c.Source.Family)
		}
		for _, d := range c.Domains {
			if _, ok := s.providers[d.Provider]; ok {
				continue
			}
			pc, ok := cfg.DNS.Providers[d.Provider]
			if !ok {
				return nil, fmt.Errorf("item %s: unknown provider %q", c.Name, d.Provider)
			}
			if deps.NewProvider == nil {
				return nil, fmt.Errorf("provider %s: Deps.NewProvider is required", d.Provider)
			}
			provider, err := deps.NewProvider(d.Provider, pc)
			if err != nil {
				return nil, fmt.Errorf("create provider %s: %s", d.Provider, s.errorText(err))
			}
			if provider == nil {
				return nil, fmt.Errorf("create provider %s: factory returned nil", d.Provider)
			}
			s.providers[d.Provider] = provider
		}
		it := newItem(s, c)
		s.items = append(s.items, it)
		s.ordered = append(s.ordered, statusSlot{item: it})
		s.byName[c.Name] = it
	}
	if len(s.items) > 0 {
		client, err := deps.NewROS(cfg.RouterOS)
		if err != nil {
			return nil, fmt.Errorf("create RouterOS client: %s", s.errorText(err))
		}
		if client == nil {
			return nil, errors.New("create RouterOS client: factory returned nil")
		}
		s.client = client
	}
	s.poller = newRouterPoller(s)
	return s, nil
}

// Run starts item workers and immediately polls the used address families. Poll
// and target failures are retried until cancellation; cancellation returns nil.
// Run and runOnce (the package-internal synchronous test driver) must not overlap
// on the same Service.
func (s *Service) Run(ctx context.Context) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.end()
	// A reused Service must observe sources again before a new Run can act on
	// queued work. Restore startup force after a prior canceled Run/runOnce.
	for _, it := range s.items {
		it.mu.Lock()
		it.sourceReady, it.sourceValid = false, false
		it.sourcePhase, it.sourceError = "waiting", ""
		it.candidate, it.valueSince = "", time.Time{}
		if it.cfg.AddressList != nil && it.listValue != "" {
			it.listStale = true
		}
		it.listRenewAt = time.Time{}
		if it.listStale {
			it.listRenewAt = time.Now()
		}
		it.listBackoff = 0
		if s.runtime.SyncOnStart {
			clear(it.domainValues)
		}
		it.pendingForce = it.pendingForce || s.runtime.SyncOnStart
		it.refreshLocked()
		it.mu.Unlock()
	}
	var workers sync.WaitGroup
	for _, it := range s.items {
		workers.Go(func() { it.run(ctx) })
		if it.cfg.AddressList != nil {
			workers.Go(func() { it.runListRenewal(ctx) })
		}
	}
	s.poller.run(ctx)
	workers.Wait()
	return nil
}

// Snapshot returns a deep copy in config order, including disabled items.
func (s *Service) Snapshot() Snapshot {
	out := Snapshot{ConfigPath: s.configPath, StartedAt: s.startedAt, Items: make([]ItemStatus, 0, len(s.ordered))}
	for _, slot := range s.ordered {
		if slot.item != nil {
			out.Items = append(out.Items, slot.item.snapshot())
		} else {
			out.Items = append(out.Items, cloneStatus(slot.disabled))
		}
	}
	return out
}

// TriggerItem queues a force reconciliation and resets polling backoff. Unknown
// and disabled items return false. Requests coalesce while work is in flight.
func (s *Service) TriggerItem(name string) bool {
	it, ok := s.byName[name]
	if !ok {
		return false
	}
	it.enqueue(true)
	s.poller.reset()
	return true
}

// Ready requires a successful fresh poll for every enabled item's family. A
// successful poll with no eligible address counts; disk state never counts.
func (s *Service) Ready() bool {
	for _, it := range s.items {
		it.mu.RLock()
		ready := it.sourceReady
		it.mu.RUnlock()
		if !ready {
			return false
		}
	}
	return true
}

func (s *Service) begin() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.active {
		return errors.New("service is already running")
	}
	s.active = true
	return nil
}

func (s *Service) end() {
	s.lifecycleMu.Lock()
	s.active = false
	s.lifecycleMu.Unlock()
}

// CloseIdleConnections releases provider connection pools after this generation
// has stopped and is permanently retired. Run/runOnce do not call it themselves:
// a canceled service can still be reused by a reload rollback. Providers remain
// usable after closing idle connections; the optional method does not change the
// dns.Provider contract.
func (s *Service) CloseIdleConnections() {
	for _, provider := range s.providers {
		if closer, ok := provider.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

func (s *Service) errorText(err error) string {
	if err == nil {
		return ""
	}
	return s.redactor.Replace(err.Error())
}
