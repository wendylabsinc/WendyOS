package services

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func topologyFixture(t *testing.T) (meshid.Device, localmesh.NodeSnapshot, time.Time) {
	t.Helper()
	self := adminDevice(t)
	peer, err := meshid.ParseDevice(self.Tenant(), adminPeerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: 7, Seqno: 9}
	return self, localmesh.NodeSnapshot{
		Self: self, CapturedAt: now, Peers: 2,
		Links:               []localmesh.PeerLink{{ID: 2, Device: peer, Cost: 4096, Transport: "ble"}, {ID: 1, Device: peer, Cost: 64, Transport: "ethernet"}},
		Devices:             []localmesh.Manifest{{Version: localmesh.ManifestVersionRoute, Device: peer.UUID(), Revision: 42, Name: "peer", AgentPort: 50052, AgentTransport: localmesh.AgentMTLS, Platform: localmesh.PlatformWendyOS, Caps: []string{"sensors"}, Issued: now.Add(-time.Second).UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), Route: &origin}},
		Routes:              []babel.Route{{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Metric: 128, Link: 1, NextHop: netip.MustParseAddr("fe80::2")}, {Prefix: netip.PrefixFrom(self.MeshAddress(), 128), Local: true}},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true},
	}, now
}

func TestLocalMeshTopologyAuthorizationAndIdentityChange(t *testing.T) {
	self, snapshot, _ := topologyFixture(t)
	current := self
	svc := NewLocalMeshAdminService(nil, t.TempDir(), func() (meshid.Tenant, meshid.Device) { return current.Tenant(), current })
	reads := 0
	svc.SetTopologySource(func() localmesh.NodeSnapshot { reads++; return snapshot })
	for _, ctx := range []context.Context{context.Background(), ctxWithIdentity(t, self.Principal()), ctxWithIdentity(t, userPrincipal(otherTenantID, "bob"))} {
		if _, err := svc.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("unauthorized topology: %v", err)
		}
	}
	if reads != 0 {
		t.Fatal("unauthorized caller read topology")
	}
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	got, err := svc.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{})
	if err != nil || !got.GetRuntimeAvailable() || got.GetStatus() == nil || got.GetSelfDeviceId() != self.UUID().String() {
		t.Fatalf("topology: %v %v", got, err)
	}
	otherTenant, _ := meshid.ParseTenant(otherTenantID)
	svc.SetTopologySource(func() localmesh.NodeSnapshot {
		current = meshid.NewDevice(otherTenant, self.UUID())
		return snapshot
	})
	if _, err := svc.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("enrollment replaced during snapshot: %v", err)
	}
}

// A local socket caller has no tenant of its own for the second authorization
// to reject, so a re-enrollment during the read must be caught by the device
// comparison instead of returning the old tenant's snapshot.
func TestLocalMeshTopologyLocalSocketIdentityChange(t *testing.T) {
	self, snapshot, _ := topologyFixture(t)
	current := self
	svc := NewLocalMeshAdminService(nil, t.TempDir(), func() (meshid.Tenant, meshid.Device) { return current.Tenant(), current })
	otherTenant, _ := meshid.ParseTenant(otherTenantID)
	svc.SetTopologySource(func() localmesh.NodeSnapshot {
		current = meshid.NewDevice(otherTenant, self.UUID())
		return snapshot
	})
	if _, err := svc.GetLocalMeshTopology(localAdminSocketCtx(), &pb.GetLocalMeshTopologyRequest{}); status.Code(err) != codes.Aborted {
		t.Fatalf("enrollment replaced during local socket snapshot: %v", err)
	}
}

