package localmesh

import (
	"bytes"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func TestManifestExportUsesAdmittedDurationAndExpungesIdle(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	pool := netgossip.NewPool()
	sender := NewSynchronizer(d, d.cache, pool)
	defer sender.Close()
	direct := NewSynchronizer(d, d.cache)
	defer direct.Close()
	for i := range 150 {
		at := now.Add(time.Duration(i) * time.Second)
		m := routeManifest(device, now.Add(-10*time.Minute))
		m.Revision = uint64(i + 1)
		m.Expires = time.UnixMilli(m.Issued).Add(time.Second).UnixMilli()
		w, err := SignManifest(m, chain, key, at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Accept(w, at); err != nil {
			t.Fatal(err)
		}
		out := sender.Record(w, at)
		legacy := direct.Record(w, at)
		if len(out) != 1 || len(legacy) == 0 || out[0].Deadline() != at.Add(time.Second) || legacy[len(legacy)-1].Deadline() != at.Add(time.Second) {
			t.Fatal("export used signed UTC expiry instead of admitted duration", out, legacy)
		}
		if !bytes.Equal(legacy[len(legacy)-1].Manifest.Body, w.Body) {
			t.Fatal("receiver deadline rewrote immutable signed bytes")
		}
		if again := sender.Record(w, at.Add(500*time.Millisecond)); len(again) != 1 || again[0].Deadline() != out[0].Deadline() || again[0].canceled != out[0].canceled {
			t.Fatal("repeat export renewed queue ownership")
		}
		d.Sweep(at.Add(time.Second))
		sender.Sweep(at.Add(time.Second))
		direct.Sweep(at.Add(time.Second))
		stats := pool.PayloadStats()
		if stats.Objects != 0 || stats.Uploads != 0 || stats.Downloads != 0 || stats.ObjectBytes != 0 || len(sender.artifacts) != 0 || len(d.entries) != 0 {
			t.Fatal("idle expiration retained full artifact holders", stats)
		}
		if !out[0].Expired(at.Add(time.Second)) || !legacy[len(legacy)-1].Expired(at.Add(time.Second)) {
			t.Fatal("queued signed payload survived its original deadline")
		}
	}
}

func TestSupersessionCancelsCrossSessionQueuedPayloads(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	pool := netgossip.NewPool()
	first := NewSynchronizer(d, d.cache, pool)
	second := NewSynchronizer(d, d.cache, pool)
	direct := NewSynchronizer(d, d.cache)
	defer first.Close()
	defer second.Close()
	defer direct.Close()
	w, _ := SignManifest(routeManifest(device, now), chain, key, now)
	if _, err := d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	a, b, c := first.Record(w, now)[0], second.Record(w, now)[0], direct.Record(w, now)
	want := *a.Gossip
	want.Op, want.Request = "want", 1
	queued, _, err := first.Receive(ControlMessage{Kind: "gossip", Gossip: &want}, now, 16)
	if err != nil {
		t.Fatal(err)
	}
	m := routeManifest(device, now)
	m.Revision++
	m.Withdraw = true
	next, _ := SignManifest(m, chain, key, now)
	if _, err = d.Accept(next, now); err != nil {
		t.Fatal(err)
	}
	// Node's changed-record fanout reaches every peer, including idle links.
	for _, s := range []*Synchronizer{first, second, direct} {
		s.Record(next, now)
	}
	for _, old := range append(append([]ControlMessage{a, b}, c...), queued...) {
		if !old.Expired(now) {
			t.Fatal("superseded queued payload retained a live marker", old.Kind)
		}
	}
	if data := first.gossip.Lookup(a.Gossip.ID(), now); len(data) != 0 {
		t.Fatal("superseded artifact still present in shared gossip pool")
	}
	stats := pool.PayloadStats()
	if stats.Objects != 1 || stats.Uploads != 0 {
		t.Fatal("superseded pool body/upload retained", stats)
	}
}

func TestPendingManifestIdleExpiryCancelsUnfinishedCertificateFetch(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, d, cache := f.receiver(t)
	requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil || len(requests) == 0 {
		t.Fatal(requests, err)
	}
	m := routeManifest(f.renewed.Device, f.now.Add(-5*time.Minute))
	m.Revision = 2
	m.Expires = time.UnixMilli(m.Issued).Add(10 * time.Second).UnixMilli()
	w, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
		t.Fatal(err)
	}
	deadline := receiver.pendingDeadlines[w.ownerKey()]
	if deadline != f.now.Add(10*time.Second) {
		t.Fatal("pending resource deadline used signed absolute expiry")
	}
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now.Add(9*time.Second)); err != nil || receiver.pendingDeadlines[w.ownerKey()] != deadline {
		t.Fatal("duplicate renewed pending ownership", err)
	}
	// Retain a partial public certificate to prove idle cleanup releases the
	// owned assembly, rather than merely dropping the manifest map entry.
	q := receiver.certificates.waiting
	if q == nil {
		t.Fatal("fixture did not claim a certificate")
	}
	cache.parts.mu.Lock()
	claim := cache.parts.pending[q.Hash]
	claim.data, claim.total = []byte("partial certificate"), 1024
	cache.parts.bytes += claim.total
	cache.parts.mu.Unlock()
	receiver.Sweep(deadline)
	if len(receiver.pending) != 0 || len(receiver.pendingDeadlines) != 0 || len(receiver.pendingRenewals) != 0 || receiver.certificates.Pending() || receiver.certificates.waiting != nil || len(d.entries) != 0 {
		t.Fatal("idle deadline retained manifest or certificate continuation")
	}
	cache.parts.mu.Lock()
	retained := len(cache.parts.pending)
	cache.parts.mu.Unlock()
	if retained != 0 || claim.data != nil {
		t.Fatal("idle deadline retained partial certificate bytes")
	}
}

