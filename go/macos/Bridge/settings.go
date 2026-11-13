package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"unsafe"
)

//export WDNetworkSettings
func WDNetworkSettings(session C.uint64_t) *C.WDNetworkSettingsResult {
	value, err := desktop.NetworkSettings(uint64(session))
	return networkSettings(value, err)
}
func networkSettings(value backend.NetworkSettings, err error) *C.WDNetworkSettingsResult {
	p := alloc[C.WDNetworkSettingsResult](1)
	p.status = status(err)
	topology := value.Topology
	p.topology_available = boolInt(topology != nil)
	p.topology_runtime = boolInt(topology.GetRuntimeAvailable())
	p.topology_truncated = boolInt(topology.GetTruncated())
	p.topology_captured_ms = C.int64_t(topology.GetCapturedAtUnixMs())
	p.topology_error = C.CString(value.TopologyError)
	p.self_principal, p.self_address = C.CString(topology.GetSelfPrincipal()), C.CString(topology.GetSelfMeshAddress())
	names := map[string]string{}
	for _, a := range topology.GetAnnouncements() {
		names[a.GetPrincipal()] = a.GetName()
	}
	announcements := topology.GetAnnouncements()
	p.mesh_announcements, p.mesh_announcement_count = alloc[C.WDMeshAnnouncement](len(announcements)), C.size_t(len(announcements))
	for i, a := range announcements {
		unsafe.Slice(p.mesh_announcements, len(announcements))[i] = C.WDMeshAnnouncement{principal: C.CString(a.GetPrincipal()), device_id: C.CString(a.GetDeviceId()), name: C.CString(a.GetName()), prefix: C.CString(a.GetPrefix())}
	}
	links := topology.GetPeers()
	p.mesh_links, p.mesh_link_count = alloc[C.WDMeshPeer](len(links)), C.size_t(len(links))
	for i, n := range links {
		unsafe.Slice(p.mesh_links, len(links))[i] = C.WDMeshPeer{principal: C.CString(n.GetPrincipal()), device_id: C.CString(n.GetDeviceId()), address: C.CString(n.GetMeshAddress()), transport: C.CString(n.GetTransport()), name: C.CString(names[n.GetPrincipal()]), link_id: C.uint64_t(n.GetLinkId()), cost: C.uint32_t(n.GetCost())}
	}
	routes := topology.GetRoutes()
	p.mesh_routes, p.mesh_route_count = alloc[C.WDMeshRoute](len(routes)), C.size_t(len(routes))
	for i, r := range routes {
		unsafe.Slice(p.mesh_routes, len(routes))[i] = C.WDMeshRoute{prefix: C.CString(r.GetPrefix()), origin: C.CString(r.GetOriginPrincipal()), next_hop: C.CString(r.GetNextHopPrincipal()), metric: C.uint32_t(r.GetMetric()), local: boolInt(r.GetLocal()), unreachable: boolInt(r.GetUnreachable()), authorized: boolInt(r.GetOriginAuthorized())}
	}
	p.wifi_available = boolInt(value.WiFi != nil && value.WiFiError == "")
	p.wifi_connected = boolInt(value.WiFi.GetConnected())
	p.ssid = C.CString(value.WiFi.GetSsid())
	p.wifi_error, p.scan_error = C.CString(value.WiFiError), C.CString(value.ScanError)
	p.known_error, p.mesh_error = C.CString(value.KnownError), C.CString(value.MeshError)
	p.networks, p.network_count = alloc[C.WDWiFiNetwork](len(value.Networks)), C.size_t(len(value.Networks))
	for i, n := range value.Networks {
		unsafe.Slice(p.networks, len(value.Networks))[i] = C.WDWiFiNetwork{ssid: C.CString(n.GetSsid()), security: C.int32_t(n.GetSecurity()), signal: C.int32_t(n.GetSignalStrength()), has_signal: boolInt(n.SignalStrength != nil), known: boolInt(n.GetIsKnown()), connected: boolInt(n.GetIsConnected()), priority: C.int32_t(n.GetPriority())}
	}
	p.known, p.known_count = alloc[C.WDWiFiNetwork](len(value.Known)), C.size_t(len(value.Known))
	for i, n := range value.Known {
		unsafe.Slice(p.known, len(value.Known))[i] = C.WDWiFiNetwork{ssid: C.CString(n.GetSsid()), security: C.int32_t(n.GetSecurity()), known: 1, priority: C.int32_t(n.GetPriority())}
	}
	p.mesh_available = boolInt(value.Mesh != nil && value.Mesh.GetConfigured() != nil)
	p.runtime_available = boolInt(value.Mesh.GetRuntimeAvailable())
	p.mesh_state, p.mesh_detail, p.gateway = C.CString(value.Mesh.GetState()), C.CString(value.Mesh.GetDetail()), C.CString(value.Mesh.GetGatewayPrincipal())
	p.peers = C.uint32_t(value.Mesh.GetAuthenticatedPeers())
	config := value.Mesh.GetConfigured()
	flags := []bool{config.GetParticipate(), config.GetRoam(), config.GetShareUplink(), config.GetNan(), config.GetBle(), config.GetEthernet(), config.GetInfrastructureWifi()}
	for i, enabled := range flags {
		if enabled {
			p.mesh_config |= 1 << i
		}
	}
	return p
}

