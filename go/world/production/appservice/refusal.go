package appservice

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
)

// Observations accompany the same call that decides liveness. They never
// cache a result for another call, expose raw errno, or change the returned
// ErrPeer identity. Other pin implementations still receive one Alive call.
type pinObservation struct {
	reason        rpctrace.RefusalReason
	interruptions uint8
}

type diagnosticPeerPin interface {
	aliveDiagnostic() (error, pinObservation)
}

func observePeerPin(pin peerPin, diagnostic bool) (error, pinObservation) {
	if diagnostic {
		if observed, ok := pin.(diagnosticPeerPin); ok {
			return observed.aliveDiagnostic()
		}
	}
	err := pin.Alive()
	if err != nil && diagnostic {
		return err, pinObservation{reason: rpctrace.ReasonPinUnknown}
	}
	return err, pinObservation{}
}

func contextRefusal(err error) rpctrace.RefusalReason {
	if errors.Is(err, context.DeadlineExceeded) {
		return rpctrace.ReasonContextDeadline
	}
	return rpctrace.ReasonContextCanceled
}

func (s *service) refuse(ctx context.Context, point rpctrace.RefusalPoint, reason rpctrace.RefusalReason, conn *trackedConn) {
	if !rpctrace.Enabled(ctx) {
		return
	}
	var watcher rpctrace.RefusalReason
	if conn != nil {
		watcher = rpctrace.RefusalReason(conn.watcherReason.Load())
	}
	if watcher == 0 {
		watcher = rpctrace.RefusalReason(s.watcherReason.Load())
	}
	rpctrace.Refuse(ctx, point, reason, watcher)
}

func (s *service) peerAlive(ctx context.Context, point rpctrace.RefusalPoint, conn *trackedConn) error {
	diagnostic := rpctrace.Enabled(ctx)
	err, observed := observePeerPin(conn.pin, diagnostic)
	if diagnostic {
		rpctrace.PinInterrupted(ctx, point, observed.interruptions)
		if err != nil {
			s.refuse(ctx, point, observed.reason, conn)
		}
	}
	return err
}

func (s *service) rememberAuthorityWatcher(reason rpctrace.RefusalReason) {
	if s.manager.cfg.Trace != nil {
		s.watcherReason.CompareAndSwap(0, uint32(reason))
	}
}

// Watch events are emitted only after the original close/stop operation. Total
// measures diagnostic emission setup, not the earlier poll or validation.
// A retained watcher reason is an observation before a close attempt, not proof
// that this watcher won a concurrent close or caused another RPC's refusal.
func (s *service) traceWatcher(method rpctrace.Method, point rpctrace.RefusalPoint, observed pinObservation, failed bool) {
	if s.manager.cfg.Trace == nil || !failed && observed.interruptions == 0 {
		return
	}
	ctx, finish := s.traceContext(s.ctx, method)
	rpctrace.PinInterrupted(ctx, point, observed.interruptions)
	code := rpctrace.OK
	if failed {
		rpctrace.Refuse(ctx, point, observed.reason, 0)
		code = rpctrace.Unavailable
	}
	finish(code)
}
