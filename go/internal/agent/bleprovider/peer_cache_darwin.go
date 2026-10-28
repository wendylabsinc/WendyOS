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

func (c *darwinPeerCache) suppressed(address string, adv Advertisement, selection *localmesh.PeerSelection, now time.Time) bool {
	c.mu.Lock()
	known, ok := c.peers[address]
	c.mu.Unlock()
	return ok && known.advertisement == adv && now.Sub(known.verified) < time.Minute &&
		selection != nil && !selection.AllowRadio(known.peer, localmesh.RadioBLE)
}
