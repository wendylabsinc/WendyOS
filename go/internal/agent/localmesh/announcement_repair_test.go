package localmesh

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func TestManifestHaveSurvivesTemporaryIdentityLoss(t *testing.T) {
	for _, stage := range []string{"pending", "queue", "prune", "writer", "superseded", "expired"} {
		t.Run(stage, func(t *testing.T) {
			origin, other := testCredentials(t)
			now := time.Now()
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), origin.Verify, now)
			if err != nil {
				t.Fatal(err)
			}
			d, err := NewDirectory(origin.Device.Tenant(), 2, cache)
			if err != nil {
				t.Fatal(err)
			}
			admit := func(creds *Credentials, rev uint64) SignedManifest {
				t.Helper()
				chain := creds.Certificate.Certificate
				if _, err := cache.Put(chain, now); err != nil {
					t.Fatal(err)
				}
				m := routeManifest(creds.Device, now)
				m.Revision = rev
				w, err := SignManifest(m, chain, creds.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := d.Accept(w, now); err != nil || !changed {
					t.Fatalf("admit revision %d: changed=%t err=%v", rev, changed, err)
				}
				return w
			}
			w, later := admit(origin, 1), admit(other, 1)
			link := &expiryNodeLink{}
			p := &nodePeer{control: make(chan ControlMessage, 1), link: link, sync: NewSynchronizer(d, cache, netgossip.NewPool())}
			defer p.sync.Close()
			n := &Node{directory: d, peers: map[babel.LinkID]*nodePeer{1: p}}
			p.sync.markAnnouncement(w, now)
			var have []ControlMessage
			if stage != "pending" {
				have = p.sync.drainAnnouncements(now, 1)
				if len(have) != 1 {
					t.Fatal("expected original HAVE")
				}
				if stage == "prune" {
					n.queue(p, have)
				}
			}

			var replacement SignedManifest
			if stage == "superseded" {
				replacement = admit(origin, 2)
			}
			cache.mu.Lock()
			delete(cache.entries, origin.Device.Principal())
			cache.mu.Unlock()

			switch stage {
			case "pending":
				p.sync.markAnnouncement(later, now)
				n.flushAnnouncements(p, now)
				if len(p.control) != 1 || (<-p.control).origin != later.ownerKey() {
					t.Fatal("temporarily unverifiable origin blocked a healthy HAVE")
				}
			case "queue":
				n.queue(p, have)
			case "writer", "expired", "superseded":
				if err := n.writePeerControl(p, have[0]); err != nil || link.writes != 0 {
					t.Fatal("unverifiable HAVE reached the transport", err)
				}
				if stage == "superseded" {
					p.sync.markAnnouncement(replacement, now)
					if !have[0].Expired(now) {
						t.Fatal("unverifiable replacement did not retire superseded HAVE")
					}
					w = replacement
				}
			}
			if stage == "expired" {
				now = now.Add(time.Minute)
			}
			// Delay collecting the superseded write notification until
			// after the replacement has already been queued.
			if stage != "superseded" {
				n.sweepAnnouncements(now)
			}
			if len(p.control) != 0 {
				t.Fatal("unverifiable HAVE survived the queue")
			}
			if _, err := cache.Put(origin.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}

			n.flushAnnouncements(p, now)
			if stage == "expired" {
				if len(p.control) != 0 || len(p.sync.pendingAnnouncements) != 0 {
					t.Fatal("expired HAVE retried after identity repair")
				}
				return
			}
			if len(p.control) != 1 {
				t.Fatal("repaired identity did not release the pending HAVE")
			}
			got := <-p.control
			if got.Gossip == nil || got.Gossip.ID() != manifestGossipID(w) {
				t.Fatal("repair announced the wrong revision")
			}
			n.sweepAnnouncements(now)
			n.flushAnnouncements(p, now)
			if len(p.control) != 0 {
				t.Fatal("retry duplicated a HAVE")
			}
		})
	}
}

func TestCertificateBatchRelaysCommittedManifestBeforeClosingCarrier(t *testing.T) {
	for _, flow := range []string{"ingress", "maintenance"} {
		t.Run(flow, func(t *testing.T) {
			first, second := testCredentials(t)
			self := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceCID))
			n := newActorTestNode(t, self, &handoffActorNetwork{})
			t.Cleanup(func() { n.cancel(); _ = n.Stop() })
			if err := n.step(babel.Originate{Prefix: netip.PrefixFrom(self.Device.MeshAddress(), 128)}); err != nil {
				t.Fatal(err)
			}
			n.directory.limit = 2 // One slot remains after the node's own proof.
			now := time.Now()
			sourceLink := &handoffActorLink{}
			newCarrier := func(id babel.LinkID, creds *Credentials, link nodeLink) *nodePeer {
				s := NewSynchronizer(n.directory, n.cache, n.controlHub.Announcements())
				t.Cleanup(s.Close)
				p := &nodePeer{id: id, device: creds.Device, link: link, sync: s, control: make(chan ControlMessage, 4)}
				n.peers[id] = p
				return p
			}
			source := newCarrier(1, first, sourceLink)
			destination := newCarrier(2, second, &handoffActorLink{})
			var committed SignedManifest
			for _, creds := range []*Credentials{first, second} {
				chain := creds.Certificate.Certificate
				if _, err := n.cache.Put(chain, now); err != nil {
					t.Fatal(err)
				}
				m := routeManifest(creds.Device, now)
				w, err := SignManifest(m, chain, creds.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				source.sync.retainPending(w, now)
				desc := DescribeCertificates(w.Principal, chain)
				source.sync.certificates.pending[w.Principal] = pendingCertificateChain{chain: *desc.Chain, expires: now.Add(time.Minute)}
				if creds == first {
					committed = w
				}
			}
			var err error
			if flow == "ingress" {
				err = n.handleControl(source, &ControlMessage{Kind: "certificate", Certificate: DescribeCertificates(first.Device.Principal(), first.Certificate.Certificate)})
			} else {
				err = n.maintainDirectory(source)
			}
			if err != nil || !sourceLink.closed {
				t.Fatal("source carrier did not close after the second admission failed", err)
			}
			if _, ok := n.directory.RecordDeadline(committed, time.Now()); !ok || len(n.directory.Records(time.Now())) != 2 {
				t.Fatal("fixture did not commit the first manifest only")
			}
			if len(source.control) != 0 || len(destination.control) != 1 {
				t.Fatal("committed admission was not relayed exactly once")
			}
			if got := <-destination.control; got.Gossip == nil || got.Gossip.ID() != manifestGossipID(committed) {
				t.Fatal("relayed the wrong manifest")
			}
		})
	}
}
