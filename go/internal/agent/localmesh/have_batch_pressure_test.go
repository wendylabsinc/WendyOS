package localmesh

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// Exercise the production actor queue, response reserve, batching, per-origin
// writer filter and actual framing with a deterministic 8 kB/s duplex carrier.
// The 1 ms scheduling quantum is a simulation clock, not measured host latency.
func TestManifestHaveBatchSlowPressuredConvergence(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f := newHaveBatchFixture(t, 33)
		f.sender.haveBatches = enabled
		f.receiver.haveBatches = enabled
		type endpoint struct {
			node    *Node
			peer    *nodePeer
			link    *haveBatchCapture
			ingress []ControlMessage
			free    time.Duration
		}
		ends := [2]endpoint{}
		for i, s := range []*Synchronizer{f.sender, f.receiver} {
			link := &haveBatchCapture{}
			ends[i] = endpoint{node: &Node{directory: s.directory, cache: s.cache}, peer: &nodePeer{sync: s, haveBatches: enabled, control: make(chan ControlMessage, 1), link: link}, link: link}
		}
		// The receiver already owes a full channel plus the fixed 32-frame reserve.
		// These valid no-op negative replies consume real encoded link time.
		filler := ControlMessage{Kind: "identity-missing", Principal: testPrincipal(testTenantUUID, "00000000-0000-4000-8000-999999999999")}
		for range maxDirectoryReplyFrames + 1 {
			if !ends[1].node.queueReplies(ends[1].peer, []ControlMessage{filler}) {
				t.Fatal("failed to saturate replies")
			}
		}
		f.sender.seedAnnouncements(f.credentials[0].Device.Key(), f.now)
		type delivery struct {
			at      time.Duration
			to      int
			message ControlMessage
		}
		var flying []delivery
		bytesSent, frames := 0, 0
		var firstWant, converged time.Duration
		for at := time.Duration(0); at < 45*time.Second; at += time.Millisecond {
			now := f.now.Add(at)
			retained := flying[:0]
			for _, d := range flying {
				if d.at <= at {
					ends[d.to].ingress = append(ends[d.to].ingress, d.message)
				} else {
					retained = append(retained, d)
				}
			}
			flying = retained
			for i := range ends {
				e := &ends[i]
				p := e.peer
				n := e.node
				n.flushReplies(p)
				if len(e.ingress) > 0 {
					out, _, err := p.sync.Receive(e.ingress[0], now, directoryResponseSlots(p))
					if !errors.Is(err, ErrControlBackpressure) {
						if err != nil {
							t.Fatal(err)
						}
						e.ingress = e.ingress[1:]
						if !n.queueReplies(p, out) {
							t.Fatal("receiver lost reply credit")
						}
					}
				}
				out, _, err := p.sync.ContinueCertificates(now, directoryResponseSlots(p))
				if err != nil {
					t.Fatal(err)
				}
				if !n.queueReplies(p, out) {
					t.Fatal("maintenance lost reply credit")
				}
				n.flushAnnouncements(p, now)
				if at < e.free || len(p.control) == 0 {
					continue
				}
				message := <-p.control
				if err := n.writePeerControl(p, message); err != nil {
					t.Fatal(err)
				}
				if len(e.link.messages) == 0 {
					continue
				}
				message = e.link.messages[0]
				e.link.messages = e.link.messages[:0]
				var wire bytes.Buffer
				if err := WriteControl(&wire, message); err != nil {
					t.Fatal(err)
				}
				size := wire.Len()
				decoded, err := ReadControl(&wire)
				if err != nil {
					t.Fatal(err)
				}
				bytesSent += size
				frames++
				e.free = at + time.Duration(size)*time.Second/8000
				flying = append(flying, delivery{e.free + 20*time.Millisecond, 1 - i, decoded})
				if i == 1 && message.Gossip != nil && message.Gossip.Op == "want" && firstWant == 0 {
					firstWant = at
				}
			}
			if len(f.destination.entries) == len(f.records) {
				if len(f.destination.Snapshot(now)) != len(f.records) {
					t.Fatal("converged entries lost trust")
				}
				converged = at
				break
			}
		}
		if converged == 0 {
			t.Fatalf("enabled=%t did not converge under reply pressure", enabled)
		}
		t.Logf("enabled=%t origins=33 writer_slots=1 prior_replies=33 rate_Bps=8000 one_way_ms=20 frames=%d framed_bytes=%d first_WANT_ms=%.1f convergence_ms=%.1f", enabled, frames, bytesSent, float64(firstWant)/float64(time.Millisecond), float64(converged)/float64(time.Millisecond))
	}
}

