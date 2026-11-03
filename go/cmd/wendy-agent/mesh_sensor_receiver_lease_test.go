package main

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// The projection consumes a verified router snapshot; certificate and signed
// body validation remain the Directory's responsibility. These fixtures retain
// a real typed tenant/device and an installed exact native endpoint route.
func sensorReceiverLeaseSnapshot(t *testing.T, now time.Time) (localmesh.NodeSnapshot, meshid.Device) {
	t.Helper()
	peer, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/device/44444444-4444-4444-8444-444444444444")
	if err != nil {
		t.Fatal(err)
	}
	self, err := meshid.ParseDevice(peer.Tenant(), "44444444-4444-4444-8444-444444444445")
	if err != nil {
		t.Fatal(err)
	}
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: 100, Seqno: 1}
	manifest := localmesh.Manifest{Version: localmesh.ManifestVersionRoute, Device: peer.UUID(), Name: "Clocked sensor", Revision: 1, Issued: now.Add(-2 * time.Minute).UnixMilli(), Expires: now.Add(-time.Minute).UnixMilli(), AdmittedUntil: now.Add(time.Minute), AgentPort: 5054, AgentTransport: localmesh.AgentMTLS, Platform: localmesh.PlatformLite, Caps: []string{localmesh.CapabilitySensors}, Route: &origin}
	route := babel.Route{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Link: 1, Metric: 100}
	return localmesh.NodeSnapshot{Self: self, Devices: []localmesh.Manifest{manifest}, Routes: []babel.Route{route}, ForwardingRoutes: []babel.Route{route}, RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true}}, peer
}

func TestMeshSensorSourcesUseAdmittedReceiverLease(t *testing.T) {
	now := time.Now()
	snapshot, peer := sensorReceiverLeaseSnapshot(t, now)
	if _, native, err := meshsession.ResolveAgentMTLS(snapshot, peer, 5054); err != nil || !native {
		t.Fatal("fixture native endpoint is not live", native, err)
	}
	before := snapshot.Devices[0]
	sources := meshSensorSources(snapshot)
	if len(sources) != 1 || sources[0].PrincipalURI != peer.Principal() || sources[0].Port != 5054 || !sources[0].Sensorlink || !sources[0].IsMTLS {
		t.Fatal("valid local lease was hidden by stale signing UTC", sources)
	}
	if snapshot.Devices[0].Issued != before.Issued || snapshot.Devices[0].Expires != before.Expires || !snapshot.Devices[0].AdmittedUntil.Equal(before.AdmittedUntil) {
		t.Fatal("projection rewrote signed dates or renewed its admitted lease")
	}
	// A future signed expiry cannot keep an expired receiver lease alive.
	snapshot.Devices[0].Issued = now.Add(time.Hour).UnixMilli()
	snapshot.Devices[0].Expires = now.Add(time.Hour + time.Minute).UnixMilli()
	snapshot.Devices[0].AdmittedUntil = now.Add(-time.Second)
	if sources := meshSensorSources(snapshot); len(sources) != 0 {
		t.Fatal("future signing UTC revived an expired local lease", sources)
	}
}

func TestMeshSensorReceiverLeasePreservesEligibilityAndCurrentRevision(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{"withdrawn", "capability-removed", "route-absent", "route-unreachable", "non-native", "non-lite"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, _ := sensorReceiverLeaseSnapshot(t, now)
			// Model a newly admitted higher revision. Even a still-live local lease
			// does not lend authority to capabilities or routes it no longer has.
			snapshot.Devices[0].Revision++
			switch mode {
			case "withdrawn":
				snapshot.Devices[0].Withdraw = true
			case "capability-removed":
				snapshot.Devices[0].Caps = nil
			case "route-absent":
				snapshot.ForwardingRoutes = []babel.Route{}
			case "route-unreachable":
				snapshot.ForwardingRoutes[0].Unreachable = true
			case "non-native":
				snapshot.Devices[0].AgentTransport = "future-native"
			case "non-lite":
				snapshot.Devices[0].Platform = localmesh.PlatformWendyOS
			}
			if sources := meshSensorSources(snapshot); len(sources) != 0 {
				t.Fatal("current revision lost its eligibility gate", mode, sources)
			}
		})
	}
	snapshot, peer := sensorReceiverLeaseSnapshot(t, now)
	if sources := meshSensorSources(snapshot); len(sources) != 1 {
		t.Fatal("initial current revision missing", sources)
	}
	snapshot.Devices[0].Revision++
	snapshot.Devices[0].AgentPort = 5055
	snapshot.Devices[0].Name = "Updated sensor"
	if sources := meshSensorSources(snapshot); len(sources) != 1 || sources[0].Port != 5055 || sources[0].DisplayName != "Updated sensor" || sources[0].PrincipalURI != peer.Principal() {
		t.Fatal("projection retained an older revision's endpoint", sources)
	}
}
