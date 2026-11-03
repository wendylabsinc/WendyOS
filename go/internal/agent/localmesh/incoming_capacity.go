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

func workingCarrierAtMost(view NodeSnapshot, peer meshid.Device, cost uint16) bool {
	if peer == (meshid.Device{}) || cost == 0 {
		return false
	}
	for _, link := range view.Links {
		if link.Device == peer && !link.Retiring && link.ForwardingReady && link.Cost <= cost {
			return true
		}
	}
	return false
}

func directPeerPresent(view NodeSnapshot, peer meshid.Device) bool {
	for _, link := range view.Links {
		if link.Device == peer && !link.Retiring {
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
	return false
}

func (p *ConnectionPolicy) notePendingIncomingDemand() {
	for l := range p.connections {
		if l.reclaimed || l.capacityClosing.Load() {
			l.incomingDemand = true
			return
		}
	}
}

func (p *ConnectionPolicy) clearIncomingDemand() {
	p.incomingDemandUntil = time.Time{}
	for l := range p.connections {
		l.incomingDemand = false
	}
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
	return p.incomingDemandActive() && workingCarrierAtMost(view, peer, cost)
}

func connectionReadyForCarrier(ctx context.Context, peer meshid.Device, cost uint16, closeConnection func()) bool {
	l, _ := ctx.Value(connectionLeaseKey{}).(*ConnectionLease)
	return l.readyWithCost(peer, cost, closeConnection)
}
