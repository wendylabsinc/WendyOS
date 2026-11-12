//go:build darwin || linux

package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
)

func stubJetsonInstallAccess(t *testing.T) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	stubEnrollPrompts(t) // Name/enrollment must never be reached on USB failure.
	origScan, origRoot := listInstallRecoveryDevices, ensureThorInstallAccess
	origConfirm, origRead := installConfirmPrompt, readLineFn
	t.Cleanup(func() {
		listInstallRecoveryDevices, ensureThorInstallAccess = origScan, origRoot
		installConfirmPrompt, readLineFn = origConfirm, origRead
	})
	ensureThorInstallAccess = func() error { return nil }
	installConfirmPrompt = func(string) (bool, error) { return true, nil }
	readLineFn = func() string { return "q" }
	serveManifests(t, map[string]string{
		"manifests/master.json": `{"devices":{"jetson-agx-thor":{"latest":"1.0.0","manifest_path":"manifests/thor.json"}}}`,
		"manifests/thor.json":   `{"versions":{"1.0.0":{"flashpack_path":"thor/flashpack.tar.zst","flashpack_checksum":"abc123"}}}`,
	})
}

func runJetsonAccessInstall(device string, force bool, wifi wifiCLIOptions) error {
	if device == thorDeviceType {
		return installThor(context.Background(), "1.0.0", false, force, wifi, "", preEnrollOptions{}, 0, nil)
	}
	return installOrin(context.Background(), t234InstallOptions{
		DeviceType: device, DeviceName: device, Version: "1.0.0", Storage: "nvme",
		Artifact: &recoveryFlashpackInfo{}, Force: force, WiFi: wifi,
	})
}

func TestJetsonUSBAccessDeniedBeforeConfiguration(t *testing.T) {
	for _, device := range []string{orinNanoDeviceType, orinDeviceType, thorDeviceType} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%t", device, force), func(t *testing.T) {
				stubJetsonInstallAccess(t)
				keychain, prompts := stubWifiChoice(t, true, "")
				scans := 0
				listInstallRecoveryDevices = func() ([]rcm.RecoveryDevice, error) {
					scans++
					return nil, rcm.ErrUSBAccess
				}
				var err error
				out := captureStdout(t, func() {
					err = runJetsonAccessInstall(device, force, wifiCLIOptions{RequireChoice: true})
				})
				if err == nil || !strings.Contains(err.Error(), "permission denied") {
					t.Fatalf("install = %v; want USB permission failure", err)
				}
				if scans != 1 || !strings.Contains(out, "USB access denied") {
					t.Fatalf("scans = %d, output = %q; want denial guidance before quitting", scans, out)
				}
				if *keychain != 0 || *prompts != 0 {
					t.Fatalf("USB denial reached Wi-Fi: keychain=%d, prompts=%d", *keychain, *prompts)
				}
			})
		}
	}
}

func TestJetsonUSBScanFailureBeforeConfiguration(t *testing.T) {
	for _, device := range []string{orinNanoDeviceType, orinDeviceType, thorDeviceType} {
		t.Run(device, func(t *testing.T) {
			stubJetsonInstallAccess(t)
			_, prompts := stubWifiChoice(t, true, "")
			want := errors.New("USB enumeration failed")
			listInstallRecoveryDevices = func() ([]rcm.RecoveryDevice, error) { return nil, want }
			var err error
			_ = captureStdout(t, func() { err = runJetsonAccessInstall(device, true, wifiCLIOptions{RequireChoice: true}) })
			if !errors.Is(err, want) || *prompts != 0 {
				t.Fatalf("install = %v, Wi-Fi prompts = %d; want original scan error before config", err, *prompts)
			}
		})
	}
}

func TestJetsonConfigurationFollowsSuccessfulUSBSelection(t *testing.T) {
	for _, tc := range []struct {
		device  string
		product uint16
	}{
		{orinNanoDeviceType, rcm.ProductOrinNano8},
		{orinDeviceType, rcm.ProductOrinAGX32},
		{thorDeviceType, rcm.ProductThor},
	} {
		t.Run(tc.device, func(t *testing.T) {
			stubJetsonInstallAccess(t)
			stubWifiChoice(t, true, "")
			scanned := false
			listInstallRecoveryDevices = func() ([]rcm.RecoveryDevice, error) {
				scanned = true
				return []rcm.RecoveryDevice{{Product: tc.product, PathKey: "selected-port"}}, nil
			}
			want := errors.New("cancelled Wi-Fi setup")
			confirmWifiSetup = func() (bool, error) {
				if !scanned {
					t.Error("Wi-Fi question preceded USB access check")
				}
				return false, want
			}
			var err error
			_ = captureStdout(t, func() { err = runJetsonAccessInstall(tc.device, true, wifiCLIOptions{RequireChoice: true}) })
			if !errors.Is(err, want) || !scanned {
				t.Fatalf("install = %v, scanned = %t; want configuration after selection", err, scanned)
			}
		})
	}
}

func TestJetsonReadinessCancellationBeforeConfiguration(t *testing.T) {
	for _, device := range []string{orinNanoDeviceType, orinDeviceType, thorDeviceType} {
		t.Run(device, func(t *testing.T) {
			stubJetsonInstallAccess(t)
			_, prompts := stubWifiChoice(t, true, "")
			installConfirmPrompt = func(string) (bool, error) { return false, nil }
			listInstallRecoveryDevices = func() ([]rcm.RecoveryDevice, error) {
				t.Error("USB selection ran after readiness cancellation")
				return nil, errors.New("unexpected scan")
			}
			var err error
			_ = captureStdout(t, func() { err = runJetsonAccessInstall(device, false, wifiCLIOptions{RequireChoice: true}) })
			if !errors.Is(err, ErrUserCancelled) || *prompts != 0 {
				t.Fatalf("install = %v, Wi-Fi prompts = %d; want cancellation before config", err, *prompts)
			}
		})
	}
}