//export WDNetworkSettingsFree
func WDNetworkSettingsFree(p *C.WDNetworkSettingsResult) {
	if p == nil {
		return
	}
	for _, n := range unsafe.Slice(p.networks, int(p.network_count)) {
		free(n.ssid)
	}
	for _, n := range unsafe.Slice(p.known, int(p.known_count)) {
		free(n.ssid)
	}
	for _, n := range unsafe.Slice(p.mesh_links, int(p.mesh_link_count)) {
		free(n.principal)
		free(n.device_id)
		free(n.address)
		free(n.transport)
		free(n.name)
	}
	for _, r := range unsafe.Slice(p.mesh_routes, int(p.mesh_route_count)) {
		free(r.prefix)
		free(r.origin)
		free(r.next_hop)
	}
	for _, a := range unsafe.Slice(p.mesh_announcements, int(p.mesh_announcement_count)) {
		free(a.principal)
		free(a.device_id)
		free(a.name)
		free(a.prefix)
	}
	free(p.mesh_links)
	free(p.mesh_routes)
	free(p.mesh_announcements)
	free(p.topology_error)
	free(p.self_principal)
	free(p.self_address)
	free(p.networks)
	free(p.known)
	free(p.ssid)
	free(p.wifi_error)
	free(p.scan_error)
	free(p.known_error)
	free(p.mesh_error)
	free(p.mesh_state)
	free(p.mesh_detail)
	free(p.gateway)
	free(p.status.message)
	free(p)
}

//export WDJoinWiFi
func WDJoinWiFi(session C.uint64_t, ssid, password *C.char, security, hidden C.int32_t) *C.WDStatus {
	return statusResult(desktop.JoinWiFi(uint64(session), C.GoString(ssid), C.GoString(password), int32(security), hidden != 0))
}

//export WDDisconnectWiFi
func WDDisconnectWiFi(session C.uint64_t) *C.WDStatus {
	return statusResult(desktop.DisconnectWiFi(uint64(session)))
}

//export WDForgetWiFi
func WDForgetWiFi(session C.uint64_t, ssid *C.char) *C.WDStatus {
	return statusResult(desktop.ForgetWiFi(uint64(session), C.GoString(ssid)))
}

//export WDConfigureMesh
func WDConfigureMesh(session C.uint64_t, changed, enabled C.uint32_t) *C.WDStatus {
	return statusResult(desktop.ConfigureMesh(uint64(session), uint32(changed), uint32(enabled)))
}
