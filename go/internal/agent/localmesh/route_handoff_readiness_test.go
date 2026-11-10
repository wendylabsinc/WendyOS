package localmesh

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Exercise the real actor/FIB barrier and the immutable readiness snapshot
// together. The memory carrier models an authenticated cheaper TCP link; no
// radio, socket or installed host route is involved.
func assertHandoffCarrierReadiness(t *testing.T, f *handoffActorFixture, want bool) {
	t.Helper()
	n := f.nodes[1]
	peer := f.nodes[0].Credentials.Device
	view := n.Snapshot()
	found := false
	for _, link := range view.Links {
		if link.ID == 1 && link.Device == peer {
			found = true
			if link.ForwardingReady != want {
				t.Fatalf("t=%v forwarding ready=%v want=%v, routes=%+v proofs=%+v", f.elapsed, link.ForwardingReady, want, view.Routes, view.RouteProofs)
			}
		}
	}
	if want && !found {
		t.Fatal("authenticated cheap link missing")
	}
	if got := n.HasCheaperLink(peer, 4096); got != want {
		t.Fatalf("t=%v cheaper-link retirement evidence=%v want=%v", f.elapsed, got, want)
	}
	for origin := range view.RouteProofs {
		if !view.RouteAuthorizations[origin] {
			t.Fatal("public proof and readiness authorization projections differ")
		}
	}
}

func TestNodeHandoffKeepsInstalledLANForwardingReadiness(t *testing.T) {
	for _, order := range []string{"proof-before-update", "update-before-proof"} {
		t.Run(order, func(t *testing.T) {
			f := newHandoffActorFixture(t)
			f.nodes[1].peers[1].transport = "tcp"
			f.nodes[1].refresh()
			assertHandoffCarrierReadiness(t, f, true)
			var proof SignedManifest
			if order == "update-before-proof" {
				proof = f.lateProof()
			} else {
				origin := f.nodes[0]
				origin.nextPublication = time.Time{}
				f.step(0, babel.Receive{ID: 1, Packet: handoffActorSequenceRequest(netip.PrefixFrom(origin.Credentials.Device.MeshAddress(), 128), babel.RouterID(origin.state.RouterID), 1)})
				proof = f.ownProof(0)
			}
			f.proof(1, proof)
			f.assertForwarded(true, 0)
			assertHandoffCarrierReadiness(t, f, true)
			old := RouteOrigin{Prefix: netip.PrefixFrom(f.nodes[0].Credentials.Device.MeshAddress(), 128), RouterID: babel.RouterID(f.nodes[0].state.RouterID)}
			retained := f.nodes[1].Snapshot().RouteProofs[old]
			if !retained.Retained || retained.Deadline.IsZero() {
				t.Fatal("forwarding readiness lost original detached proof")
			}
			f.step(1, babel.Tick{}) // No response/new candidate has been delivered.
			assertHandoffCarrierReadiness(t, f, true)
			f.request() // Real RFC prefix request/finite response promotes sequence1.
			f.assertForwarded(true, 1)
			assertHandoffCarrierReadiness(t, f, true)
			if _, held := f.nodes[1].Snapshot().RouteProofs[old]; held {
				t.Fatal("old readiness authority survived committed replacement")
			}
			if len(f.networks[1].interrupted) != 0 {
				t.Fatal("normal handoff published an interrupted applied FIB")
			}
		})
	}
}

func TestNodeHandoffReadinessCannotOutliveOriginalAuthority(t *testing.T) {
	for _, revoke := range []string{"old-deadline", "signed-withdrawal", "peer-loss"} {
		t.Run(revoke, func(t *testing.T) {
			f := newHandoffActorFixture(t)
			f.nodes[1].peers[1].transport = "tcp"
			f.nodes[1].refresh()
			origin := f.nodes[0]
			old, err := DecodeManifest(f.ownProof(0).Body)
			if err != nil {
				t.Fatal(err)
			}
			if revoke == "old-deadline" {
				old.Revision++
				old.Issued = f.now().UnixMilli()
				old.Expires = old.Issued + (2 * time.Second).Milliseconds()
				wire, err := SignManifest(old, origin.Credentials.Certificate.Certificate, origin.Credentials.Signer, f.now())
				if err != nil {
					t.Fatal(err)
				}
				f.proof(1, wire)
			}
			origin.nextPublication = time.Time{}
			f.step(0, babel.Receive{ID: 1, Packet: handoffActorSequenceRequest(netip.PrefixFrom(origin.Credentials.Device.MeshAddress(), 128), babel.RouterID(origin.state.RouterID), 1)})
			newManifest, err := DecodeManifest(f.ownProof(0).Body)
			if err != nil {
				t.Fatal(err)
			}
			newManifest.Revision = old.Revision + 1
			wire, err := SignManifest(newManifest, origin.Credentials.Certificate.Certificate, origin.Credentials.Signer, f.now())
			if err != nil {
				t.Fatal(err)
			}
			f.proof(1, wire)
			f.assertForwarded(true, 0)
			assertHandoffCarrierReadiness(t, f, true)
			switch revoke {
			case "old-deadline":
				var deadline time.Time
				for _, held := range f.nodes[1].directory.handoffs {
					deadline = held.deadline
				}
				if deadline.IsZero() {
					t.Fatal("missing original monotonic deadline")
				}
				f.elapsed = deadline.Sub(f.epoch) + time.Millisecond
				f.step(1, babel.Tick{}) // New announcement remains live; no new path arrives.
				if !f.nodes[1].Snapshot().RouteAuthorizations[*newManifest.Route] {
					t.Fatal("test lost current authority too")
				}
			case "signed-withdrawal":
				newManifest.Revision++
				newManifest.Withdraw = true
				wire, err := SignManifest(newManifest, origin.Credentials.Certificate.Certificate, origin.Credentials.Signer, f.now())
				if err != nil {
					t.Fatal(err)
				}
				f.proof(1, wire)
			case "peer-loss":
				if err := f.nodes[1].handle(nodeEvent{id: 1, remove: true}); err != nil {
					t.Fatal(err)
				}
			}
			f.assertForwarded(false, 0)
			assertHandoffCarrierReadiness(t, f, false)
		})
	}
}
