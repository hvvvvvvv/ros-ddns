package service

import (
	"context"
	"testing"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/ros"
)

type closingProvider struct {
	fakeProvider
	closes int
}

func (p *closingProvider) CloseIdleConnections() { p.closes++ }

func TestServiceExplicitPoolCleanupPreservesReuse(t *testing.T) {
	provider := &closingProvider{}
	svc, err := New(testConfig(), Deps{
		NewROS:      func(config.RouterOSConfig) (ros.Client, error) { return newFakeROS(), nil },
		NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) { return provider, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if snapshot, err := svc.runOnce(context.Background()); err != nil || snapshot.Items[0].Phase != "synced" {
			t.Fatalf("runOnce: %+v, %v", snapshot, err)
		}
	}
	if provider.closes != 0 {
		t.Fatal("ordinary lifecycle closed a reusable provider")
	}
	svc.CloseIdleConnections()
	if provider.closes != 1 {
		t.Fatalf("shared provider closed %d times, want once per name", provider.closes)
	}
	if snapshot, err := svc.runOnce(context.Background()); err != nil || snapshot.Items[0].Phase != "synced" {
		t.Fatal("idle close disabled service reuse")
	}
}
