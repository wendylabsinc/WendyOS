package main

import (
	"context"
	"time"
)

// repairMeshUDPPolicy returns a join handle. After cancellation the owner must
// join it before retiring the policy, including a repair already in progress.
func repairMeshUDPPolicy(ctx context.Context, interval time.Duration, repair func() error, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if ctx.Err() != nil {
				return
			}
			if err := repair(); err != nil {
				onError(err)
			}
		}
	}()
	return done
}
