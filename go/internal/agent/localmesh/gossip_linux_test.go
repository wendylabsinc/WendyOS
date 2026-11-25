//go:build linux

package localmesh

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func TestGossipReplySchedulerSurvivesSaturatedSlowControlQueue(t *testing.T) {
	now := time.Now()
	source := netgossip.NewPool().NewSession(netgossip.Manifest)
	pool := netgossip.NewPool()
	receiver, alternate := pool.NewSession(netgossip.Manifest), pool.NewSession(netgossip.Manifest)
	body := bytes.Repeat([]byte("x"), 32<<10)
	have := source.Offer(body, now.Add(time.Minute), now)
	want, _, _ := receiver.Receive(*have, now)
	alternate.Receive(*have, now)
	first, _, _ := source.Receive(*want, now)
	n := &Node{}
	p := &nodePeer{control: make(chan ControlMessage, 2)}
	for range cap(p.control) {
		p.control <- ControlMessage{Kind: "gossip", Gossip: have}
	}
	if !n.queueReplies(p, []ControlMessage{{Kind: "gossip", Gossip: first}}) {
		t.Fatal("initial reply lost")
	}
	var received []byte
	for tick := 0; tick < 50; tick++ {
		now = now.Add(time.Second)
		// Model a slow physical writer accepting only one frame per second.
		select {
		case m := <-p.control:
			if m.Gossip.Op == "data" {
				_, data, err := receiver.Receive(*m.Gossip, now)
				if err != nil {
					t.Fatal(err)
				}
				if data != nil {
					received = data
				}
			}
		default:
		}
		n.flushReplies(p)
		var replies []ControlMessage
		for _, m := range source.Poll(now, directoryReplySlots(p)) {
			replies = append(replies, ControlMessage{Kind: "gossip", Gossip: m})
		}
		if !n.queueReplies(p, replies) {
			t.Fatal("normal backpressure overflowed reply reserve", tick)
		}
		if q := alternate.Poll(now); len(q) > 0 {
			t.Fatal("slow progressing transfer triggered duplicate request", tick)
		}
		if received != nil {
			break
		}
	}
	if !bytes.Equal(received, body) {
		t.Fatal("saturated scheduler skipped bytes or failed to complete")
	}
}

func TestGossipDescriptorDataPairsFitExistingReplyReserve(t *testing.T) {
	n := &Node{}
	p := &nodePeer{control: make(chan ControlMessage, 2)}
	for range cap(p.control) {
		p.control <- ControlMessage{Kind: "identity-request", Principal: "already queued"}
	}
	// Bulk scheduling has used its allowance and must preserve all eight
	// response slots. Eight WANT responses can each retain one descriptor; DATA
	// waits for bulk credit after the queue drains.
	for range maxDirectoryReplyFrames - 8 {
		p.replies = append(p.replies, ControlMessage{Kind: "identity-request", Principal: "already queued"})
	}
	if directoryReplySlots(p) != 0 {
		t.Fatal("fixture left bulk scheduling slots")
	}
	for range 8 {
		f := newGossipManifestRenewal(t, false)
		have := f.sender.Record(f.wire, f.now)
		want, _, err := f.receiver.Receive(have[0], f.now)
		if err != nil || len(want) != 1 {
			t.Fatal(want, err)
		}
		if replies, _, err := f.sender.Receive(want[0], f.now, 0); !errors.Is(err, ErrControlBackpressure) || len(replies) != 0 {
			t.Fatal("zero credit consumed the request", replies, err)
		}
		replies, _, err := f.sender.Receive(want[0], f.now, directoryResponseSlots(p))
		if err != nil || len(replies) != 1 || replies[0].Certificate == nil {
			t.Fatal("response credit lost descriptor or advanced bulk DATA", replies, err)
		}
		if !n.queueReplies(p, replies) {
			t.Fatal("ordinary descriptor/DATA responses overflowed reserved queue")
		}
	}
	if len(p.replies) != maxDirectoryReplyFrames || directoryReplySlots(p) != 0 {
		t.Fatal("response reserve was miscounted")
	}
	for i := maxDirectoryReplyFrames - 8; i < len(p.replies); i++ {
		if p.replies[i].Certificate == nil {
			t.Fatal("admitted descriptor order changed")
		}
	}
}
