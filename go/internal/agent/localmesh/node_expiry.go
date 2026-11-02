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
		// This actor is the only producer. A concurrent writer can only consume
		// this prefix; retained messages are put back in their original order.
		count := len(p.control)
		retained := make([]ControlMessage, 0, count)
		for range count {
			select {
			case m := <-p.control:
				if !m.Expired(now) {
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
	}
}

func (n *Node) pruneReplies(p *nodePeer, now time.Time) {
	kept := p.replies[:0]
	p.replyBytes = 0
	for _, m := range p.replies {
		if m.Expired(now) {
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
