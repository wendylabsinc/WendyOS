package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "unsafe"

//export WDDeviceEnrollment
func WDDeviceEnrollment(session C.uint64_t) *C.WDEnrollmentResult {
	value, err := desktop.DeviceEnrollment(uint64(session))
	p := alloc[C.WDEnrollmentResult](1)
	p.status = status(err)
	if value != nil {
		p.enrolled = boolInt(value.Enrolled)
		p.organization_id, p.organization_name = C.CString(value.OrganizationID), C.CString(value.OrganizationName)
		p.device_id = C.CString(value.DeviceID)
	}
	return p
}

//export WDEnrollmentFree
func WDEnrollmentFree(p *C.WDEnrollmentResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.organization_id)
	free(p.organization_name)
	free(p.device_id)
	free(p)
}

//export WDEnrollmentOrganizations
func WDEnrollmentOrganizations() *C.WDEnrollmentOrganizationsResult {
	values, err := desktop.EnrollmentOrganizations()
	p := alloc[C.WDEnrollmentOrganizationsResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDEnrollmentOrganization](len(values)), C.size_t(len(values))
	for i, value := range values {
		unsafe.Slice(p.items, len(values))[i] = C.WDEnrollmentOrganization{account: C.CString(value.Account), id: C.CString(value.ID), name: C.CString(value.Name), email: C.CString(value.Email)}
	}
	return p
}

//export WDEnrollmentOrganizationsFree
func WDEnrollmentOrganizationsFree(p *C.WDEnrollmentOrganizationsResult) {
	if p == nil {
		return
	}
	for _, value := range unsafe.Slice(p.items, int(p.count)) {
		free(value.account)
		free(value.id)
		free(value.name)
		free(value.email)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDEnrollDevice
func WDEnrollDevice(session C.uint64_t, account, organization, name *C.char) *C.WDStatus {
	return statusResult(desktop.EnrollDevice(uint64(session), C.GoString(account), C.GoString(organization), C.GoString(name)))
}
