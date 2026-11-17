package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "encoding/json"

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
