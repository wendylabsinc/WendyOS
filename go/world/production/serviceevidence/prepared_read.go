package serviceevidence

import (
	"context"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// PreparedRead records immutable content facts for one host operation. It is
// not reader authority or a clock lease. The host must keep checking current
// grants, lifecycles and original deadlines, and close it when its operation
// finishes. No content or writable validity field is exposed by the handle.
type PreparedRead struct {
	store    *Store
	ctx      context.Context
	ref      Reference
	observed trustedtime.Interval
	closed   atomic.Bool
}

// PrepareRead performs the ordinary integrity/retention read exactly once.
// The returned bytes and reference are owned copies; mutating either cannot
// change the handle's exact metadata. No lock survives the call.
func (s *Store) PrepareRead(ctx context.Context, o Owner, sha string, size int64, contentType string) (*PreparedRead, Reference, []byte, error) {
	if s == nil || ctx == nil || !reflect.TypeOf(ctx).Comparable() {
		return nil, Reference{}, nil, ErrClosed
	}
	s.traceLock(ctx)
	defer s.mu.Unlock()
	ref, data, err := s.readCompleteLocked(ctx, o, sha, size, contentType)
	if err != nil {
		return nil, Reference{}, nil, err
	}
	owner, _ := s.owner(o)
	if err = s.inner.ValidateProtectedOwned(owner, evidence.Reference{SHA256: sha, Size: size, ContentType: contentType}, ref.RetainUntilUnixMS); err != nil {
		return nil, Reference{}, nil, err
	}
	return &PreparedRead{store: s, ctx: ctx, ref: ref, observed: s.observed}, ref, data, nil
}

func (p *PreparedRead) Close() {
	if p != nil {
		p.closed.Store(true)
	}
}

// ValidatePrepared checks current store health and exact protected metadata
// against a fresh physical interval already acquired by the host. It does no
// clock I/O, content read, pruning or persistence. Normal APIs cannot evict or
// rewrite protected completed content before this original retained deadline.
// Later media access still performs its ordinary disk integrity read.
func (s *Store) ValidatePrepared(ctx context.Context, p *PreparedRead, w trustedtime.Interval) error {
	if s == nil || p == nil || p.store != s || ctx == nil || !reflect.TypeOf(ctx).Comparable() || ctx != p.ctx || p.ctx == nil || ctx.Err() != nil || p.ctx.Err() != nil || p.closed.Load() {
		return ErrDenied
	}
	s.traceLock(ctx)
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.fault != nil {
		return s.fault
	}
	if p.closed.Load() || ctx.Err() != nil || p.ctx.Err() != nil {
		return ErrDenied
	}
	if w.Validate() != nil || w.BootID != p.observed.BootID || !forward(p.observed, w) || !w.Latest.Before(time.UnixMilli(p.ref.RetainUntilUnixMS)) {
		return ErrUnavailable
	}
	// A concurrent operation may have already observed expiry. It must not be
	// undone by the caller's earlier interval or an allocation with same bytes.
	if !s.observed.Latest.Before(time.UnixMilli(p.ref.RetainUntilUnixMS)) {
		return ErrUnavailable
	}
	owner, err := s.owner(p.ref.Owner)
	if err != nil {
		return err
	}
	return s.inner.ValidateProtectedOwned(owner, evidence.Reference{SHA256: p.ref.SHA256, Size: p.ref.Size, ContentType: p.ref.ContentType}, p.ref.RetainUntilUnixMS)
}
