package commands

import (
	"fmt"
	"sort"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

type runtimePermission string

type runtimePermissionStatus string

const (
	runtimePermissionCamera     runtimePermission = "camera"
	runtimePermissionMicrophone runtimePermission = "microphone"
	runtimePermissionBluetooth  runtimePermission = "bluetooth"
)

const (
	runtimePermissionStatusGranted runtimePermissionStatus = "granted"
	runtimePermissionStatusMissing runtimePermissionStatus = "missing"
	runtimePermissionStatusUnknown runtimePermissionStatus = "unknown"
)

type runtimePermissionChecker interface {
	status(permission runtimePermission) runtimePermissionStatus
}

func maybeWarnAboutRuntimePermissions(appCfg *appconfig.AppConfig) {
	for _, warning := range runtimePermissionWarnings(appCfg, newRuntimePermissionChecker()) {
		cliNotice("%s", warning)
	}
}

func runtimePermissionWarnings(appCfg *appconfig.AppConfig, checker runtimePermissionChecker) []string {
	var warnings []string
	for _, permission := range requiredRuntimePermissions(appCfg) {
		status := checker.status(permission)
		if status == runtimePermissionStatusGranted {
			continue
		}
		warnings = append(warnings, formatRuntimePermissionWarning(permission, status))
	}
	return warnings
}

func requiredRuntimePermissions(appCfg *appconfig.AppConfig) []runtimePermission {
	seen := map[runtimePermission]bool{}
	var permissions []runtimePermission
	add := func(permission runtimePermission) {
		if seen[permission] {
			return
		}
		seen[permission] = true
		permissions = append(permissions, permission)
	}

	for _, entitlement := range appCfg.Entitlements {
		switch entitlement.Type {
		case appconfig.EntitlementCamera:
			add(runtimePermissionCamera)
		case appconfig.EntitlementAudio:
			add(runtimePermissionMicrophone)
		case appconfig.EntitlementBluetooth:
			add(runtimePermissionBluetooth)
		}
	}

	sort.SliceStable(permissions, func(i, j int) bool {
		return runtimePermissionOrder(permissions[i]) < runtimePermissionOrder(permissions[j])
	})
	return permissions
}

func runtimePermissionOrder(permission runtimePermission) int {
	switch permission {
	case runtimePermissionCamera:
		return 0
	case runtimePermissionMicrophone:
		return 1
	case runtimePermissionBluetooth:
		return 2
	default:
		return 99
	}
}

func formatRuntimePermissionWarning(permission runtimePermission, status runtimePermissionStatus) string {
	friendlyName := string(permission)
	entitlement := string(permission)
	if permission == runtimePermissionMicrophone {
		entitlement = appconfig.EntitlementAudio
	}

	return fmt.Sprintf(
		"Warning: local %s permission is %s. Apps that declare the %s entitlement may fail to access it. Run 'wendy-agent setup' to retry permission onboarding.",
		friendlyName,
		status,
		entitlement,
	)
}
