package main

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Router-directory admission verifies manifests before this projection runs.
// Native endpoints require no separate catalogue record or inventory exchange.
func TestMeshSensorSourcesFollowRouteAndAgentPort(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const device = "44444444-4444-4444-8444-444444444444"
	peer, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/" + tenant + "/device/" + device)
	if err != nil {
		t.Fatal(err)
	}
	self, err := meshid.ParseDevice(peer.Tenant(), "44444444-4444-4444-8444-444444444445")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := localmesh.NodeSnapshot{
		Self: self,
		Devices: []localmesh.Manifest{{Device: peer.UUID(), Name: "Lab C5", Expires: time.Now().Add(time.Minute).UnixMilli(), AgentPort: 5054,
			AgentTransport: localmesh.AgentMTLS, Platform: localmesh.PlatformLite, Caps: []string{localmesh.CapabilitySensors}}},
		Routes: []babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128)}},
	}
	check := func(port int) {
		t.Helper()
		got := meshSensorSources(snapshot)
		if len(got) != 1 || got[0].PrincipalURI != peer.Principal() || got[0].IPAddress != peer.MeshAddress().String() || got[0].Port != port || !got[0].WendyLite || !got[0].Sensorlink || !got[0].IsMTLS {
			t.Fatalf("source = %+v, want native WendyCom endpoint at port %d", got, port)
		}
	}
	check(5054)
	snapshot.Routes = nil
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("route absent: %+v", got)
	}
	snapshot.Routes = []babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128)}}
	snapshot.Devices[0].AgentPort = 5055
	check(5055)
	snapshot.Devices[0].Caps = nil // Provider stopped; native management remains.
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("provider absent: %+v", got)
	}
	snapshot.Devices[0].Caps = []string{localmesh.CapabilitySensors}
	snapshot.Devices[0].Expires = time.Now().Add(-time.Second).UnixMilli()
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("router lease expired: %+v", got)
	}
	snapshot.Devices[0].Expires = time.Now().Add(time.Minute).UnixMilli()
	snapshot.Devices[0].Withdraw = true
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("withdrawn provider: %+v", got)
	}
}
