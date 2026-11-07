package localmesh

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
)

// A socket can remain attached after Babel has expired its neighbour. Retire
// only an incarnation whose two-way liveness was previously confirmed; setup,
// route-proof gaps and explicit Infinity IHUs do not spend a new grace timer.
// The actor signals the existing per-link lifetime owner instead of closing a
// potentially blocking Bluetooth/TLS stream or starting another goroutine.
func (n *Node) retireExpiredBabelPeers(snapshot babel.Snapshot) {
	for _, neighbour := range snapshot.Neighbours {
		p := n.peers[neighbour.Link]
		if p == nil {
			continue
		}
		if neighbour.Reachable {
			p.babelConfirmed = true
		}
		if !p.babelConfirmed || !neighbour.LivenessExpired || p.babelRetiring || p.retire == nil {
			continue
		}
		p.babelRetiring = true
		p.signalRetire()
		asyncdiag.Print(fmt.Sprintf("wendy net local carrier retirement peer=%q link=%d transport=%q reason=%q",
			p.device.Principal(), p.id, diagnosticText(p.transport, 32), "babel-liveness-expired"))
	}
}

func (p *nodePeer) signalRetire() {
	p.retireOnce.Do(func() { close(p.retire) })
}

func (p *nodePeer) signalCapacityRetire() {
	p.retireOnce.Do(func() {
		p.capacityRetire.Store(true)
		if p.capacityLease != nil {
			p.capacityLease.capacityClosing.Store(true)
		}
		close(p.retire)
	})
}

func (p *nodePeer) retiring() bool {
	if p.babelRetiring {
		return true
	}
	select {
	case <-p.retire:
		return true
	default:
		return false
	}
}

// Exactly one lifetime owner belongs to each admitted attachment. It also
// handles node/attachment cancellation, and Attach joins it during cleanup.
func (n *Node) closePeerOnDone(ctx context.Context, cancel context.CancelFunc, p *nodePeer) {
	select {
	case <-n.ctx.Done():
		cancel()
	case <-ctx.Done():
	case <-p.retire:
		cancel()
	}
	_ = p.link.Close()
}
