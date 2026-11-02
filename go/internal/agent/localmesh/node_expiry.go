package localmesh

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"time"
)

// sweepAnnouncements runs even without ingress or output credit. A pool owns
// transfer storage, while the actor owns independent reply/channel copies; both
// must release expired payloads. Certificate/session lifetime is separate.
func (n *Node) sweepAnnouncements(now time.Time) {
	if n.directory != nil {
		n.directory.Sweep(now)
	}
	if n.controlHub != nil {
		n.controlHub.Announcements().Sweep(now)
	}
	for _, p := range n.peers {
		if p.sync != nil {
			p.sync.Sweep(now)
		}
		n.pruneReplies(p, now)
		// This actor is the only producer. Serialize removal/reinsertion with
		// the writer's dequeue so live controls cannot overtake each other.
		p.controlMu.Lock()
		count := len(p.control)
		retained := make([]ControlMessage, 0, count)
		for range count {
			select {
			case m := <-p.control:
				if n.controlEligible(p, m, now) {
					retained = append(retained, m)
				} else if m.Kind == "bundle" {
					p.pendingBundles.Delete(Fingerprint(m.Bundle))
				}
			default:
			}
		}
		for _, m := range retained {
			p.control <- m
		}
		p.controlMu.Unlock()
		if len(retained) != 0 {
			p.wakeControl()
		}
	}
}

func (n *Node) pruneReplies(p *nodePeer, now time.Time) {
	kept := p.replies[:0]
	p.replyBytes = 0
	for _, m := range p.replies {
		if !n.controlEligible(p, m, now) {
			if m.Kind == "bundle" {
				p.pendingBundles.Delete(Fingerprint(m.Bundle))
			}
			continue
		}
		kept = append(kept, m)
		data, _ := netwire.Marshal(m)
		p.replyBytes += len(data)
	}
	clear(p.replies[len(kept):])
	p.replies = kept
}

// Suppressing an untrusted export must not erase a still-bounded cached body:
// it can be needed for identity-only repair after certificate rotation. The
// Directory independently revalidates its signing chain before any actual send.
func (n *Node) controlEligible(p *nodePeer, m ControlMessage, now time.Time) bool {
	if m.Expired(now) {
		return false
	}
	if p.sync == nil || m.Gossip == nil || (m.Gossip.Op != "have" && m.Gossip.Op != "data") {
		return true
	}
	return m.origin != "" && n.directory != nil && n.directory.hasCurrentOwner(m.origin, now)
}
