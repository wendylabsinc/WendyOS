package localmesh

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func TestGossipSequenceProofAndRoutingUpdateReordering(t *testing.T) {
	for _, order := range []string{"proof first", "update first"} {
		t.Run(order, func(t *testing.T) {
			n := newSignedRouteTestNetwork(t, 3)
			n.connect(0, 1, 1)
			n.connect(0, 2, 2)
			pools := []*netgossip.Pool{netgossip.NewPool(), netgossip.NewPool(), netgossip.NewPool()}
			senders, receivers := make([]*Synchronizer, 3), make([]*Synchronizer, 3)
			for _, i := range []int{1, 2} {
				senders[i] = NewSynchronizer(n.nodes[0].directory, n.nodes[0].directory.cache, pools[0])
				receivers[i] = NewSynchronizer(n.nodes[i].directory, n.nodes[i].directory.cache, pools[i])
				defer senders[i].Close()
				defer receivers[i].Close()
			}
			deliverProof := func(proof SignedManifest) {
				t.Helper()
				n.accept(0, proof)
				for _, i := range []int{1, 2} {
					queue := senders[i].Record(proof, n.now())
					toReceiver := true
					changed := 0
					for len(queue) > 0 {
						var next []ControlMessage
						for _, m := range queue {
							target := senders[i]
							if toReceiver {
								target = receivers[i]
							}
							replies, admitted, err := target.Receive(m, n.now())
							if err != nil {
								t.Fatal(err)
							}
							if toReceiver {
								changed += len(admitted)
							}
							next = append(next, replies...)
						}
						queue, toReceiver = next, !toReceiver
					}
					if changed != 1 {
						t.Fatal("pooled gossip did not authenticate one sequence proof", i, changed)
					}
					n.step(i, babel.Tick{})
				}
			}
			deliverProof(n.sign(1, time.Minute, false))
			n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
			n.advance(2 * time.Second)
			n.assertForwarded(1, true, 0)
			n.assertForwarded(2, true, 0)
			// Public RFC 8966 Seqno Request encoding, independent of #2377's helper.
			packet := make([]byte, 36)
			copy(packet, []byte{42, 2, 0, 32, 10, 30, 2, 128})
			binary.BigEndian.PutUint16(packet[8:10], 1)
			packet[10] = 64
			binary.BigEndian.PutUint64(packet[12:20], uint64(n.nodes[0].device.routerID))
			address := n.nodes[0].device.prefix.Addr().As16()
			copy(packet[20:], address[:])
			n.step(0, babel.Receive{ID: 1, Packet: packet})
			if n.nodes[0].routing.Snapshot().Seqno != 1 {
				t.Fatal("request did not advance the sequence")
			}
			if order == "update first" {
				n.deliver()
			}
			deliverProof(n.sign(2, time.Minute, false))
			if order == "proof first" {
				n.deliver()
			}
			n.advance(5 * time.Second)
			n.assertForwarded(1, true, 1)
			n.assertForwarded(2, true, 1)
			if n.nodes[0].routing.Snapshot().Seqno != 1 {
				t.Fatal("gossip reordering caused sequence churn")
			}
		})
	}
}
