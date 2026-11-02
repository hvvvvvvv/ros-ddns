package app

import (
	"context"
	"errors"
	"sync"
)

// Called only for permanently discarded generations, after their workers join.
// Run cancellation alone cannot do this: rollback may reuse the old service.
func closeIdleConnections(svc managedService) {
	if closer, ok := svc.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type serviceRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error // published by closing done
}

func (r *serviceRun) stop() {
	if r != nil {
		r.cancel()
		<-r.done
	}
}

func (r *serviceRun) result() (error, bool) {
	if r == nil {
		return nil, false
	}
	select {
	case <-r.done:
		return r.err, true
	default:
		return nil, false
	}
}

// startupContext observes the existing service's lifecycle boundary without
// polling readiness or requiring RouterOS to be online. Service.Run checks its
// lifecycle guard and initializes workers before using ctx.Done; its only
// startup error returns before that point. Subsequent poll/target failures are
// retried by Service and must not roll back an otherwise valid configuration.
type startupContext struct {
	context.Context
	once    sync.Once
	started chan struct{}
}

func (ctx *startupContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.started) })
	return ctx.Context.Done()
}

func (m *Manager) startService(parent context.Context, svc managedService) (*serviceRun, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	startup := &startupContext{Context: ctx, started: make(chan struct{})}
	run := &serviceRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		run.err = svc.Run(startup)
		close(run.done)
		select {
		case m.exited <- struct{}{}:
		default:
		}
	}()
	select {
	case <-startup.started:
		if parent.Err() == nil {
			if _, stopped := run.result(); !stopped {
				return run, nil
			}
		}
	case <-run.done:
	case <-parent.Done():
	}
	run.stop()
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if run.err != nil {
		return nil, run.err
	}
	return nil, errors.New("service stopped during startup")
}
