//go:build darwin

package commands

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework AVFoundation -framework CoreBluetooth -framework Foundation
#include "run_permission_warnings_darwin.h"
*/
import "C"

type platformRuntimePermissionChecker struct{}

func newRuntimePermissionChecker() runtimePermissionChecker {
	return platformRuntimePermissionChecker{}
}

func (platformRuntimePermissionChecker) status(permission runtimePermission) runtimePermissionStatus {
	switch permission {
	case runtimePermissionCamera:
		return mapRuntimePermissionStatus(int(C.wendy_camera_permission_status()))
	case runtimePermissionMicrophone:
		return mapRuntimePermissionStatus(int(C.wendy_microphone_permission_status()))
	case runtimePermissionBluetooth:
		return mapRuntimePermissionStatus(int(C.wendy_bluetooth_permission_status()))
	default:
		return runtimePermissionStatusUnknown
	}
}

func mapRuntimePermissionStatus(code int) runtimePermissionStatus {
	switch code {
	case 0:
		return runtimePermissionStatusGranted
	case 1:
		return runtimePermissionStatusMissing
	default:
		return runtimePermissionStatusUnknown
	}
}
