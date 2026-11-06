package main

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
)

// runWhileMeshParticipating gives all carriers one participation lifetime.
// Sharing/roaming-only changes do not restart links. Missing, invalid or
// disabled policy cancels the runner and waits for its owned state to drain.
func runWhileMeshParticipating(ctx context.Context, path string, interval time.Duration, run func(context.Context) error) (bool, error) {
	config, err := meshsharing.LoadConfig(path)
	if err != nil || !config.Participate {
		return false, err
	}
	active, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-ticker.C:
				current, err := meshsharing.LoadConfig(path)
				if err != nil || !current.Participate {
					stopped <- err
					cancel()
					return
				}
			}
		}
	}()
	err = run(active)
	cancel()
	<-done
	select {
	case reason := <-stopped:
		if err == context.Canceled {
			err = nil
		}
		return false, errors.Join(err, reason)
	default:
		return true, err
	}
}

// runMeshSupervisor stays alive before initial configuration and between carrier
// lifetimes. Enrollment starts it once; policy changes never require re-enrollment
// or rebuilding the app proxy. A stopped/disabled runner is retried with backoff.
func runMeshSupervisor(ctx context.Context, interval time.Duration, run func(context.Context) error, report func(error)) {
	for ctx.Err() == nil {
		if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			report(err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
