package services

import (
	"context"
	"net/netip"
	"sort"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxTopologyEntries = 1024

// SetTopologySource installs a read-only coherent node snapshot source. A nil
// source means the runtime is absent, which is distinct from an empty mesh.
func (s *LocalMeshAdminService) SetTopologySource(source func() localmesh.NodeSnapshot) {
	s.mu.Lock()
	s.topology = source
	s.mu.Unlock()
}

func (s *LocalMeshAdminService) GetLocalMeshTopology(ctx context.Context, _ *pb.GetLocalMeshTopologyRequest) (*pb.LocalMeshTopology, error) {
	device, err := s.authorizeRead(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	source := s.topology
	sharing, carriers, err := s.read(device)
	var runtimeStatus *pb.LocalMeshStatus
	if err == nil {
		runtimeStatus = s.response(sharing, carriers)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading local net configuration: %v", err)
	}
	var snapshot localmesh.NodeSnapshot
	if source != nil {
		snapshot = source()
	}
	// Enrollment may have changed while the source was read. Never label an old
	// tenant's snapshot with the new identity or return it to a new tenant caller.
	current, err := s.authorizeRead(ctx)
	if err != nil {
		return nil, err
	}
	if current != device {
		return nil, status.Error(codes.Aborted, "device enrollment changed; retry topology")
	}
	out := projectLocalMeshTopology(device, snapshot, time.Now())
	out.Status = runtimeStatus
	return out, nil
}

func projectLocalMeshTopology(self meshid.Device, snapshot localmesh.NodeSnapshot, now time.Time) *pb.LocalMeshTopology {
	out := &pb.LocalMeshTopology{SelfPrincipal: self.Principal(), SelfTenantId: self.Tenant().String(), SelfDeviceId: self.UUID().String(), SelfMeshAddress: self.MeshAddress().String()}
	if snapshot.Self != self || snapshot.CapturedAt.IsZero() {
		return out
	}
	out.RuntimeAvailable = true
	out.CapturedAtUnixMs = snapshot.CapturedAt.UnixMilli()
	links := make(map[babel.LinkID]meshid.Device)
	for _, link := range snapshot.Links {
		if !link.Device.IsValid() || link.Device.Tenant() != self.Tenant() || link.ID == 0 {
			continue
		}
		links[link.ID] = link.Device
		out.Peers = append(out.Peers, &pb.LocalMeshPeerLink{LinkId: uint64(link.ID), Principal: link.Device.Principal(), DeviceId: link.Device.UUID().String(), MeshAddress: link.Device.MeshAddress().String(), Transport: link.Transport, Cost: uint32(link.Cost)})
	}
	sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].LinkId < out.Peers[j].LinkId })
	if len(out.Peers) > maxTopologyEntries {
		out.Peers, out.Truncated = out.Peers[:maxTopologyEntries], true
	}

	// Manifest signatures and certificate trust were checked by Directory at
	// capture. Project only public fields; never return signed wire payloads.
	origins := make(map[localmesh.RouteOrigin]*pb.LocalMeshAnnouncement)
	knownPrefixes := map[netip.Prefix]string{netip.PrefixFrom(self.MeshAddress(), 128): self.Principal()}
	for _, manifest := range snapshot.Devices {
		if manifest.Withdraw {
			continue
		}
		device := meshid.NewDevice(self.Tenant(), manifest.Device)
		announcement := &pb.LocalMeshAnnouncement{Principal: device.Principal(), DeviceId: device.UUID().String(), Name: manifest.Name, IssuedAtUnixMs: manifest.Issued, ExpiresAtUnixMs: manifest.Expires, Revision: manifest.Revision, TrustState: "verified-at-snapshot"}
		if origin := manifest.Route; origin != nil && manifest.Version == localmesh.ManifestVersionRoute {
			if origin.Prefix != netip.PrefixFrom(device.MeshAddress(), 128) {
				continue
			}
			announcement.Prefix, announcement.OriginRouterId, announcement.SequenceNumber = origin.Prefix.String(), uint64(origin.RouterID), uint32(origin.Seqno)
			origins[*origin] = announcement
			knownPrefixes[origin.Prefix] = device.Principal()
		}
		out.Announcements = append(out.Announcements, announcement)
	}
	sort.Slice(out.Announcements, func(i, j int) bool { return out.Announcements[i].Principal < out.Announcements[j].Principal })
	if len(out.Announcements) > maxTopologyEntries {
		out.Announcements, out.Truncated = out.Announcements[:maxTopologyEntries], true
	}
	for _, route := range snapshot.Routes {
		principal, known := knownPrefixes[route.Prefix]
		if !known || (route.Local && principal != self.Principal()) {
			continue
		}
		peer, hasPeer := links[route.Link]
		if !route.Local && !route.Unreachable && !hasPeer {
			continue
		}
		entry := &pb.LocalMeshRoute{Prefix: route.Prefix.String(), OriginRouterId: uint64(route.RouterID), SequenceNumber: uint32(route.Seqno), Metric: uint32(route.Metric), LinkId: uint64(route.Link), Local: route.Local, Unreachable: route.Unreachable, OriginPrincipal: principal}
		if hasPeer {
			entry.NextHopPrincipal = peer.Principal()
		}
		if route.NextHop.IsValid() {
			entry.NextHopAddress = route.NextHop.String()
		}
		origin := localmesh.RouteOrigin{Prefix: route.Prefix, RouterID: route.RouterID, Seqno: route.Seqno}
		if announcement := origins[origin]; announcement != nil {
			entry.SignedRouteExpiresAtUnixMs = announcement.ExpiresAtUnixMs
			entry.OriginAuthorized = snapshot.RouteAuthorizations[origin] && announcement.ExpiresAtUnixMs > now.UnixMilli()
		}
		out.Routes = append(out.Routes, entry)
	}
	sort.Slice(out.Routes, func(i, j int) bool { return out.Routes[i].Prefix < out.Routes[j].Prefix })
	if len(out.Routes) > maxTopologyEntries {
		out.Routes, out.Truncated = out.Routes[:maxTopologyEntries], true
	}
	return out
}
