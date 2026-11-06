package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"errors"
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"unsafe"
)

//export WDSharedLoginExport
func WDSharedLoginExport() *C.WDSharedLoginResult {
	data, err := desktop.ExportSharedLogin()
	p := alloc[C.WDSharedLoginResult](1)
	p.status = status(err)
	if err == nil {
		p.data = (*C.uint8_t)(C.CBytes(data))
		p.count = C.size_t(len(data))
	}
	return p
}

//export WDSharedLoginFree
func WDSharedLoginFree(p *C.WDSharedLoginResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	if p.data != nil {
		clear(unsafe.Slice((*byte)(unsafe.Pointer(p.data)), int(p.count)))
		free(p.data)
	}
	free(p)
}

//export WDSharedLoginImport
func WDSharedLoginImport(p *C.uint8_t, count C.size_t) *C.WDStatus {
	if p == nil || count == 0 || count > backend.MaxSharedLoginBytes {
		return statusResult(errors.New("invalid shared login size"))
	}
	return statusResult(desktop.ImportSharedLogin(C.GoBytes(unsafe.Pointer(p), C.int(count))))
}
