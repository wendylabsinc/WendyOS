package meshview

import (
	"crypto/sha256"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestTopologyNormalOperatorInstallationsKeepRealPrincipalAndRoutes(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice"
	identity, err := meshid.PeerFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	self, err := identity.WithInstance(uuid.MustParse("22222222-2222-4222-8222-222222222222"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := identity.WithInstance(uuid.MustParse("33333333-3333-4333-8333-333333333333"))
	if err != nil {
		t.Fatal(err)
	}
	device := meshid.NewDevice(self.Tenant(), uuid.MustParse("44444444-4444-4444-8444-444444444444"))
	now := time.Now()
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(other.MeshAddress(), 128), RouterID: 7, Seqno: 9}
	hash := sha256.Sum256([]byte("verified remote installation signer chain"))
	snapshot := localmesh.NodeSnapshot{
		Self: self, CapturedAt: now,
		Links: []localmesh.PeerLink{{ID: 1, Device: device, Cost: 128, Transport: "tcp"}},
		Devices: []localmesh.Manifest{{Version: localmesh.ManifestVersionRoute,
			Owner:    &localmesh.OperatorOwner{Principal: principal, Instance: other.UUID(), Hashes: [][]byte{hash[:]}},
			Revision: 42, Name: "operator", Issued: now.Add(-time.Second).UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), Route: &origin}},
		Routes: []babel.Route{
			{Prefix: netip.PrefixFrom(self.MeshAddress(), 128), Local: true},
			{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Metric: 256, Link: 1},
		},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true},
	}
	view := Topology(self, snapshot, now, true)
	if !view.RuntimeAvailable || view.SelfPrincipal != principal || view.SelfDeviceId != self.UUID().String() || view.SelfMeshAddress != self.MeshAddress().String() {
		t.Fatal("operator self identity lost", view)
	}
	if len(view.Announcements) != 1 || len(view.Routes) != 2 || len(view.Peers) != 1 {
		t.Fatal("operator installation route not projected", view)
	}
	announcement := view.Announcements[0]
	if announcement.Principal != principal || announcement.DeviceId != other.UUID().String() || announcement.Prefix != origin.Prefix.String() {
		t.Fatal("remote installation replaced by a device or self identity", announcement)
	}
	if announcement.AgentPort != 0 || announcement.AgentTransport != "" || announcement.Platform != "" || len(announcement.Caps) != 0 || len(announcement.Services) != 0 {
		t.Fatal("operator acquired a fabricated device endpoint", announcement)
	}
	for _, route := range view.Routes {
		if !route.Local && (route.Prefix != origin.Prefix.String() || route.OriginPrincipal != principal || !route.OriginAuthorized || route.NextHopPrincipal != device.Principal()) {
			t.Fatal("operator route authorization or physical next hop lost", route)
		}
	}
}
