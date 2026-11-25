package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"unsafe"
)

func nearbyWiFiResult(s backend.NearbyWiFiState, err error) *C.WDNearbyWiFiResult {
	p := alloc[C.WDNearbyWiFiResult](1)
	p.status = status(err)
	p.session = C.uint64_t(s.Session)
	p.connected, p.wifi_connected = boolInt(s.Connected), boolInt(s.WiFiConnected)
	p.ssid, p.device_id = C.CString(s.SSID), C.CString(s.DeviceID)
	p.principal, p.organization = C.CString(s.Principal), C.CString(s.Organization)
	p.networks, p.network_count = alloc[*C.char](len(s.Networks)), C.size_t(len(s.Networks))
	for i, v := range s.Networks {
		unsafe.Slice(p.networks, len(s.Networks))[i] = C.CString(v)
	}
	return p
}

//export WDNearbyWiFiOpen
func WDNearbyWiFiOpen(address, name, account *C.char) *C.WDNearbyWiFiResult {
	return nearbyWiFiResult(desktop.NearbyWiFiOpen(C.GoString(address), C.GoString(name), C.GoString(account)))
}

//export WDNearbyWiFiAction
func WDNearbyWiFiAction(session C.uint64_t, action C.int32_t, ssid, password *C.char) *C.WDNearbyWiFiResult {
	return nearbyWiFiResult(desktop.NearbyWiFiAction(uint64(session), int(action), C.GoString(ssid), C.GoString(password)))
}

//export WDNearbyWiFiClose
func WDNearbyWiFiClose(session C.uint64_t) { desktop.NearbyWiFiClose(uint64(session)) }

//export WDNearbyWiFiFree
func WDNearbyWiFiFree(p *C.WDNearbyWiFiResult) {
	if p == nil {
		return
	}
	for _, v := range unsafe.Slice(p.networks, int(p.network_count)) {
		free(v)
	}
	free(p.networks)
	free(p.ssid)
	free(p.device_id)
	free(p.principal)
	free(p.organization)
	free(p.status.message)
	free(p)
}
