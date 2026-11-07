package main

/*
#include "../Sources/Shared/Bridge.h"
#include <stdlib.h>
*/
import "C"
import "unsafe"

//export WDCameraControls
func WDCameraControls(session C.uint64_t, camera C.uint32_t, stableID, name *C.char, value C.int32_t) *C.WDCameraControlsResult {
	controls, err := desktop.CameraControls(uint64(session), uint32(camera), C.GoString(stableID), C.GoString(name), int32(value))
	p := alloc[C.WDCameraControlsResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDCameraControl](len(controls)), C.size_t(len(controls))
	for i, c := range controls {
		unsafe.Slice(p.items, len(controls))[i] = C.WDCameraControl{name: C.CString(c.Name), value: C.int32_t(c.Value), minimum: C.int32_t(c.Minimum), maximum: C.int32_t(c.Maximum), step: C.int32_t(c.Step), mutable: boolInt(c.Mutable)}
	}
	return p
}

//export WDCameraControlsFree
func WDCameraControlsFree(p *C.WDCameraControlsResult) {
	if p == nil {
		return
	}
	for _, c := range unsafe.Slice(p.items, int(p.count)) {
		C.free(unsafe.Pointer(c.name))
	}
	C.free(unsafe.Pointer(p.items))
	C.free(unsafe.Pointer(p.status.message))
	C.free(unsafe.Pointer(p))
}