// Both readers already hold a maximum-sized batch and await actor admission.
// The transport has only one frame slot per direction, and each one-slot writer
// is backed by bulk replies that leave exactly the reserved progress credit.
// Raising the accepted batch above that reserve reproduces a duplex stall:
// neither reader can acknowledge its batch or consume the other's old replies.
func TestManifestHaveBatchSaturatedDuplexProgress(t *testing.T) {
	f := newHaveBatchFixture(t, maxManifestHaves)
	type endpoint struct {
		node  *Node
		peer  *nodePeer
		wire  *ControlMessage
		link  *haveBatchCapture
		wants int
	}
	ends := [2]endpoint{}
	batch := ControlMessage{Kind: "manifest-haves", Haves: make([]byte, maxManifestHaves*manifestDigestSize)}
	for i := 0; i < maxManifestHaves; i++ {
		batch.Haves[i*manifestDigestSize] = byte(i + 1)
	}
	framed := func(message ControlMessage) ControlMessage {
		var wire bytes.Buffer
		if err := WriteControl(&wire, message); err != nil {
			t.Fatal(err)
		}
		decoded, err := ReadControl(&wire)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	for i, s := range []*Synchronizer{f.sender, f.receiver} {
		link := &haveBatchCapture{}
		p := &nodePeer{sync: s, haveBatches: true, control: make(chan ControlMessage, 1), link: link}
		n := &Node{directory: s.directory, cache: s.cache}
		incoming := framed(batch)
		ends[i] = endpoint{node: n, peer: p, wire: &incoming, link: link}
		filler := ControlMessage{Kind: "identity-missing", Principal: testPrincipal(testTenantUUID, "00000000-0000-4000-8000-999999999999")}
		for range 1 + maxDirectoryReplyFrames - directoryResponseReserve {
			if !n.queueReplies(p, []ControlMessage{filler}) {
				t.Fatal("failed to fill bulk budget")
			}
		}
		if directoryResponseSlots(p) != directoryResponseReserve {
			t.Fatal("fixture did not leave exactly progress reserve")
		}
	}
	for step := 0; step < 500; step++ {
		// Finish both actor inputs before allowing either writer to advance.
		for i := range ends {
			e := &ends[i]
			if e.wire != nil {
				out, changed, err := e.peer.sync.Receive(*e.wire, f.now, directoryResponseSlots(e.peer))
				if errors.Is(err, ErrControlBackpressure) {
					continue
				}
				if err != nil || len(changed) != 0 {
					t.Fatal("invalid duplex input", err)
				}
				e.wire = nil
				if !e.node.queueReplies(e.peer, out) {
					t.Fatal("duplex replies lost credit")
				}
			}
		}
		if step == 0 && (ends[0].wire != nil || ends[1].wire != nil) {
			t.Fatal("maximum accepted batch exceeds duplex progress reserve")
		}
		idle := true
		for i := range ends {
			e, other := &ends[i], &ends[1-i]
			out, _, err := e.peer.sync.ContinueCertificates(f.now, directoryResponseSlots(e.peer))
			if err != nil {
				t.Fatal(err)
			}
			if !e.node.queueReplies(e.peer, out) {
				t.Fatal("duplex maintenance lost credit")
			}
			if other.wire == nil && len(e.peer.control) > 0 {
				m := <-e.peer.control
				if err := e.node.writePeerControl(e.peer, m); err != nil {
					t.Fatal(err)
				}
				if len(e.link.messages) != 1 {
					t.Fatal("duplex writer lost a reply")
				}
				decoded := framed(e.link.messages[0])
				e.link.messages = e.link.messages[:0]
				if decoded.Gossip != nil && decoded.Gossip.Op == "want" {
					e.wants++
				}
				other.wire = &decoded
				e.node.flushReplies(e.peer)
			}
			if e.wire != nil || len(e.peer.control) > 0 || len(e.peer.replies) > 0 {
				idle = false
			}
		}
		if idle {
			for _, e := range ends {
				if e.wants != maxManifestHaves || e.peer.sync.gossipPool.PayloadStats().Downloads != 0 {
					t.Fatal("duplex hints or requests were lost", e.wants)
				}
			}
			return
		}
	}
	t.Fatal("saturated duplex carrier did not drain")
}
