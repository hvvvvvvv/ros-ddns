package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"ros-ddns/internal/config"
	"ros-ddns/internal/service"
)

type retirementService struct {
	managedService
	t      *testing.T
	active atomic.Bool
	closes atomic.Int32
	fail   bool
}

func (s *retirementService) Run(ctx context.Context) error {
	s.active.Store(true)
	defer s.active.Store(false)
	if s.fail {
		return errors.New("candidate startup failed")
	}
	return s.managedService.Run(ctx)
}

func (s *retirementService) CloseIdleConnections() {
	if s.active.Load() {
		s.t.Error("closed a pool before its workers joined")
	}
	s.closes.Add(1)
	closeIdleConnections(s.managedService)
}

func TestManagerClosesOnlyRetiredGenerations(t *testing.T) {
	f := newFixture(t, Options{})
	initial := &retirementService{managedService: f.m.svc, t: t}
	f.m.svc = initial
	services := []*retirementService{initial}
	factory := f.m.newService
	f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
		svc, err := factory(cfg, deps)
		if err != nil {
			return nil, err
		}
		tracked := &retirementService{managedService: svc, t: t}
		services = append(services, tracked)
		return tracked, nil
	}
	running := startManager(t, f.m)
	await(t, f.m.Ready)
	for range 3 {
		hash, data := editDocument(t, f.m, nil)
		if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err != nil {
			t.Fatal(err)
		}
		for i, svc := range services {
			want := int32(1)
			if i == len(services)-1 {
				want = 0
			}
			if svc.closes.Load() != want {
				t.Fatalf("generation %d closed %d times, want %d", i, svc.closes.Load(), want)
			}
		}
	}
	running.stop(t)
	for i, svc := range services {
		if svc.closes.Load() != 1 {
			t.Fatalf("shutdown left generation %d unclosed", i)
		}
	}
}

func TestRollbackDoesNotCloseReusableOldGeneration(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse old", true: "rebuild old"}[rebuild], func(t *testing.T) {
			f := newFixture(t, Options{})
			old := &retirementService{managedService: f.m.svc, t: t}
			f.m.svc = old
			running := startManager(t, f.m)
			await(t, f.m.Ready)
			factory := f.m.newService
			var candidates []*retirementService
			builds := 0
			f.m.newService = func(cfg *config.Config, deps service.Deps) (managedService, error) {
				builds++
				if builds == 3 && !rebuild {
					return nil, errors.New("rollback construction failed")
				}
				svc, err := factory(cfg, deps)
				if err != nil {
					return nil, err
				}
				tracked := &retirementService{managedService: svc, t: t, fail: builds == 2}
				candidates = append(candidates, tracked)
				return tracked, nil
			}
			hash, data := editDocument(t, f.m, renameItem)
			if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err == nil {
				t.Fatal("startup failure ignored")
			}
			await(t, f.m.Ready)
			want := int32(0)
			if rebuild {
				want = 1
			}
			if old.closes.Load() != want {
				t.Fatal("old pool closed before rollback reuse was resolved")
			}
			for _, candidate := range candidates[:2] {
				if candidate.closes.Load() != 1 {
					t.Fatal("discarded candidate pool not closed")
				}
			}
			if rebuild && candidates[2].closes.Load() != 0 {
				t.Fatal("restored active pool closed")
			}
			running.stop(t)
			if old.closes.Load() != 1 {
				t.Fatal("old pool not closed exactly once")
			}
			if rebuild && candidates[2].closes.Load() != 1 {
				t.Fatal("restored pool not closed on shutdown")
			}
		})
	}
}
