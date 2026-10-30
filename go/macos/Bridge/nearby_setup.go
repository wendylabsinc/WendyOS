package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"unsafe"
)

//export WDNearbyDiscover
func WDNearbyDiscover() *C.WDNearbyDevicesResult {
	values, err := desktop.NearbyDiscover()
	p := alloc[C.WDNearbyDevicesResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDNearbyDevice](len(values)), C.size_t(len(values))
	for i, v := range values {
		unsafe.Slice(p.items, len(values))[i] = C.WDNearbyDevice{address: C.CString(v.Address), name: C.CString(v.Name), rssi: C.int32_t(v.RSSI)}
	}
	return p
}

//export WDNearbyDevicesFree
func WDNearbyDevicesFree(p *C.WDNearbyDevicesResult) {
	if p == nil {
		return
	}
	for _, v := range unsafe.Slice(p.items, int(p.count)) {
		free(v.address)
		free(v.name)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

func nearbySetupResult(s backend.NearbySetupState, err error) *C.WDNearbySetupResult {
	p := alloc[C.WDNearbySetupResult](1)
	p.status = status(err)
	p.session = C.uint64_t(s.Session)
	p.connected, p.lite, p.enrolled = boolInt(s.Connected), boolInt(s.Lite), boolInt(s.Enrolled)
	p.attempted, p.pending, p.completed = boolInt(s.Attempted), boolInt(s.Pending), boolInt(s.Completed)
	p.wifi_connected = boolInt(s.WiFiConnected)
	p.ssid, p.device_id = C.CString(s.SSID), C.CString(s.DeviceID)
	p.networks, p.network_count = alloc[*C.char](len(s.Networks)), C.size_t(len(s.Networks))
	for i, v := range s.Networks {
		unsafe.Slice(p.networks, len(s.Networks))[i] = C.CString(v)
	}
	return p
}

//export WDNearbyOpen
func WDNearbyOpen(address *C.char) *C.WDNearbySetupResult {
	return nearbySetupResult(desktop.NearbyOpen(C.GoString(address)))
}

//export WDNearbyAction
func WDNearbyAction(session C.uint64_t, action C.int32_t, ssid, password, account, organization, name *C.char) *C.WDNearbySetupResult {
	return nearbySetupResult(desktop.NearbyAction(uint64(session), int(action), C.GoString(ssid), C.GoString(password), C.GoString(account), C.GoString(organization), C.GoString(name)))
}

//export WDNearbyClose
func WDNearbyClose(session C.uint64_t) { desktop.NearbyClose(uint64(session)) }

//export WDNearbySetupFree
func WDNearbySetupFree(p *C.WDNearbySetupResult) {
	if p == nil {
		return
	}
	for _, v := range unsafe.Slice(p.networks, int(p.network_count)) {
		free(v)
	}
	free(p.networks)
	free(p.ssid)
	free(p.device_id)
	free(p.status.message)
	free(p)
}
