package derivedreader

import (
	"context"
	"errors"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type readOperation struct {
	ctx           context.Context
	clockError    error
	clockRPCError error
	history       map[string]*highwater
	verified      map[string]highwater
}

type readOperationKey struct{}
type operationClockClient struct{ api appv1.WorldAppServiceClient }

func noteClockRPC(ctx context.Context, err error) {
	if operation, _ := ctx.Value(readOperationKey{}).(*readOperation); operation != nil && err != nil {
		operation.clockRPCError = err
	}
}
func (c operationClockClient) Clock(ctx context.Context, q *appv1.ClockRequest, options ...grpc.CallOption) (*appv1.ClockReply, error) {
	r, err := c.api.Clock(ctx, q, options...)
	noteClockRPC(ctx, err)
	return r, err
}
func (c operationClockClient) PhysicalClock(ctx context.Context, q *appv1.ClockRequest, options ...grpc.CallOption) (*appv1.PhysicalClockReply, error) {
	r, err := c.api.PhysicalClock(ctx, q, options...)
	noteClockRPC(ctx, err)
	return r, err
}

type operationClock[T any] struct{ session *Session[T] }

func (c *operationClock[T]) Now() (trustedtime.Interval, error) {
	op := c.session.operation
	if op == nil {
		return trustedtime.Interval{}, ErrRefused
	}
	o, err := c.session.clock.PhysicalObserve(op.ctx)
	if err != nil {
		op.clockError = err
	}
	return o.Physical, err
}

func temporaryRead(err error) bool {
	if errors.Is(err, ErrUnavailable) || errors.Is(err, appclock.ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return false
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Session[T]) retainVerifiedWindow(p parsed[T]) {
	entity := p.record.Publication.Entity
	if p.retired {
		return
	}
	mark := s.operation.verified[entity]
	if prior := s.operation.history[entity]; prior != nil && prior.version.Compare(mark.version) > 0 {
		mark = *prior
	}
	mark.sampleUntil = later(mark.sampleUntil, earlier(p.payload.SampleValidUntil, p.record.Publication.Timing.SampleTime.Add(p.record.Publication.TTL-max(p.record.Publication.Timing.ErrorBound, p.record.Publication.Timing.AgeHint))))
	mark.physicalUntil = later(mark.physicalUntil, earlier(p.payload.PhysicalValidUntil, time.UnixMilli(p.record.ExpiresAtUnixMS)))
	mark.version.Epoch, mark.version.Sequence = p.record.Epoch, p.record.Sequence
	mark.hash, mark.verified = hash(p.raw), true
	s.operation.verified[entity] = mark
	s.water[entity] = mark
}

// Query observations are pending until the complete original batch verifies.
// Failed requests cannot leave one permanent unverified entry per transient ID.
func (s *Session[T]) stageHistory(entity string, prior highwater, exists bool) {
	if _, seen := s.operation.history[entity]; seen {
		return
	}
	if exists {
		copy := prior
		s.operation.history[entity] = &copy
	} else {
		s.operation.history[entity] = nil
	}
}
func (s *Session[T]) finishHistory() {
	for entity, prior := range s.operation.history {
		if verified, ok := s.operation.verified[entity]; ok {
			s.water[entity] = verified
		} else if prior != nil {
			s.water[entity] = *prior
		} else {
			delete(s.water, entity)
		}
	}
}

// Keep all observed version windows until the same pinned clock proves they
// cannot be shown again. The compact source floor prevents a pruned entity's
// older version from reappearing with a different, longer original window.
// An otherwise-current unseen record below the floor makes Step unavailable;
// it is never called absence. Live entity entries keep their individual marks.
func (s *Session[T]) pruneHistory(now appclock.Observation) {
	for entity, mark := range s.water {
		if !mark.verified || mark.sampleUntil.IsZero() || mark.physicalUntil.IsZero() {
			continue
		}
		if !now.SampleTime.Before(mark.sampleUntil) || !now.Physical.Latest.Before(mark.physicalUntil) {
			if mark.version.Compare(s.retired) > 0 {
				s.retired = mark.version
			}
			delete(s.water, entity)
		}
	}
}