func TestLocalMeshTopologySeparatesLinksRoutesAndLeaseTrust(t *testing.T) {
	self, snapshot, now := topologyFixture(t)
	got := projectLocalMeshTopology(self, snapshot, now)
	if len(got.Peers) != 2 || got.Peers[0].LinkId != 1 || got.Peers[0].Transport != "ethernet" || got.Peers[1].Transport != "ble" {
		t.Fatalf("parallel carrier projection: %v", got.Peers)
	}
	if len(got.Routes) != 2 || len(got.Announcements) != 1 {
		t.Fatalf("route/announcement projection: %v", got)
	}
	hint := got.Announcements[0]
	if hint.AgentPort != 50052 || hint.AgentTransport != localmesh.AgentMTLS || hint.Platform != localmesh.PlatformWendyOS || len(hint.Caps) != 1 || hint.Caps[0] != "sensors" {
		t.Fatalf("verified agent hints lost: %v", hint)
	}
	var remote *pb.LocalMeshRoute
	for _, route := range got.Routes {
		if !route.Local {
			remote = route
		}
	}
	if remote == nil || !remote.OriginAuthorized || remote.LinkId != 1 || remote.NextHopPrincipal != snapshot.Links[1].Device.Principal() || remote.NextHopAddress != "fe80::2" || remote.Metric != 128 {
		t.Fatalf("selected next hop: %v", remote)
	}
	if got.Announcements[0].TrustState != "verified-at-snapshot" || got.CapturedAtUnixMs != snapshot.CapturedAt.UnixMilli() {
		t.Fatalf("observation timestamp or trust changed: %v", got)
	}
	for _, tc := range []struct {
		name   string
		change func(*localmesh.NodeSnapshot)
	}{
		{"expired lease", func(s *localmesh.NodeSnapshot) { s.Devices[0].Expires = now.UnixMilli() }},
		{"origin mismatch", func(s *localmesh.NodeSnapshot) { s.Routes[0].RouterID++ }},
		{"sequence mismatch", func(s *localmesh.NodeSnapshot) { s.Routes[0].Seqno++ }},
		{"authorization rejected", func(s *localmesh.NodeSnapshot) { s.RouteAuthorizations = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, view, _ := topologyFixture(t)
			tc.change(&view)
			for _, route := range projectLocalMeshTopology(self, view, now).Routes {
				if !route.Local && route.OriginAuthorized {
					t.Fatal("invalid lease claimed authorized")
				}
			}
		})
	}
}

func TestLocalMeshTopologyTenantAndPrefixFiltering(t *testing.T) {
	self, snapshot, now := topologyFixture(t)
	otherTenant, _ := meshid.ParseTenant(otherTenantID)
	foreign := meshid.NewDevice(otherTenant, uuid.New())
	snapshot.Links = append(snapshot.Links, localmesh.PeerLink{ID: 3, Device: foreign, Cost: 64})
	snapshot.Routes = append(snapshot.Routes, babel.Route{Prefix: netip.PrefixFrom(foreign.MeshAddress(), 128), Link: 3})
	got := projectLocalMeshTopology(self, snapshot, now)
	if len(got.Peers) != 2 || len(got.Routes) != 2 {
		t.Fatalf("foreign route/link exposed: %v", got)
	}
	snapshot.Devices[0].Route.Prefix = netip.MustParsePrefix("fd00::/8")
	got = projectLocalMeshTopology(self, snapshot, now)
	if len(got.Announcements) != 0 || len(got.Routes) != 1 {
		t.Fatalf("aggregate claim exposed: %v", got)
	}
	snapshot.Self = foreign
	got = projectLocalMeshTopology(self, snapshot, now)
	if got.RuntimeAvailable || got.CapturedAtUnixMs != 0 || len(got.Peers)+len(got.Routes)+len(got.Announcements) != 0 {
		t.Fatal("old enrollment snapshot exposed")
	}
}

func TestLocalMeshTopologyBounds(t *testing.T) {
	self := adminDevice(t)
	now := time.Now()
	snapshot := localmesh.NodeSnapshot{Self: self, CapturedAt: now}
	for i := 0; i < maxTopologyEntries+1; i++ {
		peer := meshid.NewDevice(self.Tenant(), uuid.New())
		id := babel.LinkID(i + 1)
		origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: babel.RouterID(id), Seqno: 1}
		snapshot.Links = append(snapshot.Links, localmesh.PeerLink{ID: id, Device: peer, Cost: 64})
		snapshot.Devices = append(snapshot.Devices, localmesh.Manifest{Version: localmesh.ManifestVersionRoute, Device: peer.UUID(), Revision: 1, Route: &origin})
		snapshot.Routes = append(snapshot.Routes, babel.Route{Prefix: origin.Prefix, Link: id})
	}
	got := projectLocalMeshTopology(self, snapshot, now)
	if !got.Truncated || len(got.Peers) != maxTopologyEntries || len(got.Routes) != maxTopologyEntries || len(got.Announcements) != maxTopologyEntries {
		t.Fatalf("unbounded topology: peers=%d routes=%d announcements=%d truncated=%v", len(got.Peers), len(got.Routes), len(got.Announcements), got.Truncated)
	}
}
