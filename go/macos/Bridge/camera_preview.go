package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

//export WDPreviewCreate
func WDPreviewCreate() C.uint64_t { return C.uint64_t(desktop.PreviewCreate()) }

//export WDPreviewCameras
func WDPreviewCameras(id C.uint64_t, selector *C.char, secure C.int32_t) *C.WDCamerasResult {
	rows, err := desktop.PreviewCameras(uint64(id), C.GoString(selector), secure != 0)
	return cameras(rows, err)
}

//export WDPreviewPlay
func WDPreviewPlay(id C.uint64_t, stableID *C.char) *C.WDStatus {
	return statusResult(desktop.PreviewPlay(uint64(id), C.GoString(stableID)))
}

//export WDPreviewPoll
func WDPreviewPoll(id C.uint64_t) *C.WDEvents { return events(desktop.PreviewPoll(uint64(id))) }

//export WDPreviewCancel
func WDPreviewCancel(id C.uint64_t) { desktop.PreviewCancel(uint64(id)) }

//export WDPreviewClose
func WDPreviewClose(id C.uint64_t) { desktop.PreviewClose(uint64(id)) }

//export WDPreviewApps
func WDPreviewApps(id C.uint64_t) *C.WDAppsResult { return apps(desktop.PreviewApps(uint64(id))) }

//export WDPreviewOpenApp
func WDPreviewOpenApp(id C.uint64_t, app *C.char) *C.WDOpenAppResult {
	url, err := desktop.PreviewOpenApp(uint64(id), C.GoString(app))
	p := alloc[C.WDOpenAppResult](1)
	p.status = status(err)
	p.url = C.CString(url)
	return p
}
