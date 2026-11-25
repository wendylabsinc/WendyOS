//go:build !windows

package sessionbroker

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
)

func TestOperatorTargetConsumesReceiverLeaseForStaleOriginClock(t *testing.T) {
	_, adjacent, target := remoteTopologyFixture(t)
	now := time.Now()
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(target.MeshAddress(), 128), RouterID: 7, Seqno: 9}
	snapshot := localmesh.NodeSnapshot{
		Self: adjacent, CapturedAt: now,
		Links: []localmesh.PeerLink{{ID: 1, Device: target, Cost: 64, Transport: "tcp"}},
		Devices: []localmesh.Manifest{{
			Version: localmesh.ManifestVersionRoute, Device: target.UUID(), Revision: 42,
			Issued: now.Add(-598 * time.Second).UnixMilli(), Expires: now.Add(-478 * time.Second).UnixMilli(),
			AdmittedUntil: now.Add(time.Minute), Route: &origin,
			AgentPort: 50052, AgentTransport: localmesh.AgentMTLS, Platform: localmesh.PlatformWendyOS,
		}},
		Routes:              []babel.Route{{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Link: 1}},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true},
	}
	view := meshview.Topology(adjacent, snapshot, now)
	info, err := operatorManagementTarget(view, adjacent, target, now)
	if err != nil || info.Expires != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("correct-UTC operator rejected live receiver lease: %+v, %v", info, err)
	}
	// The same unchanged route/hint equality check still rejects either an
	// expired receiver lease or a mismatched route-binding deadline.
	view.Routes[0].SignedRouteExpiresAtUnixMs++
	if _, err := operatorManagementTarget(view, adjacent, target, now); err == nil {
		t.Fatal("mismatched route binding accepted")
	}
	snapshot.Devices[0].AdmittedUntil = now
	if _, err := operatorManagementTarget(meshview.Topology(adjacent, snapshot, now), adjacent, target, now); err == nil {
		t.Fatal("expired receiver lease accepted")
	}
}
