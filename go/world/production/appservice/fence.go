package appservice

import (
	"context"
	"sync"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
)

// Fence ties one acceptance attempt to one current listener generation. A
// retained fence cannot admit another publication or survive its RPC. Commit
// is the only supported place for the sink's atomic persistence operation.
type Fence struct {
	service   *service
	ctx       context.Context
	mu        sync.Mutex
	entered   bool
	committed bool
	finished  bool
}

func (f *Fence) Commit(ctx context.Context, commit func(context.Context) error) error {
	if ctx == nil || commit == nil {
		return ErrAdmission
	}
	f.mu.Lock()
	if f.entered || f.finished {
		f.mu.Unlock()
		return ErrAdmission
	}
	f.entered = true
	f.mu.Unlock()
	s := f.service
	waiting := rpctrace.Start(f.ctx, rpctrace.StageAdmissionWait)
	s.admissionMu.Lock()
	waiting()
	defer s.admissionMu.Unlock()
	f.mu.Lock()
	finished := f.finished
	f.mu.Unlock()
	if finished {
		return ErrAdmission
	}
	if ctx.Err() != nil || f.ctx.Err() != nil || s.closed.Load() {
		return ErrClosed
	}
	if err := s.authorize(f.ctx); err != nil {
		return err
	}
	// Release cancels f.ctx before waiting on this lock. The callback must
	// honor it; if its append has already committed, cancellation is not a
	// rollback and the caller may need to reconcile an unknown outcome.
	if err := commit(f.ctx); err != nil {
		return err
	}
	f.mu.Lock()
	f.committed = true
	f.mu.Unlock()
	return nil
}

func (f *Fence) finish() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = true
	return f.committed
}
