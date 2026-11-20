package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "encoding/json"

//export WDOSRelease
func WDOSRelease(session C.uint64_t) *C.WDOSUpdateResult {
	value, err := desktop.CheckOSRelease(uint64(session))
	var data []byte
	if err == nil {
		data, err = json.Marshal(value)
	}
	p := alloc[C.WDOSUpdateResult](1)
	*p = C.WDOSUpdateResult{status: status(err), json: C.CString(string(data))}
	return p
}

//export WDOSUpdate
func WDOSUpdate(session C.uint64_t, artifact *C.char, start C.int32_t) *C.WDOSUpdateResult {
	value, err := desktop.MobileOSUpdate(uint64(session), C.GoString(artifact), start != 0)
	var data []byte
	if err == nil {
		data, err = json.Marshal(value)
	}
	p := alloc[C.WDOSUpdateResult](1)
	*p = C.WDOSUpdateResult{status: status(err), json: C.CString(string(data))}
	return p
}

//export WDOSUpdateFree
func WDOSUpdateFree(p *C.WDOSUpdateResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.json)
	free(p)
}

//export WDOSUpdateCancel
func WDOSUpdateCancel(session C.uint64_t) { desktop.CancelMobileOSUpdate(uint64(session)) }

//export WDOSRestart
func WDOSRestart(session C.uint64_t) *C.WDStatus {
	return statusResult(desktop.RestartAfterOSUpdate(uint64(session)))
}

//export WDOSUpdatePrepare
func WDOSUpdatePrepare(session C.uint64_t) { desktop.PrepareMobileOSUpdate(uint64(session)) }