func TestControlQueueMetadataNeverChangesWire(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, _ := SignManifest(routeManifest(device, now), chain, key, now)
	d.Accept(w, now)
	s := NewSynchronizer(d, d.cache)
	defer s.Close()
	for _, m := range s.Record(w, now) {
		var a, b bytes.Buffer
		if err := WriteControl(&a, m); err != nil {
			t.Fatal(err)
		}
		m.deadline, m.canceled = time.Time{}, nil
		if err := WriteControl(&b, m); err != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
			t.Fatal("local queue metadata changed wire", err)
		}
		decoded, err := ReadControl(&a)
		if err != nil || !decoded.Deadline().IsZero() || decoded.canceled != nil {
			t.Fatal("wire supplied local queue authority", err)
		}
		if _, err = netwire.Marshal(decoded); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPendingCertificateRepairKeepsOriginalGossipClaimDeadline(t *testing.T) {
	f := newWarmRenewalFixture(t)
	unused, d, cache := f.receiver(t)
	unused.Close()
	receiver := NewSynchronizer(d, cache, netgossip.NewPool())
	defer receiver.Close()
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	w := f.wire(t, f.now)
	body, _ := netwire.Marshal(w)
	source := netgossip.NewPool().NewSession(netgossip.Manifest)
	defer source.Close()
	have := source.Offer(body, f.now.Add(MaxLease), f.now)
	wants, _, err := receiver.Receive(ControlMessage{Kind: "gossip", Gossip: have}, f.now)
	if err != nil || len(wants) != 1 {
		t.Fatal(wants, err)
	}
	until := wants[0].Gossip.Deadline()
	first, _, err := source.Receive(*wants[0].Gossip, f.now)
	if err != nil {
		t.Fatal(err)
	}
	frames := append([]*netgossip.Message{first}, source.Poll(f.now)...)
	for _, frame := range frames {
		if _, _, err := receiver.Receive(ControlMessage{Kind: "gossip", Gossip: frame}, f.now.Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if receiver.pendingDeadlines[w.ownerKey()] != until || receiver.pendingRenewals[w.ownerKey()].expires != until {
		t.Fatal("moving downloaded bytes into certificate repair restarted its resource allowance")
	}
	receiver.Sweep(until)
	if len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 || len(receiver.pendingDeadlines) != 0 || receiver.certificates.Pending() {
		t.Fatal("original claim deadline retained a certificate continuation")
	}
}
