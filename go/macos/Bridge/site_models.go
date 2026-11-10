package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "encoding/json"

//export WDSiteModels
func WDSiteModels(session C.uint64_t, action, path, name, id *C.char) *C.WDSiteModelsResult {
	value, err := desktop.SiteModels(uint64(session), C.GoString(action), C.GoString(path), C.GoString(name), C.GoString(id))
	var data []byte
	if err == nil {
		data, err = json.Marshal(value)
	}
	p := alloc[C.WDSiteModelsResult](1)
	*p = C.WDSiteModelsResult{status: status(err), json: C.CString(string(data))}
	return p
}

//export WDSiteModelsFree
func WDSiteModelsFree(p *C.WDSiteModelsResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.json)
	free(p)
}
