package localmesh

import "github.com/wendylabsinc/wendy/go/internal/agent/netwire"

const maxDirectoryReplyFrames = 32
const maxDirectoryReplyBytes = 64 << 10

// Indexed certificate descriptors (at most16 hashes), identity requests and
// 1024-byte certificate/gossip chunks all fit this bound. It is a credit unit
// for those response producers, never an encoded-message size restriction.
const directoryResponseBytes = 2 << 10

// queueReplies retains requested protocol data under ordinary backpressure.
// A dropped request/chunk cannot be repaired by resending only the manifest.
// The node actor owns this bounded reserve; admitted bulk frames keep order.
func (n *Node) queueReplies(p *nodePeer, messages []ControlMessage) bool {
	n.flushReplies(p)
	admitted := make([]ControlMessage, 0, len(messages))
	sizes := make([]int, 0, len(messages))
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
		wire, err := netwire.Marshal(message)
		if err != nil {
			return false
		}
		admitted = append(admitted, message)
		sizes = append(sizes, len(wire))
	}
	// The writer only consumes this channel; the actor is its sole producer.
	// These free prefix slots therefore cannot disappear during this preflight.
	// Preserve ordering with an older retained tail before placing new replies.
	free := cap(p.control) - len(p.control)
	tailFrames, tailBytes := 0, 0
	account := func(size int) {
		if free > 0 {
			free--
		} else {
			tailFrames++
			tailBytes += size
		}
	}
	for _, message := range p.replies {
		wire, err := netwire.Marshal(message)
		if err != nil {
			return false
		}
		account(len(wire))
	}
	for _, size := range sizes {
		account(size)
	}
	if tailFrames > maxDirectoryReplyFrames || tailBytes > maxDirectoryReplyBytes {
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
	for _, size := range sizes {
		p.replyBytes += size
	}
	n.flushReplies(p)
	return true
}

func (n *Node) flushReplies(p *nodePeer) {
	for len(p.replies) != 0 {
		select {
		case p.control <- p.replies[0]:
			wire, _ := netwire.Marshal(p.replies[0])
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
	return max(0, directoryResponseSlots(p)-directoryResponseReserve)
}

func directoryResponseSlots(p *nodePeer) int {
	retained := min(maxDirectoryReplyFrames-len(p.replies),
		max(0, maxDirectoryReplyBytes-p.replyBytes)/directoryResponseBytes)
	return max(0, cap(p.control)-len(p.control)+retained)
}
