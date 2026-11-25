package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "unsafe"

//export WDBluetoothScan
func WDBluetoothScan(session C.uint64_t) *C.WDBluetoothResult {
	devices, err := desktop.BluetoothScan(uint64(session))
	p := alloc[C.WDBluetoothResult](1)
	p.status = status(err)
	p.devices, p.count = alloc[C.WDBluetoothDevice](len(devices)), C.size_t(len(devices))
	for i, d := range devices {
		unsafe.Slice(p.devices, len(devices))[i] = C.WDBluetoothDevice{name: C.CString(d.GetName()), address: C.CString(d.GetAddress()), device_type: C.CString(d.GetDeviceType()), rssi: C.int32_t(d.GetRssi()), paired: boolInt(d.GetPaired()), connected: boolInt(d.GetConnected()), trusted: boolInt(d.GetTrusted())}
	}
	return p
}

//export WDBluetoothFree
func WDBluetoothFree(p *C.WDBluetoothResult) {
	if p == nil {
		return
	}
	for _, d := range unsafe.Slice(p.devices, int(p.count)) {
		free(d.name)
		free(d.address)
		free(d.device_type)
	}
	free(p.devices)
	free(p.status.message)
	free(p)
}

//export WDBluetoothAction
func WDBluetoothAction(session C.uint64_t, address *C.char, action C.int32_t) *C.WDStatus {
	return statusResult(desktop.BluetoothAction(uint64(session), C.GoString(address), int32(action)))
}
