package netmesh

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestMetadataRequiresLiveAuthorizedRoute(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	device, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	now := time.Now()
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: 1, Seqno: 2}
	agent := &localmesh.AgentMetadata{Version: "signed-build", OS: "wendyos", OSVersion: "WendyOS-0.19.5"}
	snapshot := localmesh.NodeSnapshot{
		Self:                device,
		Links:               []localmesh.PeerLink{{ID: 1, Device: device, Transport: "ble"}},
		Devices:             []localmesh.Manifest{{Device: device.UUID(), AgentPort: 50052, Agent: agent, Route: &origin, Expires: now.Add(time.Minute).UnixMilli()}},
		Routes:              []babel.Route{{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Link: 1}},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true},
	}
	if got := devicesFromSnapshot(snapshot, now); len(got) != 1 || got[0].Agent == nil || *got[0].Agent != *agent {
		t.Fatalf("lost verified metadata: %+v", got)
	}
	if got := devicesFromSnapshot(snapshot, now.Add(2*time.Minute)); len(got) != 0 {
		t.Fatal("expired metadata produced presence")
	}
	snapshot.RouteAuthorizations[origin] = false
	if got := devicesFromSnapshot(snapshot, now); len(got) != 0 {
		t.Fatal("metadata bypassed route authorization")
	}
	snapshot.RouteAuthorizations[origin] = true
	snapshot.Routes[0].Unreachable = true
	if got := devicesFromSnapshot(snapshot, now); len(got) != 0 {
		t.Fatal("metadata bypassed route reachability")
	}
}
