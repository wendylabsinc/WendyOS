package localmesh

import (
	"bytes"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func TestProcessedResignReleasesGossipClaim(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	c := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), c.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(c.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(c.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	m := routeManifest(c.Device, now)
	wire, err := SignManifest(m, c.Certificate.Certificate, c.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	pool := netgossip.NewPool()
	receiver := NewSynchronizer(d, cache, pool)
	defer receiver.Close()
	source := netgossip.NewPool().NewSession(netgossip.Manifest)
	defer source.Close()

	original := receiver.Record(wire, now)
	if len(original) != 1 {
		t.Fatal("missing original export")
	}
	deadline, ok := d.RecordDeadline(wire, now)
	if !ok {
		t.Fatal("missing original deadline")
	}
	watermark := d.history[wire.ownerKey()]
	originalBytes, err := netwire.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}

	// ECDSA produces distinct valid envelopes for one immutable signed body.
	// Four completed duplicates previously exhausted this link's fetch budget.
	for i := range 4 {
		at := now.Add(time.Duration(i+1) * 10 * time.Second)
		other, err := SignManifest(m, c.Certificate.Certificate, c.Signer, now)
		if err != nil || bytes.Equal(other.Signature, wire.Signature) {
			t.Fatal("need alternative valid ECDSA envelope", err)
		}
		payload, err := netwire.Marshal(other)
		if err != nil {
			t.Fatal(err)
		}
		have := source.Offer(payload, at.Add(time.Minute), at)
		wants, _, err := receiver.Receive(ControlMessage{Kind: "gossip", Gossip: have}, at)
		if err != nil || len(wants) != 1 {
			t.Fatalf("resign %d request: %v %v", i, wants, err)
		}
		changed := deliverClaimTestData(t, source, receiver, wants[0].Gossip, at)
		if len(changed) != 0 {
			t.Fatal("same-body resign changed the directory")
		}
		if got, ok := d.RecordDeadline(wire, at); !ok || got != deadline || d.history[wire.ownerKey()] != watermark {
			t.Fatal("same-body resign renewed lease or replay watermark")
		}
		if len(receiver.gossip.Lookup(have.ID(), at)) != 0 {
			t.Fatal("noncurrent envelope gained export authority")
		}
		if original[0].Expired(at) || !bytes.Equal(receiver.gossip.Lookup(original[0].Gossip.ID(), at), originalBytes) {
			t.Fatal("discarding duplicate removed the current artifact")
		}
	}
	got := pool.PayloadStats()
	m.Revision++
	newer, err := SignManifest(m, c.Certificate.Certificate, c.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	newPayload, err := netwire.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}
	at := now.Add(45 * time.Second)
	newHave := source.Offer(newPayload, at.Add(time.Minute), at)
	newWants, _, err := receiver.Receive(ControlMessage{Kind: "gossip", Gossip: newHave}, at)
	if err != nil {
		t.Fatal(err)
	}
	if got.Downloads != 0 || got.Objects != 1 || len(newWants) != 1 {
		t.Fatalf("processed signatures blocked a newer revision: stats=%+v newer_wants=%d", got, len(newWants))
	}
	changed := deliverClaimTestData(t, source, receiver, newWants[0].Gossip, at)
	if len(changed) != 1 || !sameManifestWire(changed[0], newer) || len(d.Snapshot(at)) != 1 || d.Snapshot(at)[0].Revision != m.Revision {
		t.Fatal("genuinely newer revision was not admitted")
	}
	if pool.PayloadStats().Downloads != 0 || !original[0].Expired(at) {
		t.Fatal("new revision retained completed claim or old export")
	}
}

func deliverClaimTestData(t *testing.T, source *netgossip.Session, receiver *Synchronizer, want *netgossip.Message, now time.Time) []SignedManifest {
	t.Helper()
	first, _, err := source.Receive(*want, now)
	if err != nil || first == nil {
		t.Fatal("source failed", err)
	}
	frames := append([]*netgossip.Message{first}, source.Poll(now)...)
	var changed []SignedManifest
	for _, frame := range frames {
		_, next, err := receiver.Receive(ControlMessage{Kind: "gossip", Gossip: frame}, now)
		if err != nil {
			t.Fatal("download admission failed", err)
		}
		changed = append(changed, next...)
	}
	return changed
}
