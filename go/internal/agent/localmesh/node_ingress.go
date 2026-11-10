package localmesh

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"io"
	"time"
)

// One unacknowledged input per neighbour prevents a fast reader from filling
// the shared actor event queue with response obligations for one slow writer.
// Cancellation must release this wait without requiring the whole Node to stop.
func (n *Node) readControls(ctx context.Context, p *nodePeer) error {
	ack := make(chan error, 1)
	for {
		message, err := p.link.ReadControl()
		if err != nil {
			return err
		}
		select {
		case n.events <- nodeEvent{id: p.id, control: &message, controlResult: ack}:
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return io.ErrClosedPipe
		}
		select {
		case err := <-ack:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return io.ErrClosedPipe
		}
	}
}

func (n *Node) admitIngress(p *nodePeer, event nodeEvent) error {
	if p.ingress != nil {
		return errors.New("multiple unacknowledged directory inputs")
	}
	// One decoded frame can wait behind output credit, but never own its
	// payload indefinitely. This is a local resource bound, not a claim lease.
	if event.control != nil && event.control.deadline.IsZero() {
		event.control.deadline = time.Now().Add(netgossip.HintLifetime)
	}
	p.ingress = &event
	return n.continueIngress(p)
}

// The existing maintenance tick resumes input after its writer drains. Bulk
// polling/reconciliation leaves eight slots reserved for solicited responses;
// those responses may use them, preserving duplex progress under ordinary load.
func (n *Node) continueIngress(p *nodePeer) error {
	if p.ingress == nil {
		return nil
	}
	if p.ingress.control != nil && p.ingress.control.Expired(time.Now()) {
		event := p.ingress
		p.ingress = nil
		err := errors.New("directory input resource deadline elapsed")
		if event.controlResult != nil {
			event.controlResult <- err
		}
		n.closePeer(p, "directory-ingress-timeout", err)
		return nil
	}
	n.flushReplies(p)
	if directoryResponseSlots(p) < directoryResponseDemand(*p.ingress.control) {
		return nil
	}
	event := p.ingress
	err := n.handleControl(p, event.control)
	if errors.Is(err, ErrControlBackpressure) {
		return nil
	} // No state was consumed.
	p.ingress = nil
	if event.controlResult != nil {
		event.controlResult <- err
	}
	return err
}
