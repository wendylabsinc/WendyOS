package localmesh

import (
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

// Exercise both independent delivery orders using the actual pooled router
// protocol. All durations below advance the simulated Babel clock, not a
// machine throughput deadline. A four-second update interval separates the
// triggered sequence fanout from ordinary periodic repair.
func TestGossipSequenceFanoutBothProofUpdateOrders(t *testing.T) {
	for _, order := range []string{"proof first", "update first"} {
		t.Run(order, func(t *testing.T) {
			n := newSignedRouteTestNetwork(t, 3, func(config *babel.Config) { config.UpdateInterval = 4 * time.Second })
			n.connect(0, 1, 1)
			n.connect(0, 2, 2)
			pool := netgossip.NewPool()
			senders, receivers := make([]*Synchronizer, 3), make([]*Synchronizer, 3)
			for _, i := range []int{1, 2} {
				senders[i] = NewSynchronizer(n.nodes[0].directory, n.nodes[0].directory.cache, pool)
				receivers[i] = NewSynchronizer(n.nodes[i].directory, n.nodes[i].directory.cache, netgossip.NewPool())
				defer senders[i].Close()
				defer receivers[i].Close()
			}
			deliverProof := func(proof SignedManifest) {
				t.Helper()
				n.accept(0, proof)
				for _, i := range []int{1, 2} {
					messages := senders[i].Record(proof, n.now())
					if len(messages) != 1 || messages[0].Gossip == nil || messages[0].Gossip.Op != "have" {
						t.Fatal("router publication bypassed HAVE/WANT", i)
					}
					// The remote Directory is updated only through framed HAVE/WANT/DATA
					// exchange and independent signature verification, never direct Accept.
					changed := exchangeDirectoryMessages(t, senders[i], receivers[i], messages, n.now())
					if len(changed) != 1 {
						t.Fatal("pooled gossip did not authenticate one sequence proof", i, len(changed))
					}
					n.step(i, babel.Tick{})
				}
			}
			deliverProof(n.sign(1, time.Minute, false))
			n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
			n.advance(2 * time.Second)
			n.assertForwarded(1, true, 0)
			n.assertForwarded(2, true, 0)
			n.step(0, babel.Receive{ID: 1, Packet: routeSequenceRequest(n.nodes[0].device.prefix, n.nodes[0].device.routerID, 1)})
			if n.nodes[0].routing.Snapshot().Seqno != 1 {
				t.Fatal("request did not advance the sequence")
			}
			if order == "update first" {
				rejected := []uint64{0, n.nodes[1].routing.Snapshot().Counters.Rejected, n.nodes[2].routing.Snapshot().Counters.Rejected}
				n.advance(2200 * time.Millisecond)
				for _, i := range []int{1, 2} {
					if n.nodes[i].routing.Snapshot().Counters.Rejected <= rejected[i] {
						t.Fatal("sequence fanout did not reach peer before proof", i)
					}
					n.assertForwarded(i, true, 0)
				}
			}
			deliverProof(n.sign(2, time.Minute, false))
			if order == "proof first" {
				// Both requester and passive peer must recover through triggered fanout
				// before the ordinary four-second origin update could repair the route.
				n.advance(2200 * time.Millisecond)
				n.assertForwarded(1, true, 1)
				n.assertForwarded(2, true, 1)
			}
			// Updates rejected before their proof are repaired by the normal periodic
			// update. Neither ordering may provoke another origin sequence or request.
			n.advance(8 * time.Second)
			if n.nodes[0].routing.Snapshot().Seqno != 1 {
				t.Fatal("gossip reordering caused sequence churn")
			}
			for _, i := range []int{1, 2} {
				n.assertForwarded(i, true, 1)
				if n.nodes[i].routing.Snapshot().PendingRequests != 0 {
					t.Fatal("peer retained a sequence request", i)
				}
			}
		})
	}
}
