package backend

import (
	"context"
	"sync"
	"time"
)

// UnitreeAttempts lets Swift cancel a blocking bridge call, including a call
// that has not started on its dispatch queue yet.
type UnitreeAttempts struct {
	mu    sync.Mutex
	next  uint64
	items map[uint64]unitreeAttempt
}

type unitreeAttempt struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (a *UnitreeAttempts) New() uint64 {
	return a.newWithTimeout(30 * time.Second)
}

func (a *UnitreeAttempts) newWithTimeout(timeout time.Duration) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.items == nil {
		a.items = make(map[uint64]unitreeAttempt)
	}
	a.next++
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	a.items[a.next] = unitreeAttempt{ctx, cancel}
	return a.next
}

func (a *UnitreeAttempts) Context(id uint64) context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if attempt, ok := a.items[id]; ok {
		return attempt.ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func (a *UnitreeAttempts) Cancel(id uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if attempt, ok := a.items[id]; ok {
		attempt.cancel()
	}
}

func (a *UnitreeAttempts) Release(id uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if attempt, ok := a.items[id]; ok {
		attempt.cancel()
		delete(a.items, id)
	}
}
