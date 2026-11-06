package main

import (
	"context"
	"sync"
	"time"
)

// meshLifetime owns every worker holding the enrolled mesh identity. Stop
// prevents a concurrent enrollment callback from restarting that identity and
// waits for sessions, carriers and their kernel policies to drain.
type meshLifetime struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	started bool
	stopped bool
	wg      sync.WaitGroup
	done    chan struct{}
}

func (m *meshLifetime) Start(parent context.Context, start func(context.Context, func(func()))) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.stopped || parent.Err() != nil {
		return
	}
	m.started = true
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	// start must register workers synchronously. Holding mu makes registration
	// atomic with Stop's Wait, including a reset racing first enrollment.
	start(ctx, func(run func()) {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			run()
		}()
	})
}

func (m *meshLifetime) Stop(ctx context.Context) error {
	m.mu.Lock()
	m.stopped = true
	if m.cancel != nil {
		m.cancel()
	}
	if m.done == nil {
		m.done = make(chan struct{})
		go func() { m.wg.Wait(); close(m.done) }()
	}
	done := m.done
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recoverCompletedResetMesh runs before listeners expose a completed reset.
// Cleanup failures hide completion and enrollment readiness, while retries
// leave diagnostics and agent updates available. Completion must come from
// ProvisioningService's authenticated recovery, never a raw state-file read.
// Fresh unenrolled hosts have no completion and require no mesh dependencies.
func recoverCompletedResetMesh(ctx context.Context, completed bool, retry time.Duration, cleanup func(context.Context) error, setError func(error), report func(error)) <-chan struct{} {
	done := make(chan struct{})
	if !completed {
		close(done)
		return done
	}
	attempt := func() error {
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := cleanup(cleanupCtx)
		setError(err)
		if err != nil {
			report(err)
		}
		return err
	}
	if attempt() == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(retry)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if attempt() == nil {
					return
				}
			}
		}
	}()
	return done
}
