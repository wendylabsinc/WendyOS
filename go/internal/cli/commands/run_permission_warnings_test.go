package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func TestRuntimePermissionWarnings_UsesOnlyCameraAudioAndBluetoothEntitlements(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork},
			{Type: appconfig.EntitlementVideo},
			{Type: appconfig.EntitlementCamera},
			{Type: appconfig.EntitlementAudio},
			{Type: appconfig.EntitlementBluetooth},
		},
	}

	warnings := runtimePermissionWarnings(cfg, fakeRuntimePermissionChecker{
		statuses: map[runtimePermission]runtimePermissionStatus{
			runtimePermissionCamera:     runtimePermissionStatusMissing,
			runtimePermissionMicrophone: runtimePermissionStatusMissing,
			runtimePermissionBluetooth:  runtimePermissionStatusMissing,
		},
	})

	if len(warnings) != 3 {
		t.Fatalf("len(warnings) = %d, want 3", len(warnings))
	}
}

func TestRuntimePermissionWarnings_MapsAudioToMicrophone(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementAudio}},
	}

	warnings := runtimePermissionWarnings(cfg, fakeRuntimePermissionChecker{
		statuses: map[runtimePermission]runtimePermissionStatus{
			runtimePermissionMicrophone: runtimePermissionStatusMissing,
		},
	})

	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1", len(warnings))
	}
	want := "Warning: local microphone permission is missing. Apps that declare the audio entitlement may fail to access it. Run 'wendy-agent setup' to retry permission onboarding."
	if warnings[0] != want {
		t.Fatalf("warning = %q, want %q", warnings[0], want)
	}
}

func TestRuntimePermissionWarnings_SuppressesGrantedPermissions(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementCamera},
			{Type: appconfig.EntitlementAudio},
			{Type: appconfig.EntitlementBluetooth},
		},
	}

	warnings := runtimePermissionWarnings(cfg, fakeRuntimePermissionChecker{
		statuses: map[runtimePermission]runtimePermissionStatus{
			runtimePermissionCamera:     runtimePermissionStatusGranted,
			runtimePermissionMicrophone: runtimePermissionStatusGranted,
			runtimePermissionBluetooth:  runtimePermissionStatusGranted,
		},
	})

	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestRuntimePermissionWarnings_CoversMissingAndUnknownStates(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementCamera},
			{Type: appconfig.EntitlementAudio},
		},
	}

	warnings := runtimePermissionWarnings(cfg, fakeRuntimePermissionChecker{
		statuses: map[runtimePermission]runtimePermissionStatus{
			runtimePermissionCamera:     runtimePermissionStatusMissing,
			runtimePermissionMicrophone: runtimePermissionStatusUnknown,
		},
	})

	if len(warnings) != 2 {
		t.Fatalf("len(warnings) = %d, want 2", len(warnings))
	}
	if warnings[0] == warnings[1] {
		t.Fatalf("warnings should be permission-specific, got %v", warnings)
	}
}

func TestRuntimePermissionWarnings_WarnsOncePerPermission(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementAudio},
			{Type: appconfig.EntitlementAudio},
			{Type: appconfig.EntitlementCamera},
			{Type: appconfig.EntitlementCamera},
		},
	}

	warnings := runtimePermissionWarnings(cfg, fakeRuntimePermissionChecker{
		statuses: map[runtimePermission]runtimePermissionStatus{
			runtimePermissionCamera:     runtimePermissionStatusMissing,
			runtimePermissionMicrophone: runtimePermissionStatusMissing,
		},
	})

	if len(warnings) != 2 {
		t.Fatalf("len(warnings) = %d, want 2", len(warnings))
	}
}

type fakeRuntimePermissionChecker struct {
	statuses map[runtimePermission]runtimePermissionStatus
}

func (f fakeRuntimePermissionChecker) status(permission runtimePermission) runtimePermissionStatus {
	if status, ok := f.statuses[permission]; ok {
		return status
	}
	return runtimePermissionStatusUnknown
}
