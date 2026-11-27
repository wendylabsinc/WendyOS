package replicastore

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

type operationKey struct{}
type operation struct {
	store  *Store
	active atomic.Bool
}

// This private outcome is created only when the host guard returns an error
// without calling its callback. It cannot describe an entered store operation.
type guardNotEntered struct{ cause error }

func (e *guardNotEntered) Error() string { return "replica host guard not entered: " + e.cause.Error() }
func (e *guardNotEntered) Unwrap() error { return e.cause }

func (s *Store) usable() error {
	if s == nil || s.closed.Load() {
		return ErrClosed
	}
	if s.faulted.Load() {
		return ErrFaulted
	}
	return nil
}
func (s *Store) inside(ctx context.Context) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	op, ok := ctx.Value(operationKey{}).(*operation)
	return ok && op.store == s && op.active.Load()
}
func (s *Store) guard(ctx context.Context, fn func(context.Context) error) error {
	if ctx == nil {
		return ErrDenied
	}
	if err := s.usable(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Public operations never reuse a nested/retained context. Only private
	// journal callbacks may pass through the already held operation fence.
	if ctx.Value(operationKey{}) != nil {
		return ErrDenied
	}
	var calls int
	var callbackErr error
	err := s.cfg.Guard(ctx, func() error {
		calls++
		if calls != 1 {
			callbackErr = ErrDenied
			return callbackErr
		}
		if e := s.usable(); e != nil {
			callbackErr = e
			return e
		}
		if e := ctx.Err(); e != nil {
			callbackErr = e
			return e
		}
		op := &operation{store: s}
		op.active.Store(true)
		defer op.active.Store(false)
		callbackErr = fn(context.WithValue(ctx, operationKey{}, op))
		return callbackErr
	})
	if calls == 0 && err != nil {
		return &guardNotEntered{cause: err}
	}
	if calls != 1 {
		return ErrDenied
	}
	if callbackErr != nil {
		return callbackErr
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}
func (s *Store) journalGuard(ctx context.Context, fn func() error) error {
	if !s.inside(ctx) {
		return ErrDenied
	}
	if e := s.usable(); e != nil {
		return e
	}
	err := fn()
	if err != nil {
		return err
	}
	if !s.inside(ctx) {
		return ErrDenied
	}
	return nil
}
func windowCurrent(w servicejournal.AuthorityWindow, now trustedtime.Interval) bool {
	return now.Validate() == nil && !w.NotBefore.IsZero() && w.NotAfter.After(w.NotBefore) && !now.Earliest.Before(w.NotBefore) && now.Latest.Before(w.NotAfter)
}
func (s *Store) check(ctx context.Context, d replication.Direction, sources []proofref.Source) (servicejournal.AuthorityWindow, error) {
	return s.checkWindow(ctx, d, sources, nil)
}
func (s *Store) checkWindow(ctx context.Context, d replication.Direction, sources []proofref.Source, original *servicejournal.AuthorityWindow) (servicejournal.AuthorityWindow, error) {
	if !s.inside(ctx) || !s.sourcesAllowed(d, sources) {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	before, e := s.cfg.Clock.Now()
	if e != nil || before.Validate() != nil {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	w, e := s.cfg.Authorize(ctx, s.cfg.Peer, s.cfg.Generation, d, append([]proofref.Source(nil), sources...), before)
	if e != nil {
		return servicejournal.AuthorityWindow{}, e
	}
	if original != nil {
		if w.NotBefore.Before(original.NotBefore) {
			w.NotBefore = original.NotBefore
		}
		if original.NotAfter.Before(w.NotAfter) {
			w.NotAfter = original.NotAfter
		}
	}
	after, e := s.cfg.Clock.Now()
	if e != nil || after.BootID != before.BootID || after.Monotonic < before.Monotonic || after.Earliest.Before(before.Earliest) || after.Latest.Before(before.Latest) || !windowCurrent(w, after) || !s.inside(ctx) {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	return w, nil
}
func (s *Store) mutationFault(err error) error {
	s.faulted.Store(true)
	return errors.Join(ErrFaulted, replication.ErrUncertain, err)
}
