package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

//export WDPrepareLocalReset
func WDPrepareLocalReset(session C.uint64_t, account, organization, device *C.char) *C.WDLocalResetResult {
	plan, err := desktop.PrepareLocalReset(uint64(session), C.GoString(account), C.GoString(organization), C.GoString(device))
	p := alloc[C.WDLocalResetResult](1)
	p.status = status(err)
	p.principal = C.CString(plan.Principal)
	p.fingerprint = C.CString(plan.Fingerprint)
	return p
}

//export WDResetLocalEnrollment
func WDResetLocalEnrollment(session C.uint64_t, account, organization, device, principal, fingerprint *C.char) *C.WDLocalResetResult {
	completed, err := desktop.ResetLocalEnrollment(uint64(session), C.GoString(account), C.GoString(organization), C.GoString(device), C.GoString(principal), C.GoString(fingerprint))
	p := alloc[C.WDLocalResetResult](1)
	p.status = status(err)
	if completed {
		p.completed = 1
	}
	return p
}

//export WDLocalResetFree
func WDLocalResetFree(p *C.WDLocalResetResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.principal)
	free(p.fingerprint)
	free(p)
}
