package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/ros"
)

// newNoPollingService builds a Service whose config has no runnable enabled item
// and counts every RouterOS client / provider factory call.
func newNoPollingService(t *testing.T, c *config.Config, tolerant bool) (*Service, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	s, err := New(c, Deps{
		Tolerant: tolerant,
		NewROS: func(config.RouterOSConfig) (ros.Client, error) {
			calls.Add(1)
			return newFakeROS(), nil
		},
		NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) {
			calls.Add(1)
			return &fakeProvider{}, nil
		},
		Selector: fakeSelector,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, &calls
}

func runUntilCanceled(t *testing.T, s *Service) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case err := <-done:
		cancel()
		return err
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
		return nil
	}
}

func TestEmptyItemsDoNotCreateClientOrPoll(t *testing.T) {
	c := testConfig()
	c.Items = nil
	s, calls := newNoPollingService(t, c, false)
	if s.polling {
		t.Fatal("polling reported for empty items")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("factories called %d times for empty items", got)
	}
	if len(s.poller.families) != 0 {
		t.Fatalf("poller families = %d, want 0", len(s.poller.families))
	}
	if !s.Ready() {
		t.Fatal("empty items should be ready without polling")
	}
	if err := runUntilCanceled(t, s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("Run created %d clients without enabled items", got)
	}
	if _, err := s.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce without items: %v", err)
	}
}

func TestAllItemsDisabledDoNotCreateClientOrPoll(t *testing.T) {
	c := testConfig()
	c.Items = []config.ItemConfig{{
		Name:     "home",
		Disabled: true,
		// A disabled item may be incomplete; it still must not create a client.
	}}
	s, calls := newNoPollingService(t, c, false)
	if s.polling || calls.Load() != 0 {
		t.Fatalf("polling=%t factory calls=%d", s.polling, calls.Load())
	}
	if len(s.items) != 0 || len(s.poller.families) != 0 {
		t.Fatalf("items=%d families=%d", len(s.items), len(s.poller.families))
	}
	status := s.Snapshot().Items[0]
	if status.Name != "home" || !status.Disabled || status.Phase != "disabled" {
		t.Fatalf("disabled status = %+v", status)
	}
	if !s.Ready() {
		t.Fatal("all-disabled items should be ready without polling")
	}
	if err := runUntilCanceled(t, s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("Run created clients without runnable items")
	}
}

func TestTolerantConfigErrorsDoNotStartPolling(t *testing.T) {
	c := testConfig()
	c.Items = []config.ItemConfig{{
		Name:   "broken",
		Source: config.SourceConfig{Family: "ipv4"},
		// Missing interface and unknown provider leave the item unusable.
		Domains: []config.DomainConfig{{Provider: "missing", Domain: "example.com", RR: "home"}},
	}}
	s, calls := newNoPollingService(t, c, true)
	if s.polling || calls.Load() != 0 {
		t.Fatalf("polling=%t factory calls=%d", s.polling, calls.Load())
	}
	if len(s.items) != 0 || len(s.poller.families) != 0 {
		t.Fatalf("items=%d families=%d", len(s.items), len(s.poller.families))
	}
	status := s.Snapshot().Items[0]
	if status.Phase != "error" || status.LastError == "" {
		t.Fatalf("error status = %+v", status)
	}
	if s.Ready() {
		t.Fatal("startup config errors must keep ready=false")
	}
	if err := runUntilCanceled(t, s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("Run created clients without runnable items")
	}
}

func TestRunPollsWhenAnyItemRunnable(t *testing.T) {
	c := testConfig()
	router := newFakeROS()
	s, err := New(c, Deps{
		NewROS:      func(config.RouterOSConfig) (ros.Client, error) { return router, nil },
		NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) { return &fakeProvider{}, nil },
		Selector:    fakeSelector,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.polling {
		t.Fatal("polling not reported for a runnable item")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		router.mu.Lock()
		calls := router.pollCalls[ros.IPv4]
		router.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run did not poll with a runnable item")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancellation: %v", err)
	}
}
