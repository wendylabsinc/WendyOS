package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"unsafe"
)

//export WDFleet
func WDFleet(account, org *C.char) *C.WDFleetResult {
	ctx, cancel := cloudContext()
	defer cancel()
	r, err := commands.DesktopFleetOverview(ctx, C.GoString(account), C.GoString(org))
	p := alloc[C.WDFleetResult](1)
	p.status = status(err)
	p.presence_error = C.CString(r.PresenceError)
	p.items, p.count = alloc[C.WDFleetDevice](len(r.Devices)), C.size_t(len(r.Devices))
	for i, d := range r.Devices {
		unsafe.Slice(p.items, len(r.Devices))[i] = C.WDFleetDevice{id: C.CString(d.ID), name: C.CString(d.Name), selector: C.CString(d.Selector), site_id: C.CString(d.SiteID), site: C.CString(d.Site), hardware: C.CString(d.Hardware), address: C.CString(d.Address), online: boolInt(d.Online), last_seen: C.int64_t(d.LastSeen)}
	}
	return p
}

//export WDFleetFree
func WDFleetFree(p *C.WDFleetResult) {
	if p == nil {
		return
	}
	for _, d := range unsafe.Slice(p.items, int(p.count)) {
		free(d.id)
		free(d.name)
		free(d.selector)
		free(d.site_id)
		free(d.site)
		free(d.hardware)
		free(d.address)
	}
	free(p.items)
	free(p.presence_error)
	free(p.status.message)
	free(p)
}
