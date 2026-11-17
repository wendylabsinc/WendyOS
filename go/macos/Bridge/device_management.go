package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

//export WDManageDevice
func WDManageDevice(session C.uint64_t, account, organization, device, name *C.char, remove C.int32_t) *C.WDStatus {
	return statusResult(desktop.ManageDevice(uint64(session), C.GoString(account), C.GoString(organization), C.GoString(device), C.GoString(name), remove != 0))
}
