package main

import (
	"github.com/wendylabsinc/wendy/go/macos/backend"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"testing"
	"unsafe"
)

func TestNetworkSettingsOwnsTopology(t *testing.T) {
	fixture := backend.NetworkSettings{Topology: &pb.LocalMeshTopology{
		SelfPrincipal: "local", SelfMeshAddress: "fd00::1", RuntimeAvailable: true,
		CapturedAtUnixMs: 123456789, Truncated: true,
		Peers:         []*pb.LocalMeshPeerLink{{LinkId: 7, Principal: "peer", Transport: "ethernet", Cost: 2}},
		Announcements: []*pb.LocalMeshAnnouncement{{Principal: "peer", Name: "Cámara"}, {Principal: "indirect", DeviceId: "device-3", Name: "Indirect camera", Prefix: "fd00::3/128"}},
		Routes:        []*pb.LocalMeshRoute{{Prefix: "fd00::3/128", OriginPrincipal: "indirect", NextHopPrincipal: "peer", Unreachable: true, OriginAuthorized: true}},
	}}
	result := networkSettings(fixture, nil)
	defer WDNetworkSettingsFree(result)
	fixture.Topology.Peers[0].Transport = "changed"
	fixture.Topology.Announcements[1].Name = "changed"
	if result.topology_available != 1 || result.topology_runtime != 1 || result.topology_truncated != 1 || result.topology_captured_ms != 123456789 {
		t.Fatal("topology metadata lost")
	}
	if result.mesh_link_count != 1 || result.mesh_links.link_id != 7 || cstring(unsafe.Pointer(result.mesh_links.transport)) != "ethernet" || cstring(unsafe.Pointer(result.mesh_links.name)) != "Cámara" {
		t.Fatal("link identity, name, or owned transport lost")
	}
	if result.mesh_route_count != 1 || result.mesh_routes.unreachable != 1 || result.mesh_routes.authorized != 1 || cstring(unsafe.Pointer(result.mesh_routes.origin)) != "indirect" || cstring(unsafe.Pointer(result.mesh_routes.next_hop)) != "peer" {
		t.Fatal("route semantics lost")
	}
	if result.mesh_announcement_count != 2 {
		t.Fatal("indirect device announcement lost")
	}
	announcement := unsafe.Slice(result.mesh_announcements, int(result.mesh_announcement_count))[1]
	if cstring(unsafe.Pointer(announcement.principal)) != "indirect" || cstring(unsafe.Pointer(announcement.device_id)) != "device-3" || cstring(unsafe.Pointer(announcement.name)) != "Indirect camera" || cstring(unsafe.Pointer(announcement.prefix)) != "fd00::3/128" {
		t.Fatal("announcement identity, owned name, or prefix lost")
	}
	WDNetworkSettingsFree(networkSettings(backend.NetworkSettings{}, nil))
	WDNetworkSettingsFree(nil)
}
