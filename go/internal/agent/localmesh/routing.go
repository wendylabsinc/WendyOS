package localmesh

import (
	"errors"
	"time"

	"github.com/wendylabsinc/WendyOS/babel"
)

// RoutingIO is the fail-closed forwarding barrier. Apply must pause tunnel
// ingress/egress, apply the COMPLETE desired FIB (including unreachable routes),
// and leave it paused until Resume. Stop must disable forwarding, even if Apply
// only partly succeeded. Persist must be durable, not an asynchronous write.
type RoutingIO interface {
	Apply([]babel.Route) error
	Persist(babel.Checkpoint) error
	Resume() error
	Stop() error
	Send(babel.Datagram) error
}

// Routing serializes the sans-I/O engine's transaction protocol. The embedding
// event loop supplies monotonic time and calls Step at NextDeadline, as well as
// for link and packet events. No method may be called concurrently.
type Routing struct {
	engine  *babel.Engine
	io      RoutingIO
	stopped bool
}

func NewRouting(config babel.Config, checkpoint *babel.Checkpoint, io RoutingIO) (*Routing, error) {
	if io == nil {
		return nil, errors.New("missing routing I/O")
	}
	var e *babel.Engine
	var err error
	if checkpoint == nil {
		e, err = babel.New(config)
	} else {
		e, err = babel.Restore(config, *checkpoint)
	}
	if err != nil {
		return nil, err
	}
	return &Routing{engine: e, io: io}, nil
}

func (r *Routing) Step(now time.Duration, event babel.Event) error {
	if r.stopped {
		return babel.ErrStopped
	}
	effects, err := r.engine.Step(now, event)
	if err != nil {
		return err
	}
	if effects.Revision != 0 {
		if err = r.io.Apply(effects.Routes); err != nil {
			_, _ = r.engine.Commit(effects.Revision, false)
			return r.fail(err)
		}
		effects, err = r.engine.Commit(effects.Revision, true)
		if err != nil {
			return r.fail(err)
		}
	}
	// Link incarnation counters and feasibility history are persisted even on
	// an event with no outgoing datagrams. This also handles first startup.
	if err = r.io.Persist(r.engine.Checkpoint()); err != nil {
		return r.fail(err)
	}
	if err = r.io.Resume(); err != nil {
		return r.fail(err)
	}
	for _, d := range effects.Datagrams {
		// Datagram congestion/loss is recovered by Babel, not replaying Step.
		_ = r.io.Send(d)
	}
	return nil
}

func (r *Routing) fail(err error) error {
	r.stopped = true
	return errors.Join(err, r.io.Stop())
}

func (r *Routing) NextDeadline() (time.Duration, bool) { return r.engine.NextDeadline() }
func (r *Routing) Snapshot() babel.Snapshot            { return r.engine.Snapshot() }
func (r *Routing) Checkpoint() babel.Checkpoint        { return r.engine.Checkpoint() }
