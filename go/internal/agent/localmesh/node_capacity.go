package localmesh

import (
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// ErrCarrierCapacityRetry is a local scheduling deferral, not a failed peer.
// Callers retry through their existing poll/backoff after owned cleanup frees
// capacity. It adds no wire field and never permits a 65th Node attachment.
var ErrCarrierCapacityRetry = errors.New("wendy net link capacity reclamation pending")

// reclaimGlobalRedundancy runs only on the Node actor, after its liveness tick
// and snapshot refresh. Unlike provider budgets, this is the existing shared
// 64-attachment ceiling. One pending owned retirement must complete first.
func (n *Node) reclaimGlobalRedundancy(candidate meshid.Device) bool {
	if !candidate.IsValid() || candidate == n.Credentials.Device || candidate.Tenant() != n.Credentials.Device.Tenant() {
		return false
	}
	_, recovering := n.capacityHoldPeers[candidate]
	for _, p := range n.peers {
		if p.device == candidate && !p.retiring() {
			return false // An upgrade uses spare capacity, never a last-path swap.
		}
		if p.device == candidate {
			recovering = true
		}
	}
	// Any already-closing incarnation will release this same global ceiling.
	// Provider pressure and liveness expiry must not provoke another eviction.
	for _, p := range n.peers {
		if p.retiring() {
			if !recovering {
				n.rememberCapacityHoldPeer(p.device)
			}
			if !recovering && (n.capacityHoldRelease == 0 || n.peers[n.capacityHoldRelease] == nil) {
				// Actual new-peer pressure can use an already-closing slot.
				// Anchor its hold to ordinary Remove, never to this observation.
				n.capacityHoldRelease = p.id
			}
			return true
		}
	}
	if n.capacityRetiring != 0 {
		if n.peers[n.capacityRetiring] != nil {
			return true
		}
		n.capacityRetiring = 0
	}
	view := n.view.Load()
	if view == nil {
		return false
	}
	var victim *nodePeer
	for _, link := range view.Links {
		p := n.peers[link.ID]
		if p == nil || p.retire == nil || p.retiring() || p.id != link.ID || p.device != link.Device || p.cost != link.Cost || view.HasInstalledRouteOnLink(link.ID) {
			continue
		}
		for _, alternate := range view.Links {
			other := n.peers[alternate.ID]
			if alternate.ID != link.ID && alternate.Device == link.Device && alternate.ForwardingReady && !alternate.Retiring &&
				other != nil && !other.retiring() && other.id == alternate.ID && other.device == alternate.Device && other.cost == alternate.Cost {
				if victim == nil || p.cost > victim.cost || (p.cost == victim.cost && p.id < victim.id) {
					victim = p
				}
			}
		}
	}
	if victim == nil {
		return false
	}
	n.capacityRetiring = victim.id
	if !recovering {
		n.capacityHoldRelease = victim.id
		n.rememberCapacityHoldPeer(victim.device)
	}
	victim.signalCapacityRetire()
	n.refresh()
	return true
}

// capacityHoldActive reserves a released global slot for a new peer's ordinary
// retry cadence. It reuses the existing failure-penalty duration and monotonic
// routing clock, without a new timer or an indefinitely sticky demand bit.
func (n *Node) capacityHoldActive() bool {
	if n.capacityHoldRelease != 0 && n.peers[n.capacityHoldRelease] != nil {
		return true
	}
	if n.capacityHoldUntil == 0 {
		return false
	}
	if n.routingElapsed() >= n.capacityHoldUntil {
		n.capacityHoldUntil = 0
		n.capacityHoldPeers = nil
		return false
	}
	return true
}

func (n *Node) rememberCapacityHoldPeer(peer meshid.Device) {
	if n.capacityHoldPeers == nil {
		n.capacityHoldPeers = make(map[meshid.Device]struct{})
	}
	if len(n.capacityHoldPeers) < maxCandidateHistory {
		n.capacityHoldPeers[peer] = struct{}{}
	}
}
