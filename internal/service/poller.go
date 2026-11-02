package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ros-ddns/internal/ros"
)

// routerPoller runs one reader per family. Each family's mutable state belongs
// exclusively to that reader. Run and runOnce are serialized by the lifecycle
// guard; reset only sends to the immutable per-family wake channels.
type routerPoller struct {
	service  *Service
	families []*familyPoll
}

type familyPoll struct {
	family    ros.Family
	items     []*item
	addresses []ros.Address // retained on errors; never treated as a fresh poll
	backoff   time.Duration
	nextDue   time.Time // wall/monotonic clock for scheduling, independent of Deps.Now
	wake      chan struct{}
}

func newRouterPoller(s *Service) *routerPoller {
	p := &routerPoller{service: s}
	for _, family := range []ros.Family{ros.IPv4, ros.IPv6} {
		f := &familyPoll{family: family, wake: make(chan struct{}, 1)}
		for _, it := range s.items {
			if ros.Family(it.cfg.Source.Family) == family {
				f.items = append(f.items, it)
			}
		}
		if len(f.items) > 0 {
			p.families = append(p.families, f)
		}
	}
	return p
}

func (p *routerPoller) reset() {
	for _, f := range p.families {
		select {
		case f.wake <- struct{}{}:
		default:
		}
	}
}

func (p *routerPoller) pollAll(ctx context.Context) error {
	failures := make([]error, len(p.families))
	var readers sync.WaitGroup
	for i, family := range p.families {
		readers.Go(func() { failures[i] = p.pollFamily(ctx, family) })
	}
	readers.Wait()
	return errors.Join(failures...)
}

func (p *routerPoller) pollFamily(ctx context.Context, f *familyPoll) error {
	pollCtx, cancel := context.WithTimeout(ctx, p.service.pollTimeout)
	var addresses []ros.Address
	err := pollCtx.Err()
	if err == nil {
		addresses, err = p.service.client.Addresses(pollCtx, f.family)
	}
	cancel()
	delay := p.service.runtime.PollInterval.Duration
	if err != nil {
		err = fmt.Errorf("poll %s addresses: %s", f.family, p.service.errorText(err))
		f.backoff = growBackoff(f.backoff, delay, p.service.runtime.MaxBackoff.Duration)
		delay = f.backoff
		slog.Warn("RouterOS address poll failed", "family", f.family, "retryAfter", delay, "error", err)
	} else {
		f.addresses = append([]ros.Address(nil), addresses...)
		f.backoff = 0
	}
	observed := p.service.now()
	f.nextDue = time.Now().Add(delay)
	for _, it := range f.items {
		it.observe(f.addresses, err, observed, observed.Add(delay))
	}
	return err
}

func (p *routerPoller) run(ctx context.Context) {
	var readers sync.WaitGroup
	for _, family := range p.families {
		readers.Go(func() { p.runFamily(ctx, family) })
	}
	<-ctx.Done()
	readers.Wait()
}

func (p *routerPoller) runFamily(ctx context.Context, family *familyPoll) {
	// Every Run starts with a fresh poll, even after a previous runOnce.
	family.nextDue, family.backoff = time.Time{}, 0
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-family.wake:
			family.backoff = 0
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		p.pollFamily(ctx, family)
		timer.Reset(max(0, time.Until(family.nextDue)))
	}
}
