package localmesh

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"testing"
)

func TestNewNeighbourOwnProofPrecedesBulkWithoutRepeatingIt(t *testing.T) {
	directory, chain, key, foreign, now := directoryFixture(t)
	own := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	ownChain, ownKey := addRouteTestIdentity(t, directory, own, now)
	foreignWire, err := SignManifest(routeManifest(foreign, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	ownManifest := routeManifest(own, now)
	ownManifest.Route.RouterID++
	ownWire, err := SignManifest(ownManifest, ownChain, ownKey, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []SignedManifest{foreignWire, ownWire} {
		if _, err := directory.Accept(wire, now); err != nil {
			t.Fatal(err)
		}
	}
	if records := directory.Records(now); len(records) != 2 || records[0].Principal != foreign.Principal() {
		t.Fatal("fixture must put foreign proof first in ordinary sort")
	}
	sender := NewSynchronizer(directory, directory.cache, netgossip.NewPool())
	defer sender.Close()
	first := sender.bootstrapOwn(own.Principal(), now, 1)
	bulk := sender.ReconcileBounded(now, 1)
	if len(first) != 1 || len(bulk) != 1 {
		t.Fatalf("bootstrap=%v bulk=%v", first, bulk)
	}
	ownBytes, _ := netwire.Marshal(ownWire)
	foreignBytes, _ := netwire.Marshal(foreignWire)
	if first[0].Gossip == nil || first[0].Gossip.ID() != netgossip.Identify(netgossip.Manifest, ownBytes) {
		t.Fatal("own proof was not first")
	}
	if bulk[0].Gossip == nil || bulk[0].Gossip.ID() != netgossip.Identify(netgossip.Manifest, foreignBytes) {
		t.Fatal("bulk pass repeated bootstrap or omitted foreign proof")
	}
	if rest := sender.ContinueReconcile(now, 1); len(rest) != 0 {
		t.Fatal("finished initial pass repeated records")
	}
}
