package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

//export WDDeviceInfo
func WDDeviceInfo(session C.uint64_t) *C.WDDeviceInfoResult {
	v, err := desktop.DeviceInfo(uint64(session))
	p := alloc[C.WDDeviceInfoResult](1)
	*p = C.WDDeviceInfoResult{status: status(err), version: C.CString(v.GetVersion()), os: C.CString(v.GetOs()), os_version: C.CString(v.GetOsVersion()), architecture: C.CString(v.GetCpuArchitecture()), board: C.CString(v.GetBoardModel()), serial: C.CString(v.GetSerialNumber()), kernel: C.CString(v.GetKernelVersion()), jetpack: C.CString(v.GetJetpackVersion()), cuda: C.CString(v.GetCudaVersion()), l4t: C.CString(v.GetL4TVersion()), uptime: C.uint64_t(v.GetUptimeSeconds()), memory: C.int64_t(v.GetMemTotalBytes()), cores: C.uint32_t(v.GetCpuCount())}
	return p
}

//export WDDeviceInfoFree
func WDDeviceInfoFree(p *C.WDDeviceInfoResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.version)
	free(p.os)
	free(p.os_version)
	free(p.architecture)
	free(p.board)
	free(p.serial)
	free(p.kernel)
	free(p.jetpack)
	free(p.cuda)
	free(p.l4t)
	free(p)
}

//export WDOnlineUpdate
func WDOnlineUpdate(session C.uint64_t, action C.int32_t, target *C.char) *C.WDOnlineUpdateResult {
	v, err := desktop.OnlineUpdate(uint64(session), int32(action), C.GoString(target))
	p := alloc[C.WDOnlineUpdateResult](1)
	*p = C.WDOnlineUpdateResult{status: status(err), installed: C.CString(v.GetInstalledVersion()), latest: C.CString(v.GetLatestVersion()), phase: C.CString(v.GetPhase()), detail: C.CString(v.GetDetail()), available: boolInt(v.GetUpdateAvailable()), downloaded: C.int64_t(v.GetDownloadedBytes()), total: C.int64_t(v.GetTotalBytes())}
	return p
}

//export WDOnlineUpdateFree
func WDOnlineUpdateFree(p *C.WDOnlineUpdateResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.installed)
	free(p.latest)
	free(p.phase)
	free(p.detail)
	free(p)
}
