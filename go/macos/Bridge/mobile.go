package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

//export WDInitializeMobile
func WDInitializeMobile(directory *C.char) *C.WDStatus {
	if runtime.GOOS != "ios" {
		return statusResult(errors.New("mobile initialization requires iOS"))
	}
	path := C.GoString(directory)
	if !filepath.IsAbs(path) {
		return statusResult(errors.New("app configuration directory must be absolute"))
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return statusResult(err)
	}
	if err := os.Setenv("WENDY_CONFIG_DIR", path); err != nil {
		return statusResult(err)
	}
	return statusResult(desktop.Initialize(""))
}

//export WDSetMobileState
func WDSetMobileState(active C.int32_t, account, organization *C.char) *C.WDStatus {
	if runtime.GOOS != "ios" {
		return statusResult(errors.New("mobile lifecycle requires iOS"))
	}
	return statusResult(desktop.SetMobileState(active != 0, C.GoString(account), C.GoString(organization)))
}

//export WDMobileMeshSummary
func WDMobileMeshSummary() *C.WDMeshSummary {
	state, detail := desktop.MobileMeshSummary()
	p := alloc[C.WDMeshSummary](1)
	p.state, p.detail = C.CString(state), C.CString(detail)
	return p
}

//export WDMeshSummaryFree
func WDMeshSummaryFree(p *C.WDMeshSummary) {
	if p == nil {
		return
	}
	free(p.state)
	free(p.detail)
	free(p)
}
