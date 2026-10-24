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
		Announcements: []*pb.LocalMeshAnnouncement{{Principal: "peer", Name: "Cámara"}},
		Routes:        []*pb.LocalMeshRoute{{Prefix: "fd00::2/128", NextHopPrincipal: "peer", Unreachable: true, OriginAuthorized: true}},
	}}
	result := networkSettings(fixture, nil)
	defer WDNetworkSettingsFree(result)
	fixture.Topology.Peers[0].Transport = "changed"
	if result.topology_available != 1 || result.topology_runtime != 1 || result.topology_truncated != 1 || result.topology_captured_ms != 123456789 {
		t.Fatal("topology metadata lost")
	}
	if result.mesh_link_count != 1 || result.mesh_links.link_id != 7 || cstring(unsafe.Pointer(result.mesh_links.transport)) != "ethernet" || cstring(unsafe.Pointer(result.mesh_links.name)) != "Cámara" {
		t.Fatal("link identity, name, or owned transport lost")
	}
	if result.mesh_route_count != 1 || result.mesh_routes.unreachable != 1 || result.mesh_routes.authorized != 1 {
		t.Fatal("route semantics lost")
	}
	WDNetworkSettingsFree(networkSettings(backend.NetworkSettings{}, nil))
	WDNetworkSettingsFree(nil)
}
