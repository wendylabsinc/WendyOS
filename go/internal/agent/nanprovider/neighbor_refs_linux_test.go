//go:build linux

package nanprovider

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestRetiringOneNANPathKeepsSharedNeighbor(t *testing.T) {
	set, del := 0, 0
	r := &nanNeighborRefs{set: func(*netlink.Neigh) error { set++; return nil }, del: func(*netlink.Neigh) error { del++; return nil }}
	n := &netlink.Neigh{IP: net.ParseIP("10.89.0.2"), HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 2}}
	a, err := r.acquire(n)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.acquire(n)
	if err != nil {
		t.Fatal(err)
	}
	if set != 1 {
		t.Fatal("crossed path overwrote existing neighbour")
	}
	a()
	a()
	if del != 0 {
		t.Fatal("retiring duplicate removed winner's neighbour")
	}
	b()
	if del != 1 {
		t.Fatal("last owner did not remove neighbour")
	}
}

func TestNANEventsResolveExactIncarnationOrRefuseAmbiguity(t *testing.T) {
	const nmi = "02:00:00:00:00:01"
	active := map[string]*radioLink{
		"out": {peer: radioPeer{Device: testDevice(t, 2), NMI: nmi, ID: "1", Outgoing: true}},
		"in":  {peer: radioPeer{Device: testDevice(t, 2), NMI: nmi, ID: "2"}},
	}
	if key, _ := radioEventLink(nmi, "2", active); key != "in" {
		t.Fatal("incoming event selected outgoing path")
	}
	if key, _ := radioEventLink(nmi, "1", active); key != "out" {
		t.Fatal("outgoing event selected incoming path")
	}
	active["in"].peer.ID = "1"
	if _, l := radioEventLink(nmi, "1", active); l != nil {
		t.Fatal("ambiguous event could remove winner")
	}
}
