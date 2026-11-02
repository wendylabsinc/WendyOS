package localmesh

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

type expiryNodeLink struct{ writes int }

func (*expiryNodeLink) Close() error                                    { return nil }
func (*expiryNodeLink) SendDatagram([]byte) error                       { return nil }
func (*expiryNodeLink) ReceiveDatagram(context.Context) ([]byte, error) { return nil, io.EOF }
func (l *expiryNodeLink) WriteControl(ControlMessage) error             { l.writes++; return nil }
func (*expiryNodeLink) ReadControl() (ControlMessage, error)            { return ControlMessage{}, io.EOF }

func TestNodeIdleSweepReleasesIndependentQueuedPayloadCopies(t *testing.T) {
	now := time.Now()
	link := &expiryNodeLink{}
	p := &nodePeer{control: make(chan ControlMessage, 4), link: link}
	node := &Node{peers: map[babel.LinkID]*nodePeer{1: p}}
	for cycle := range 150 {
		at := now.Add(time.Duration(cycle) * time.Second)
		expired := ControlMessage{Kind: "manifest", Manifest: &SignedManifest{Body: make([]byte, 4096), Signature: make([]byte, 128)}, deadline: at.Add(time.Second)}
		live := ControlMessage{Kind: "identity-missing", Principal: "retained"}
		p.control <- expired
		p.control <- live
		p.replies = []ControlMessage{expired, expired}
		backing := p.replies
		node.sweepAnnouncements(at.Add(time.Second))
		if len(p.replies) != 0 || p.replyBytes != 0 || len(p.control) != 1 {
			t.Fatal("idle sweep kept actor payload copies", len(p.replies), p.replyBytes, len(p.control))
		}
		for _, old := range backing {
			if old.Manifest != nil || old.Gossip != nil || old.Bundle != nil {
				t.Fatal("reply backing array retained expired allocation")
			}
		}
		if kept := <-p.control; kept.Principal != "retained" {
			t.Fatal("sweep lost unrelated live control")
		}
		// An already-selected write is checked at the final adapter boundary.
		if err := node.writePeerControl(p, ControlMessage{Kind: "manifest", Manifest: expired.Manifest, deadline: time.Now().Add(-time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if link.writes != 0 {
		t.Fatal("expired queue body reached transport")
	}
}
