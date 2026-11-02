package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/ros"
)

func dualFamilyConfig() *config.Config {
	c := testConfig()
	c.Items[0].Domains, c.Items[0].AddressList = nil, nil
	v6 := config.CloneItem(c.Items[0])
	v6.Name, v6.Source.Family = "v6", "ipv6"
	c.Items = append(c.Items, v6)
	return c
}

func TestSlowIPv4DoesNotDelayIPv6Observation(t *testing.T) {
	for _, mode := range []string{"Run", "runOnce"} {
		t.Run(mode, func(t *testing.T) {
			router := newFakeROS()
			enteredV4, returnedV6, releaseV4 := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{})
			router.addressFunc = func(ctx context.Context, family ros.Family) ([]ros.Address, error) {
				if family == ros.IPv6 {
					returnedV6 <- struct{}{}
					return []ros.Address{address("2001:db8::1/64")}, nil
				}
				enteredV4 <- struct{}{}
				select {
				case <-releaseV4:
					return []ros.Address{address("203.0.113.10/24")}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			s, _ := testService(t, dualFamilyConfig(), router, &fakeProvider{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if mode == "runOnce" {
					_, err := s.runOnce(ctx)
					done <- err
				} else {
					done <- s.Run(ctx)
				}
			}()
			defer stopService(t, cancel, done)
			for _, ch := range []<-chan struct{}{enteredV4, returnedV6} {
				select {
				case <-ch:
				case <-time.After(3 * time.Second):
					t.Fatal("families were not polled independently")
				}
			}
			awaitCondition(t, func() bool { return s.Snapshot().Items[1].SourceAddress == "2001:db8::1/64" })
			if s.Snapshot().Items[0].SourceAddress != "" {
				t.Fatal("IPv4 was observed before release")
			}
			close(releaseV4)
			awaitCondition(t, s.Ready)
		})
	}
}

func TestManualTriggerWakesBothFamilyLoops(t *testing.T) {
	router := newFakeROS()
	router.setAddresses(ros.IPv6, address("2001:db8::1/64"))
	router.pollErrors[ros.IPv4] = errors.New("v4 offline")
	router.pollErrors[ros.IPv6] = errors.New("v6 offline")
	s, _ := testService(t, dualFamilyConfig(), router, &fakeProvider{})
	cancel, done := startService(t, s)
	defer stopService(t, cancel, done)
	awaitCondition(t, func() bool {
		snapshot := s.Snapshot()
		return snapshot.Items[0].Phase == "degraded" && snapshot.Items[1].Phase == "degraded"
	})
	router.mu.Lock()
	clear(router.pollErrors)
	router.mu.Unlock()
	if !s.TriggerItem("home") {
		t.Fatal("manual trigger rejected")
	}
	awaitCondition(t, s.Ready)
	router.mu.Lock()
	defer router.mu.Unlock()
	if router.pollCalls[ros.IPv4] != 2 || router.pollCalls[ros.IPv6] != 2 {
		t.Fatalf("trigger did not wake both readers: %v", router.pollCalls)
	}
}

func TestCancellationWaitsForBothFamilyReaders(t *testing.T) {
	router := newFakeROS()
	entered, exited := make(chan ros.Family, 2), make(chan ros.Family, 2)
	router.addressFunc = func(ctx context.Context, family ros.Family) ([]ros.Address, error) {
		entered <- family
		<-ctx.Done()
		exited <- family
		return nil, ctx.Err()
	}
	s, _ := testService(t, dualFamilyConfig(), router, &fakeProvider{})
	cancel, done := startService(t, s)
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("both family readers did not start")
		}
	}
	for range 100 {
		s.TriggerItem("home")
	}
	stopService(t, cancel, done)
	if len(exited) != 2 || router.pollCalls[ros.IPv4] != 1 || router.pollCalls[ros.IPv6] != 1 {
		t.Fatalf("cancellation did not join exactly one reader per family: exited=%d calls=%v", len(exited), router.pollCalls)
	}
}

func TestPollAllJoinsFamilyErrors(t *testing.T) {
	router := newFakeROS()
	router.pollErrors[ros.IPv4] = errors.New("v4 failure")
	router.pollErrors[ros.IPv6] = errors.New("v6 failure")
	s, _ := testService(t, dualFamilyConfig(), router, &fakeProvider{})
	_, err := s.runOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "v4 failure") || !strings.Contains(err.Error(), "v6 failure") {
		t.Fatalf("pollAll did not join errors: %v", err)
	}
}
