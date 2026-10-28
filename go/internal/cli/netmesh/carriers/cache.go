package carriers

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"time"
)

// Adapted from Joannis Orlandos's authenticated Darwin radio cache in PR #2466
// (199b6ae42cf8). Only completed TLS verification may populate this cache.
type knownPeer struct {
	candidate Candidate
	peer      meshid.Device
	verified  time.Time
}
type peerCache map[string]knownPeer

func (p peerCache) remember(c Candidate, peer meshid.Device, now time.Time, ttl time.Duration, maximum int) {
	for k, v := range p {
		if now.Sub(v.verified) >= ttl {
			delete(p, k)
		}
	}
	if _, ok := p[c.key()]; !ok && len(p) >= maximum {
		return
	}
	c.authenticated = nil
	p[c.key()] = knownPeer{c, peer, now}
}
func (p peerCache) peer(c Candidate, now time.Time, ttl time.Duration) meshid.Device {
	if v, ok := p[c.key()]; ok && now.Sub(v.verified) < ttl && v.candidate.sameAdvertisement(c) {
		return v.peer
	}
	return c.Device
}
