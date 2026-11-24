//go:build darwin || linux || windows

package commands

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

const orangePi5ProDeviceType = "orange-pi-5-pro"

func TestOrangePi5ProIsADiskImageBoard(t *testing.T) {
	// The picker names boards from the manifest key, the discover table from
	// deviceTypeNames; both must agree.
	if got := humanReadableDeviceType(orangePi5ProDeviceType); got != "Orange Pi 5 Pro" {
		t.Errorf("display name = %q", got)
	}
	if got := humanizeDeviceKey(orangePi5ProDeviceType); got != humanReadableDeviceType(orangePi5ProDeviceType) {
		t.Errorf("picker name %q differs from the display name", got)
	}
	if got := installDeviceArchitecture(orangePi5ProDeviceType); got != "arm64" {
		t.Errorf("architecture = %q, want arm64", got)
	}
	if isFlashBundleDeviceType(orangePi5ProDeviceType) || installedFromFlashBundle(deviceInfo{Key: orangePi5ProDeviceType}) {
		t.Error("the Orange Pi image is a plain disk image, not a flash bundle")
	}
	if installsFromFlashpack(orangePi5ProDeviceType, deviceVersion{}) {
		t.Error("the Orange Pi image must not take the Jetson flashpack path")
	}
	// The tour's substring heuristic would steer the user to an NVMe enclosure.
	if deviceUsesNVMe(orangePi5ProDeviceType) {
		t.Error("the tour treats the Orange Pi as an NVMe-only board")
	}
}

func TestOrangePi5ProInstallPlan(t *testing.T) {
	const release = "test-release"
	resolve := func(device, version string, nightly bool, pr int) (*deviceManifest, string, error) {
		return &deviceManifest{Versions: map[string]deviceVersion{release: {Path: "opi.wic", SizeBytes: 1024}}}, release, nil
	}
	drives := func() ([]onboarding.Drive, error) {
		return []onboarding.Drive{{ID: "/dev/test-disk", Name: "Test SD", Capacity: 4096, Removable: true}}, nil
	}
	p, err := buildInstallPlan(context.Background(), onboarding.Options{DeviceType: orangePi5ProDeviceType, Drive: "/dev/test-disk"}, resolve, drives)
	if err != nil {
		t.Fatal(err)
	}
	if p.Method != "removable-media" || p.Storage != "sd" {
		t.Errorf("method %q, storage %q; want removable-media on sd", p.Method, p.Storage)
	}
	if !strings.HasSuffix(p.Documentation, "/installation/wendyos-orange-pi-5-pro") {
		t.Errorf("documentation = %q, want the Orange Pi guide", p.Documentation)
	}
	want := []string{"wendy", "install", "--device-type", orangePi5ProDeviceType, "--version", release, "--storage", "sd", "--drive", "/dev/test-disk"}
	if !slices.Equal(p.Command, want) {
		t.Errorf("command = %q, want %q", p.Command, want)
	}
	if _, err := buildInstallPlan(context.Background(), onboarding.Options{DeviceType: orangePi5ProDeviceType, Carrier: "developer-kit"}, resolve, drives); err == nil {
		t.Error("a Jetson carrier was accepted for the Orange Pi")
	}
}
