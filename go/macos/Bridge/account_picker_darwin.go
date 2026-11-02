//go:build darwin && !ios

package main

/*
#include "../Sources/Shared/AccountPickerTypes.h"
*/
import "C"
import (
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"unsafe"
)

//export WDAccountChoices
func WDAccountChoices() *C.WDAccountChoicesResult {
	p := alloc[C.WDAccountChoicesResult](1)
	entries, err := commands.DesktopAccountChoices()
	p.status = status(err)
	if err != nil {
		return p
	}
	p.items = alloc[C.WDAccountChoice](len(entries))
	p.count = C.size_t(len(entries))
	for i, e := range entries {
		v := &unsafe.Slice(p.items, len(entries))[i]
		v.context = C.CString(e.Context)
		v.email = C.CString(e.Email)
		v.cloud = C.CString(e.Cloud)
		v.organization_id = C.CString(e.OrganizationID)
		v.organization_name = C.CString(e.OrganizationName)
		v.state = C.WDAccountState(e.State)
		if e.Active {
			v.active = 1
		}
	}
	return p
}

//export WDAccountChoicesFree
func WDAccountChoicesFree(p *C.WDAccountChoicesResult) {
	if p == nil {
		return
	}
	for _, e := range unsafe.Slice(p.items, int(p.count)) {
		free(e.context)
		free(e.email)
		free(e.cloud)
		free(e.organization_id)
		free(e.organization_name)
	}
	free(p.status.message)
	free(p.items)
	free(p)
}

//export WDUseAccount
func WDUseAccount(name *C.char) *C.WDStatus {
	return statusResult(commands.DesktopUseAccount(C.GoString(name)))
}
