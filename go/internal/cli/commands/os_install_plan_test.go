//go:build darwin || linux || windows

package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func TestInstallPlanRoutesAndValidatesBeforeWriting(t *testing.T) {
	const release = "test-release"
	recovery := deviceVersion{InstallMode: "recovery", NVMEFlashpackPath: "recovery.tar.zst", NVMEFlashpackChecksum: "sha", NVMEFlashpackSizeBytes: 100, NVMERootfsOnlyPath: "rootfs.img", NVMERootfsOnlySizeBytes: 1024}
	for _, tc := range []struct {
		name            string
		opts            onboarding.Options
		artifact        deviceVersion
		method, wantErr string
	}{
		{name: "new Pi needs a target", opts: onboarding.Options{DeviceType: "raspberry-pi-5"}, artifact: deviceVersion{Path: "pi.img", SizeBytes: 1024}, method: "removable-media"},
		{name: "explicit Pi drive", opts: onboarding.Options{DeviceType: "raspberry-pi-5", Drive: "/dev/test-disk"}, artifact: deviceVersion{Path: "pi.img", SizeBytes: 1024}, method: "removable-media"},
		{name: "missing disk", opts: onboarding.Options{DeviceType: "raspberry-pi-5", Drive: "/dev/stale"}, artifact: deviceVersion{Path: "pi.img", SizeBytes: 1024}, wantErr: "relist drives"},
		{name: "undersized disk", opts: onboarding.Options{DeviceType: "raspberry-pi-5", Drive: "/dev/test-disk"}, artifact: deviceVersion{Path: "pi.img", SizeBytes: 8192}, wantErr: "too small"},
		{name: "Orin defaults to recovery", opts: onboarding.Options{DeviceType: orinNanoDeviceType, Carrier: "developer-kit"}, artifact: recovery, method: "usb-recovery"},
		{name: "Orin rootfs explicit", opts: onboarding.Options{DeviceType: orinNanoDeviceType, Carrier: "developer-kit", RootfsOnly: true, Drive: "/dev/test-disk"}, artifact: recovery, method: "rootfs-only"},
		{name: "no generic custom carrier image", opts: onboarding.Options{DeviceType: orinNanoDeviceType, Carrier: "custom"}, wantErr: "vendor"},
		{name: "no assumed carrier", opts: onboarding.Options{DeviceType: orinNanoDeviceType}, wantErr: "carrier"},
		{name: "no raw drive in recovery", opts: onboarding.Options{DeviceType: orinNanoDeviceType, Carrier: "developer-kit", Drive: "/dev/test-disk"}, artifact: recovery, wantErr: "not a host drive"},
		{name: "old Orin cannot silently downgrade mode", opts: onboarding.Options{DeviceType: orinNanoDeviceType, Carrier: "developer-kit"}, artifact: deviceVersion{Path: "legacy.img"}, wantErr: "no full recovery"},
		{name: "AGX storage required", opts: onboarding.Options{DeviceType: orinDeviceType, Carrier: "developer-kit"}, artifact: recovery, wantErr: "choose nvme or emmc"},
		{name: "Nano cannot choose emmc", opts: onboarding.Options{DeviceType: orinNanoDeviceType, Carrier: "developer-kit", Storage: "emmc"}, artifact: recovery, wantErr: "unsupported recovery storage"},
		{name: "G1 cannot request a flash", opts: onboarding.Options{DeviceType: "unitree-g1", Drive: "/dev/test-disk"}, wantErr: "agent installation"},
		{name: "unknown image mode", opts: onboarding.Options{DeviceType: "raspberry-pi-5"}, artifact: deviceVersion{InstallMode: "future"}, wantErr: "cannot perform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve := func(device, version string, nightly bool, pr int) (*deviceManifest, string, error) {
				return &deviceManifest{Versions: map[string]deviceVersion{release: tc.artifact}}, release, nil
			}
			drives := func() ([]onboarding.Drive, error) {
				return []onboarding.Drive{{ID: "/dev/test-disk", Name: "Test SD", Capacity: 4096, Removable: true}}, nil
			}
			p, err := buildInstallPlan(context.Background(), tc.opts, resolve, drives)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Method != tc.method || p.Version != release {
				t.Fatalf("unexpected plan: %+v", p)
			}
			if p.Method != "usb-recovery" && tc.opts.Drive == "" {
				if len(p.Command) != 0 {
					t.Fatal("plan supplied a write command without a target")
				}
			} else {
				command := strings.Join(p.Command, " ")
				if !strings.Contains(command, "--version "+release) || strings.Contains(command, "--force") {
					t.Fatalf("command not pinned or bypasses confirmations: %s", command)
				}
			}
		})
	}
}

func TestG1PlanNeverFetchesAnOSImageOrListsDrives(t *testing.T) {
	p, err := buildInstallPlan(context.Background(), onboarding.Options{DeviceType: "unitree-g1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Method != "agent" || len(p.Command) != 0 || p.ArtifactURL != "" {
		t.Fatalf("unsafe G1 plan: %+v", p)
	}
}

func TestInstallPlanSubcommandsAvailableOnBothAliases(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{{"install", "plan"}, {"install", "verify"}, {"os", "install", "plan"}, {"os", "install", "verify"}} {
		cmd, remaining, err := root.Find(path)
		if err != nil || len(remaining) != 0 || cmd.Name() != path[len(path)-1] {
			t.Fatalf("missing command %v: %v", path, err)
		}
	}
}
