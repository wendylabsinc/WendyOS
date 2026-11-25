package localmesh

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
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

func TestQueuedExportRechecksTrustWithoutDestroyingBoundedRepairBody(t *testing.T) {
	fixture := newGossipManifestRenewal(t, false)
	d, chain, now := fixture.source, fixture.old.Certificate.Certificate, fixture.now
	original := d.cache.verify
	allowed := true
	d.cache.verify = func(chain [][]byte, at time.Time) (Identity, error) {
		if !allowed {
			return Identity{}, errors.New("current certificate trust unavailable")
		}
		return original(chain, at)
	}
	wire := fixture.wire
	if _, err := d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	pool := netgossip.NewPool()
	syncer := NewSynchronizer(d, d.cache, pool)
	defer syncer.Close()
	queued := syncer.Record(wire, now)[0]
	link := &expiryNodeLink{}
	p := &nodePeer{control: make(chan ControlMessage, 2), sync: syncer, link: link}
	node := &Node{directory: d, controlHub: &ControlHub{announcements: pool}, peers: map[babel.LinkID]*nodePeer{1: p}}
	p.control <- queued
	allowed = false
	node.sweepAnnouncements(now.Add(time.Second))
	if len(p.control) != 0 {
		t.Fatal("untrusted queue export survived idle sweep")
	}
	if len(d.entries) != 1 || pool.PayloadStats().Objects != 1 {
		t.Fatal("temporary key loss destroyed original bounded identity-repair body")
	}
	if err := node.writePeerControl(p, queued); err != nil || link.writes != 0 {
		t.Fatal("selected queued frame bypassed current trust", err)
	}
	allowed = true
	if _, err := d.cache.Put(chain, time.Now()); err != nil {
		t.Fatal(err)
	}
	repaired := syncer.Record(wire, now.Add(2*time.Second))
	if len(repaired) != 1 || !repaired[0].Deadline().Equal(queued.Deadline()) {
		t.Fatal("repair did not preserve original admission deadline")
	}
	if err := node.writePeerControl(p, repaired[0]); err != nil || link.writes != 1 {
		t.Fatal("repaired current export did not resume", err)
	}
}
