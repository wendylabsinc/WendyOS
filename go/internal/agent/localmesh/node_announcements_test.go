package localmesh

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func TestPendingManifestAnnouncementsArePerCarrier(t *testing.T) {
	origin, quiet := testCredentials(t)
	now := time.Now()
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), origin.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(origin.Device.Tenant(), 2, cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, creds := range []*Credentials{origin, quiet} {
		if _, err := cache.Put(creds.Certificate.Certificate, now); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(creds *Credentials, revision uint64) (SignedManifest, netgossip.ID) {
		t.Helper()
		manifest := routeManifest(creds.Device, now)
		manifest.Revision = revision
		w, err := SignManifest(manifest, creds.Certificate.Certificate, creds.Signer, now)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := d.Accept(w, now); err != nil || !changed {
			t.Fatalf("admission of revision %d: changed=%t, err=%v", revision, changed, err)
		}
		encoded, err := netwire.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		return w, netgossip.Identify(netgossip.Manifest, encoded)
	}
	_, first := admit(origin, 1)
	_, unchanged := admit(quiet, 1)
	pop := func(p *nodePeer) ControlMessage {
		t.Helper()
		select {
		case message := <-p.control:
			return message
		default:
			t.Fatal("expected a queued control message")
			return ControlMessage{}
		}
	}
	expectHave := func(message ControlMessage, id netgossip.ID) {
		t.Helper()
		if message.Gossip == nil || message.Gossip.Op != "have" || message.Gossip.ID() != id {
			t.Fatalf("expected HAVE %v, got %#v", id, message.Gossip)
		}
	}
	n := &Node{directory: d}
	pool := netgossip.NewPool()
	newCarrier := func() *nodePeer {
		p := &nodePeer{sync: NewSynchronizer(d, cache, pool), control: make(chan ControlMessage, 1)}
		t.Cleanup(p.sync.Close)
		p.sync.seedAnnouncements(origin.Device.Key(), now)
		for _, id := range []netgossip.ID{first, unchanged} {
			n.flushAnnouncements(p, now)
			expectHave(pop(p), id)
		}
		return p
	}
	slow, fast := newCarrier(), newCarrier()
	w, second := admit(origin, 2)
	slow.sync.markAnnouncement(w, now)
	n.flushAnnouncements(slow, now)
	if !n.queueReplies(slow, []ControlMessage{{Kind: "identity-request", Principal: origin.Device.Principal()}}) || len(slow.replies) != 1 {
		t.Fatal("expected a priority reply blocked behind an already queued HAVE")
	}
	var latest netgossip.ID
	for _, revision := range []uint64{3, 4} {
		w, latest = admit(origin, revision)
		for _, p := range []*nodePeer{slow, fast} {
			p.sync.markAnnouncement(w, now)
			n.flushAnnouncements(p, now)
		}
		expectHave(pop(fast), latest)
	}
	if pending := slow.sync.pendingAnnouncements; len(pending) != 1 || pending[0] != origin.Device.Key() {
		t.Fatalf("blocked carrier pending origins = %v", pending)
	}
	stale := pop(slow)
	expectHave(stale, second)
	if n.controlEligible(slow, stale, now) {
		t.Fatal("superseded HAVE was still eligible to write")
	}
	n.flushAnnouncements(slow, now)
	if reply := pop(slow); reply.Kind != "identity-request" {
		t.Fatalf("pending HAVE overtook priority reply: %s", reply.Kind)
	}
	n.flushAnnouncements(slow, now)
	expectHave(pop(slow), latest)
	for _, p := range []*nodePeer{slow, fast} {
		n.flushAnnouncements(p, now)
		if len(p.sync.pendingAnnouncements) != 0 || len(p.control) != 0 {
			t.Fatal("carrier repeated an already announced origin")
		}
	}
}
