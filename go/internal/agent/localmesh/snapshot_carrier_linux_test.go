//go:build linux

package localmesh

import (
	"testing"
	"time"
)

func TestNodeRefreshPreservesCarrierMetadataAndLiveCounters(t *testing.T) {
	n := routePublicationNode(t)
	peer := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	sender := &carrierIPSender{sender: &admissionFixture{capacity: 1356}, negotiated: WholeIPDatagramLimit}
	p := &nodePeer{id: 7, device: peer, cost: EthernetLinkCost, transport: "ethernet", link: &quicNodeLink{sendIP: sender}}
	n.peers[p.id] = p
	packet, fragments := assemblyPacket(t, TunnelMTU, 1)
	now := time.Now()
	if _, err := p.assembler.Receive(fragments[0], now); err != nil {
		t.Fatal(err)
	}
	if err := sender.sendIP(1, packet); err != nil {
		t.Fatal(err)
	}
	n.refresh()
	first := n.Snapshot()
	if first.Self != n.Credentials.Device || first.CapturedAt.IsZero() || len(first.Links) != 1 {
		t.Fatalf("missing authenticated snapshot identity or link: %+v", first)
	}
	link := first.Links[0]
	if link.ID != p.id || link.Device != peer || link.Cost != EthernetLinkCost || link.Transport != "ethernet" || link.CarrierIP == nil || link.Reassembly == nil {
		t.Fatalf("carrier diagnostics or topology metadata lost during refresh: %+v", link)
	}
	if link.CarrierIP.WholePackets != 1 || link.Reassembly.PendingPackets != 1 {
		t.Fatalf("wrong initial carrier counters: tx=%+v rx=%+v", link.CarrierIP, link.Reassembly)
	}
	if _, err := p.assembler.Receive(fragments[1], now); err != nil {
		t.Fatal(err)
	}
	if err := sender.sendIP(2, packet); err != nil {
		t.Fatal(err)
	}
	// No graph refresh: counter owners continue publishing behind its pointers.
	second := n.Snapshot()
	if second.CapturedAt != first.CapturedAt || second.Links[0].CarrierIP.WholePackets != 2 || second.Links[0].Reassembly.PendingPackets != 0 || second.Links[0].Reassembly.ReassembledPackets != 1 {
		t.Fatal("carrier counters did not remain live independently of graph refresh")
	}
	if first.Links[0].CarrierIP.WholePackets != 1 || first.Links[0].Reassembly.PendingPackets != 1 {
		t.Fatal("later carrier activity mutated a previously returned snapshot")
	}
	second.Links[0].ID = 99
	second.Links[0].Transport = "caller mutation"
	second.Links[0].CarrierIP.WholePackets = 99
	second.Links[0].Reassembly.ReassembledPackets = 99
	third := n.Snapshot().Links[0]
	if third.ID != p.id || third.Transport != "ethernet" || third.CarrierIP.WholePackets != 2 || third.Reassembly.ReassembledPackets != 1 {
		t.Fatal("caller mutated live carrier state through a returned snapshot")
	}
	delete(n.peers, p.id)
	n.refresh()
	if len(n.Snapshot().Links) != 0 {
		t.Fatal("removed peer retained carrier metadata or counters")
	}
}
