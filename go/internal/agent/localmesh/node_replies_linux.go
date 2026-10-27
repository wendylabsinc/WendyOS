//go:build linux

package localmesh

import "encoding/json"

const maxDirectoryReplyFrames = 32
const maxDirectoryReplyBytes = 64 << 10

// queueReplies retains requested protocol data under ordinary backpressure.
// A dropped request/chunk cannot be repaired by resending only the manifest.
// The node actor owns this bounded reserve; admitted bulk frames keep order.
func (n *Node) queueReplies(p *nodePeer, messages []ControlMessage) bool {
	n.flushReplies(p)
	bytes := 0
	admitted := make([]ControlMessage, 0, len(messages))
	bundles := make(map[string]struct{})
	for _, message := range messages {
		if message.Kind == "bundle" {
			fingerprint := Fingerprint(message.Bundle)
			if _, pending := p.pendingBundles.Load(fingerprint); pending {
				continue
			}
			if _, duplicate := bundles[fingerprint]; duplicate {
				continue
			}
			bundles[fingerprint] = struct{}{}
		}
		wire, err := json.Marshal(message)
		if err != nil {
			return false
		}
		bytes += len(wire)
		admitted = append(admitted, message)
	}
	if len(p.replies)+len(admitted) > maxDirectoryReplyFrames || p.replyBytes+bytes > maxDirectoryReplyBytes {
		return false
	}
	// Requested leaf bundles share admission tokens with ordinary gossip.
	// Reserve them only after the entire batch fits, so rejected replies do
	// not suppress a later repair. The node actor owns all token additions;
	// the writer only releases a token after its corresponding frame exits.
	for fingerprint := range bundles {
		p.pendingBundles.Store(fingerprint, struct{}{})
	}
	p.replies = append(p.replies, admitted...)
	p.replyBytes += bytes
	n.flushReplies(p)
	return true
}

func (n *Node) flushReplies(p *nodePeer) {
	for len(p.replies) != 0 {
		select {
		case p.control <- p.replies[0]:
			wire, _ := json.Marshal(p.replies[0])
			p.replyBytes -= len(wire)
			p.replies[0] = ControlMessage{}
			p.replies = p.replies[1:]
		default:
			return
		}
	}
}

// Reserve room for unsolicited arrivals of up to four requested responses and
// their identity-repair messages while bulk transfer scheduling is paused.
func directoryReplySlots(p *nodePeer) int {
	return max(0, cap(p.control)-len(p.control)+maxDirectoryReplyFrames-len(p.replies)-8)
}
