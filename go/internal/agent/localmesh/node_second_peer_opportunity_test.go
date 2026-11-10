package localmesh

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// This fixture begins at the authenticated transport's Node admission boundary.
// Separate socket/TLS fixtures cover the handshake and provider retry boundary;
// here the actual Node/Babel/FIB clock exercises the existing five-second
// carrier retry floor without introducing a localmesh production timer.
func TestNodeDistinctPeerRetryBeatsUnrelatedSecondStandby(t *testing.T) {
	for _, cost := range []uint16{64, 128} {
		t.Run(strconv.Itoa(int(cost)), func(t *testing.T) {
			f := newCarrierTransitFixture(t)
			f.releaseLANTransit()
			n := f.nodes[0]
			victim := n.peers[1]
			competitor := f.third.Credentials.Device
			if victim.device == competitor || !n.Snapshot().HasOtherForwardingLink(competitor, 0) || !n.Snapshot().HasOtherForwardingLink(victim.device, victim.id) {
				t.Fatal("actual signed Node/Babel fixture lacks two distinct forwarding peers")
			}
			// Preserve the real C link3. The other slots represent unrelated
			// admitted peers, as in the normal Node-capacity fixture.
			for id := babel.LinkID(4); len(n.peers) < 64; id++ {
				n.peers[id] = &nodePeer{id: id, device: selectionDevice(10000 + int(id)), cost: 128, retire: make(chan struct{}), link: &handoffActorLink{}}
				n.nextLink = id
			}
			n.refresh()
			unique := nodeCapacityHoldCandidate(selectionDevice(999), 128)
			rejectedAt := f.elapsed
			if err := n.handle(nodeEvent{add: unique}); !errors.Is(err, ErrCarrierCapacityRetry) || !victim.retiring() || len(n.peers) != 64 {
				t.Fatal("unique authenticated peer was not deferred for an exact owned release")
			}
			if err := n.handle(nodeEvent{id: victim.id, remove: true}); err != nil {
				t.Fatal(err)
			}
			deadline := f.elapsed + FailedConnectPenalty
			if n.capacityHoldUntil != deadline || len(n.peers) != 63 {
				t.Fatal("ordinary release did not reserve the final global opportunity")
			}
			// C is neither the retired victim nor its identity. Its faster/equal
			// additional carrier must not steal the last slot at the earlier
			// ordinary one-second discovery opportunity.
			f.advanceTransit(rejectedAt+time.Second, false)
			if !n.Snapshot().HasOtherForwardingLink(competitor, 0) {
				t.Fatal("unrelated competitor lost its actual working direct path")
			}
			oldNextID := n.nextLink
			additional := nodeCapacityHoldCandidate(competitor, cost)
			if err := n.handle(nodeEvent{add: additional}); !errors.Is(err, ErrCarrierCapacityRetry) || n.nextLink != oldNextID || len(n.peers) != 63 || n.capacityHoldUntil != deadline {
				t.Fatal("unrelated second standby stole or renewed the distinct peer's final opportunity")
			}
			// Config.defaults RetryDelay and BLE's normal failed-candidate retry
			// are five seconds. Advance the actual injected routing clock to that
			// normal retry; no immediate retry is supplied to win a race.
			f.advanceTransit(rejectedAt+5*time.Second, false)
			if err := n.handle(nodeEvent{add: unique}); err != nil {
				t.Fatalf("normal delayed unique retry did not receive its released slot: %v", err)
			}
			if unique.id != oldNextID+1 || n.peers[unique.id] != unique || len(n.peers) != 64 || n.capacityHoldUntil != 0 || n.capacityHoldRelease != 0 {
				t.Fatal("new unique admission expanded capacity or failed to consume the opportunity")
			}
			if !n.Snapshot().HasOtherForwardingLink(competitor, 0) || !n.Snapshot().HasOtherForwardingLink(victim.device, 0) {
				t.Fatal("capacity scheduling dropped an existing actual forwarding path")
			}
		})
	}
}
