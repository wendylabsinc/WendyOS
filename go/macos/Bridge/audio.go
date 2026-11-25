package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "unsafe"

//export WDAudioDevices
func WDAudioDevices(session C.uint64_t) *C.WDAudioResult {
	devices, err := desktop.AudioDevices(uint64(session))
	p := alloc[C.WDAudioResult](1)
	p.status = status(err)
	p.devices, p.count = alloc[C.WDAudioDevice](len(devices)), C.size_t(len(devices))
	for i, d := range devices {
		unsafe.Slice(p.devices, len(devices))[i] = C.WDAudioDevice{id: C.uint32_t(d.GetDeviceId()), name: C.CString(d.GetName()), description: C.CString(d.GetDescription()), direction: C.int32_t(d.GetType()), is_default: boolInt(d.GetIsDefault())}
	}
	return p
}

//export WDAudioFree
func WDAudioFree(p *C.WDAudioResult) {
	if p == nil {
		return
	}
	for _, d := range unsafe.Slice(p.devices, int(p.count)) {
		free(d.name)
		free(d.description)
	}
	free(p.devices)
	free(p.status.message)
	free(p)
}

//export WDSetDefaultAudio
func WDSetDefaultAudio(session C.uint64_t, id C.uint32_t, direction C.int32_t) *C.WDStatus {
	return statusResult(desktop.SetDefaultAudio(uint64(session), uint32(id), int32(direction)))
}
