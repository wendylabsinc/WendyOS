package litetopology

import (
	"context"
	"sync"
	"time"
)

const (
	// TCP admission stays short; verified certificate/WendyCom work can cross BLE.
	SetupBudget     = 75 * time.Second
	HandshakeBudget = 60 * time.Second
	AggregateBudget = 60 * time.Second
	PageBudget      = 3 * time.Second
)

// BeginSetup bounds establishment without expiring a successful connection's
// lifetime. Finish stops only the setup timer; cancel still owns the stream.
func BeginSetup(parent context.Context, budget time.Duration) (context.Context, func() error, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(parent)
	var mu sync.Mutex
	finished := false
	timer := time.AfterFunc(budget, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			cancelCause(context.DeadlineExceeded)
		}
	})
	finish := func() error { mu.Lock(); defer mu.Unlock(); finished = true; timer.Stop(); return context.Cause(ctx) }
	cancel := func() { _ = finish(); cancelCause(context.Canceled) }
	return ctx, finish, cancel
}
