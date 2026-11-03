package localmesh

import (
	"context"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// SetCarrierCost is local scheduling metadata, never a discovery assertion.
// Mixed-cost LAN providers supply their locally constructed numeric group.
func (p *ConnectionPolicy) SetCarrierCost(cost uint16) {
	p.mu.Lock()
	p.carrierCost = cost
	p.mu.Unlock()
}

func (p *ConnectionPolicy) costForGroup(group string) uint16 {
	if value, err := strconv.ParseUint(group, 10, 16); err == nil && value != 0 && value != 65535 {
		return uint16(value)
	}
	return p.carrierCost
}

func workingCarrierPresent(view NodeSnapshot, peer meshid.Device) bool {
	if peer == (meshid.Device{}) {
		return false
	}
	for _, link := range view.Links {
		if link.Device == peer && !link.Retiring && link.ForwardingReady {
			return true
		}
	}
	return false
}

func directPeerPresent(view NodeSnapshot, peer meshid.Device) bool {
	for _, link := range view.Links {
		if link.Device == peer {
			return true
		}
	}
	return false
}

// incomingDemandActive runs under the policy lock. A pending owner still
// consumes capacity; only its actual release starts the finite interval.
func (p *ConnectionPolicy) incomingDemandActive() bool {
	if p.now().Before(p.incomingDemandUntil) {
		return true
	}
	for l := range p.connections {
		if l.incomingDemand {
			return true
		}
	}
	p.incomingDemandPeers = nil
	return false
}

func (p *ConnectionPolicy) notePendingIncomingDemand() {
	for l := range p.connections {
		if l.reclaimed || l.capacityClosing.Load() {
			l.incomingDemand = true
			p.rememberIncomingDemandPeer(l.peer)
			return
		}
	}
}

func (p *ConnectionPolicy) clearIncomingDemand() {
	p.incomingDemandUntil = time.Time{}
	p.incomingDemandPeers = nil
	for l := range p.connections {
		l.incomingDemand = false
		l.demandAtSetup = false
	}
}

func (p *ConnectionPolicy) rememberIncomingDemandPeer(peer meshid.Device) {
	if !peer.IsValid() {
		return
	}
	if p.incomingDemandPeers == nil {
		p.incomingDemandPeers = make(map[meshid.Device]struct{})
	}
	if len(p.incomingDemandPeers) < maxCandidateHistory {
		p.incomingDemandPeers[peer] = struct{}{}
	}
}

func (p *ConnectionPolicy) incomingDemandKnows(peer meshid.Device) bool {
	_, known := p.incomingDemandPeers[peer]
	return known
}

// DeferRedundantCarrier is a bounded scheduling hint gate. It never supplies
// authentication or forwarding authority, and always permits path recovery.
func (p *ConnectionPolicy) DeferRedundantCarrier(peer meshid.Device, cost uint16) bool {
	p.mu.Lock()
	snapshot := p.snapshot
	p.mu.Unlock()
	if snapshot == nil {
		return false
	}
	view := snapshot()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.incomingDemandActive() && p.limits.Total-p.counts().Total <= 1 && workingCarrierPresent(view, peer)
}

func connectionReadyForCarrier(ctx context.Context, peer meshid.Device, cost uint16, closeConnection func()) bool {
	l, _ := ctx.Value(connectionLeaseKey{}).(*ConnectionLease)
	captureIncomingWorker(ctx, l)
	return l.readyWithCost(peer, cost, closeConnection)
}

// DeferIncomingCarrier also protects the accept loop's separate final worker
// token. Native quota spare alone cannot admit a leaf when that gate is full.
func (p *ConnectionPolicy) DeferIncomingCarrier(peer meshid.Device, cost uint16, freeWorkers int) bool {
	p.mu.Lock()
	snapshot := p.snapshot
	p.mu.Unlock()
	if snapshot == nil {
		return false
	}
	view := snapshot()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.incomingDemandActive() && (p.limits.Total-p.counts().Total <= 1 || freeWorkers <= 1) && workingCarrierPresent(view, peer)
}

type lastIncomingWorkerKey struct{}

// WithLastIncomingWorker records local setup capacity, never peer identity.
func WithLastIncomingWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, lastIncomingWorkerKey{}, true)
}

func captureIncomingWorker(ctx context.Context, l *ConnectionLease) {
	if l == nil {
		return
	}
	if last, _ := ctx.Value(lastIncomingWorkerKey{}).(bool); last {
		l.policy.mu.Lock()
		l.lastIncomingWorker = true
		l.policy.mu.Unlock()
	}
}
