//go:build !darwin

package commands

type platformRuntimePermissionChecker struct{}

func newRuntimePermissionChecker() runtimePermissionChecker {
	return platformRuntimePermissionChecker{}
}

func (platformRuntimePermissionChecker) status(runtimePermission) runtimePermissionStatus {
	return runtimePermissionStatusUnknown
}
