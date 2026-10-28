// Package meshview projects one locally verified Node snapshot for agent and
// broker clients. No remote observation participates in route admission here.
package meshview

import (
	"net/netip"
	"sort"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

const maxTopologyEntries = 1024
const maxTopologyServiceBytes = 256 * 1024

func Topology(self meshid.Device, snapshot localmesh.NodeSnapshot, now time.Time, includeServices ...bool) *pb.LocalMeshTopology {
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
		device, err := manifest.Peer(self.Tenant())
		if err != nil {
			continue
		}
		announcement := &pb.LocalMeshAnnouncement{Principal: device.Principal(), DeviceId: device.UUID().String(), Name: manifest.Name, IssuedAtUnixMs: manifest.Issued, ExpiresAtUnixMs: manifest.Expires, Revision: manifest.Revision, TrustState: "verified-at-snapshot", AgentPort: uint32(manifest.AgentPort), AgentTransport: manifest.AgentTransport, Platform: manifest.Platform, Caps: append([]string(nil), manifest.Caps...)}
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
	if len(includeServices) > 0 && includeServices[0] {
		projectLocalMeshServices(out, snapshot, now)
	}
	return out
}

// Service descriptions are opt-in so regular Nearby polling carries only the
// compact device directory. Each service inherits the complete manifest lease;
// no second service gossip/cache or independently refreshed expiry is created.
func projectLocalMeshServices(out *pb.LocalMeshTopology, snapshot localmesh.NodeSnapshot, now time.Time) {
	out.ServicesIncluded = true
	manifests := make(map[string]localmesh.Manifest)
	for _, manifest := range snapshot.Devices {
		if !manifest.Withdraw && manifest.Expires > now.UnixMilli() {
			manifests[manifest.Device.String()] = manifest
		}
	}
	rows, bytes := 0, 0
	for _, announcement := range out.Announcements {
		available := false
		for _, route := range out.Routes {
			if route.OriginPrincipal == announcement.Principal && route.Prefix == announcement.Prefix && route.OriginRouterId == announcement.OriginRouterId && route.SequenceNumber == announcement.SequenceNumber && route.SignedRouteExpiresAtUnixMs == announcement.ExpiresAtUnixMs && route.OriginAuthorized && !route.Unreachable {
				available = true
				break
			}
		}
		if !available {
			continue
		}
		for _, service := range manifests[announcement.DeviceId].Services {
			if service.Type == "" || service.Port == 0 {
				continue // A gateway capability has no application socket.
			}
			entry := &pb.LocalMeshService{Mesh: service.Mesh, AppId: service.AppID, ServiceId: service.ServiceID, Type: service.Type, Instance: service.Instance, Port: uint32(service.Port), Txt: append([]string(nil), service.TXT...)}
			size := proto.Size(entry) + 8
			if rows == maxTopologyEntries || bytes+size > maxTopologyServiceBytes {
				out.Truncated = true
				return
			}
			rows++
			bytes += size
			announcement.Services = append(announcement.Services, entry)
		}
	}
}
