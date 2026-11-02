package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

//export WDOpenApp
func WDOpenApp(app *C.char, session C.uint64_t) *C.WDOpenAppResult {
	url, err := desktop.OpenApp(C.GoString(app), uint64(session))
	p := alloc[C.WDOpenAppResult](1)
	p.status = status(err)
	p.url = C.CString(url)
	return p
}

//export WDOpenAppFree
func WDOpenAppFree(p *C.WDOpenAppResult) {
	if p == nil {
		return
	}
	free(p.url)
	free(p.status.message)
	free(p)
}
