//go:build darwin && !ios

package main

/*
#include <stdlib.h>
#include "../Sources/Shared/UnitreeTypes.h"
*/
import "C"
import (
	"unsafe"

	"github.com/wendylabsinc/wendy/go/macos/backend"
)

var unitreeAttempts backend.UnitreeAttempts

//export WDUnitreeAttemptCreate
func WDUnitreeAttemptCreate() C.uint64_t { return C.uint64_t(unitreeAttempts.New()) }

//export WDUnitreeAttemptCancel
func WDUnitreeAttemptCancel(attempt C.uint64_t) { unitreeAttempts.Cancel(uint64(attempt)) }

//export WDUnitreeAttemptFree
func WDUnitreeAttemptFree(attempt C.uint64_t) { unitreeAttempts.Release(uint64(attempt)) }

//export WDUnitreeRobots
func WDUnitreeRobots(session C.uint64_t) *C.WDUnitreeResult {
	p := alloc[C.WDUnitreeResult](1)
	result, err := desktop.UnitreePairings(uint64(session))
	p.status = status(err)
	p.warning = C.CString(result.Warning)
	p.count = C.size_t(len(result.Robots))
	p.items = alloc[C.WDUnitreeRobot](len(result.Robots))
	for i, r := range result.Robots {
		o := &unsafe.Slice(p.items, len(result.Robots))[i]
		o.serial = C.CString(r.Serial)
		o.name = C.CString(r.Name)
		o.address = C.CString(r.Address)
		if r.Paired {
			o.paired = 1
		}
		if r.Connected {
			o.connected = 1
		}
	}
	return p
}

//export WDUnitreeRobotsFree
func WDUnitreeRobotsFree(p *C.WDUnitreeResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.warning)
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.serial)
		free(r.name)
		free(r.address)
	}
	free(p.items)
	free(p)
}

//export WDUnitreePair
func WDUnitreePair(attempt, session C.uint64_t, serial, name, address, region, email, password *C.char) *C.WDStatus {
	return statusResult(desktop.PairUnitreeContext(unitreeAttempts.Context(uint64(attempt)), uint64(session), C.GoString(serial), C.GoString(name), C.GoString(address), C.GoString(region), C.GoString(email), C.GoString(password)))
}

//export WDUnitreeForget
func WDUnitreeForget(session C.uint64_t, serial *C.char) *C.WDStatus {
	return statusResult(desktop.ForgetUnitree(uint64(session), C.GoString(serial)))
}

//export WDUnitreeConnect
func WDUnitreeConnect(attempt C.uint64_t, serial, address, region, email, password *C.char) *C.WDUnitreeConnection {
	p := alloc[C.WDUnitreeConnection](1)
	ctx := unitreeAttempts.Context(uint64(attempt))
	handle, err := desktop.UnitreeControl.Connect(ctx, C.GoString(serial), C.GoString(address), C.GoString(region), C.GoString(email), C.GoString(password))
	p.status = status(err)
	p.handle = C.uint64_t(handle)
	return p
}

//export WDUnitreeConnectionFree
func WDUnitreeConnectionFree(p *C.WDUnitreeConnection) {
	if p != nil {
		free(p.status.message)
		free(p)
	}
}

//export WDUnitreeMove
func WDUnitreeMove(handle C.uint64_t, forward, lateral, yaw C.double) *C.WDStatus {
	return statusResult(desktop.UnitreeControl.Move(uint64(handle), float64(forward), float64(lateral), float64(yaw)))
}

//export WDUnitreeClose
func WDUnitreeClose(handle C.uint64_t) *C.WDStatus {
	return statusResult(desktop.UnitreeControl.Close(uint64(handle)))
}

//export WDUnitreeVideoStart
func WDUnitreeVideoStart(handle C.uint64_t) *C.WDStatus {
	return statusResult(desktop.UnitreeControl.StartVideo(uint64(handle)))
}

//export WDUnitreeVideoNext
func WDUnitreeVideoNext(handle C.uint64_t) *C.WDUnitreeVideoFrame {
	p := alloc[C.WDUnitreeVideoFrame](1)
	data, err := desktop.UnitreeControl.NextVideo(uint64(handle))
	p.status = status(err)
	if len(data) != 0 {
		p.bytes = (*C.uint8_t)(C.CBytes(data))
		p.count = C.size_t(len(data))
	}
	return p
}

//export WDUnitreeVideoFrameFree
func WDUnitreeVideoFrameFree(p *C.WDUnitreeVideoFrame) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.bytes)
	free(p)
}

//export WDUnitreeVideoStop
func WDUnitreeVideoStop(handle C.uint64_t) { desktop.UnitreeControl.StopVideo(uint64(handle)) }
