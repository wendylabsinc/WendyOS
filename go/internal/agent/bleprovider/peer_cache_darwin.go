//go:build darwin

package bleprovider

import (
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Only a completed, verified TLS handshake populates this scheduling cache.
// A radio hint alone must never suppress another peer's fallback connection.
type darwinPeerCache struct {
	mu    sync.Mutex
	peers map[string]darwinKnownPeer
}
type darwinKnownPeer struct {
	advertisement Advertisement
	peer          meshid.Device
	verified      time.Time
}

func (c *darwinPeerCache) remember(address string, adv Advertisement, peer meshid.Device, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peers == nil {
		c.peers = make(map[string]darwinKnownPeer)
	}
	for key, known := range c.peers {
		if now.Sub(known.verified) >= time.Minute {
			delete(c.peers, key)
		}
	}
	if _, exists := c.peers[address]; !exists && len(c.peers) >= 256 {
		return
	}
	c.peers[address] = darwinKnownPeer{adv, peer, now}
}

func newDarwinConnectionPolicy(self meshid.Device, snapshot func() localmesh.NodeSnapshot) *localmesh.ConnectionPolicy {
	policy := localmesh.NewConnectionPolicy(self, true, localmesh.ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 2})
	policy.SetSnapshot(snapshot)
	policy.SetCarrierCost(LinkCost)
	return policy
}

// The verified identity is a scheduling hint, not authentication. Changed or
// expired advertisements must authenticate again before their identity is known.
func (c *darwinPeerCache) known(address string, adv Advertisement, now time.Time) meshid.Device {
	c.mu.Lock()
	known, ok := c.peers[address]
	c.mu.Unlock()
	if ok && known.advertisement == adv && now.Sub(known.verified) < time.Minute {
		return known.peer
	}
	return meshid.Device{}
}

func (c *darwinPeerCache) reserve(policy *localmesh.ConnectionPolicy, candidates *localmesh.CandidateQueue, address string, adv Advertisement, now time.Time) *localmesh.ConnectionLease {
	return policy.ReservePreferred(address, c.known(address, adv, now), true, "", candidates.ReclaimEligible(address, now))
}
