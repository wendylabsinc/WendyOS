//go:build darwin || linux

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
)

func TestRecoveryFlagCombinationsFailBeforeManifestLookup(t *testing.T) {
	tests := [][]string{
		{"--device-type", orinDeviceType, "--drive", "/dev/disk9"},
		{"--device-type", orinNanoDeviceType, "--no-bmap"},
		{"--device-type", orinDeviceType, "--yes-overwrite-internal"},
		{"--device-type", orinDeviceType, "--rootfs-only", "--storage", "emmc"},
	}
	for _, args := range tests {
		cmd := newOSInstallCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("args %v unexpectedly accepted", args)
		}
	}
}

func TestChooseT234RecoveryStorage(t *testing.T) {
	if got, err := chooseT234RecoveryStorage(orinNanoDeviceType, ""); err != nil || got != "nvme" {
		t.Fatalf("Nano storage = %q, %v", got, err)
	}
	if _, err := chooseT234RecoveryStorage(orinNanoDeviceType, "emmc"); err == nil {
		t.Fatal("Nano eMMC recovery accepted")
	}
	if got, err := chooseT234RecoveryStorage(orinDeviceType, "emmc"); err != nil || got != "emmc" {
		t.Fatalf("AGX storage = %q, %v", got, err)
	}
}

func TestChooseT234RootfsOnlyStorage(t *testing.T) {
	tests := []struct {
		device, override, want string
		wantErr                bool
	}{
		{orinNanoDeviceType, "", "nvme", false},
		{orinNanoDeviceType, "sd", "sd", false},
		{orinNanoDeviceType, "nvme", "nvme", false},
		{orinDeviceType, "", "nvme", false},
		{orinDeviceType, "sd", "", true},
		{orinDeviceType, "emmc", "", true},
	}
	for _, tc := range tests {
		got, err := chooseT234RootfsOnlyStorage(tc.device, tc.override)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("chooseT234RootfsOnlyStorage(%q, %q) = %q, %v; want %q, err=%v", tc.device, tc.override, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestRootfsOnlyManifestDoesNotFallBackToLegacy(t *testing.T) {
	dm := &deviceManifest{Versions: map[string]deviceVersion{"1": {InstallMode: "recovery", Path: "legacy.img", NVMEPath: "legacy-nvme.img"}}}
	if _, err := getRootfsOnlyImageInfo(dm, "1", "nvme"); err == nil {
		t.Fatal("rootfs-only resolver used a legacy image field")
	}
	dm.Versions["1"] = deviceVersion{InstallMode: "recovery", NVMERootfsOnlyPath: "rootfs.img", NVMERootfsOnlySizeBytes: 12, NVMERootfsOnlyChecksum: "raw-sha", NVMERootfsOnlyZstChecksum: "compressed-sha", Checksum: "wrong-legacy-sha"}
	info, err := getRootfsOnlyImageInfo(dm, "1", "nvme")
	if err == nil && (info.Checksum != "raw-sha" || info.ZstChecksum != "compressed-sha") {
		t.Fatalf("rootfs-only checksums lost or taken from legacy artifact: %+v", info)
	}
	if err != nil || !strings.HasSuffix(info.DownloadURL, "/rootfs.img") {
		t.Fatalf("rootfs-only info = %+v, %v", info, err)
	}
}

func TestRecoveryManifestRoutesFlashpackByStorage(t *testing.T) {
	dm := &deviceManifest{Versions: map[string]deviceVersion{"1": {
		InstallMode:       "recovery",
		NVMEFlashpackPath: "nvme.flashpack", NVMEFlashpackChecksum: "n", NVMEFlashpackSizeBytes: 1,
		EMMCFlashpackPath: "emmc.flashpack", EMMCFlashpackChecksum: "e", EMMCFlashpackSizeBytes: 2,
	}}}
	nvme, err := getRecoveryFlashpackInfo(dm, orinDeviceType, "1", "nvme")
	if err != nil || !strings.HasSuffix(nvme.URL, "/nvme.flashpack") {
		t.Fatalf("NVMe flashpack = %+v, %v", nvme, err)
	}
	emmc, err := getRecoveryFlashpackInfo(dm, orinDeviceType, "1", "emmc")
	if err != nil || !strings.HasSuffix(emmc.URL, "/emmc.flashpack") {
		t.Fatalf("eMMC flashpack = %+v, %v", emmc, err)
	}
	dm.Versions["1"] = deviceVersion{EMMCFlashpackPath: "incomplete.flashpack"}
	if _, err := getRecoveryFlashpackInfo(dm, orinDeviceType, "1", "emmc"); err == nil {
		t.Fatal("incomplete eMMC flashpack metadata was accepted")
	}
}

func TestRecoverySelectorFiltersJetsonFamily(t *testing.T) {
	devs := []rcm.RecoveryDevice{
		{Product: rcm.ProductOrinAGX32},
		{Product: rcm.ProductOrinNano8},
		{Product: rcm.ProductThor},
	}
	thor := filterRecoveryDevices(devs, func(d rcm.RecoveryDevice) bool { return d.IsThor() })
	if len(thor) != 1 || !thor[0].IsThor() {
		t.Fatalf("Thor filter = %+v", thor)
	}
	// A jetson-orin-nano install must select only Nano modules, and a
	// jetson-agx-orin install only AGX modules — never each other's boards.
	nano := filterRecoveryDevices(devs, orinRecoveryMatch(orinNanoDeviceType))
	if len(nano) != 1 || !nano[0].IsOrinNano() {
		t.Fatalf("Nano filter = %+v", nano)
	}
	agx := filterRecoveryDevices(devs, orinRecoveryMatch(orinDeviceType))
	if len(agx) != 1 || !agx[0].IsOrinAGX() {
		t.Fatalf("AGX filter = %+v", agx)
	}
}

func TestPrepareT234WorkspaceLeavesCacheImmutable(t *testing.T) {
	root := t.TempDir()
	images := filepath.Join(root, "stage2", "flash")
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(images, "config.img")
	if err := os.WriteFile(config, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(images, "rootfs.img"), []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(images, "layout.xml"), []byte("layout"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp := &flashpack.Flashpack{Root: root}
	fp.Manifest.Layout.FlashImages = "stage2/flash"
	fp.Manifest.Layout.ConfigImage = "stage2/flash/config.img"
	fp.Manifest.Layout.PartitionLayout = "stage2/flash/layout.xml"
	workspace, _, err := prepareT234Workspace(fp)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workspace)
	if err := os.WriteFile(filepath.Join(workspace, "config.img"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(config)
	if string(data) != "original" {
		t.Fatalf("cached config was mutated: %q", data)
	}
}

// The Orin Nano devkit has no buttons — recovery mode is entered by jumpering
// FC REC to GND — so its guidance must never show AGX's button sequence (the
// text a Nano user actually saw in WDY-1888).
func TestOrinRecoveryGuidanceIsDeviceSpecific(t *testing.T) {
	nano := t234InstallOptions{DeviceType: orinNanoDeviceType, DeviceName: "Jetson Orin Nano", Storage: "nvme"}
	for name, text := range map[string]string{
		"wait hint": orinRecoveryHints(nano).buttonLine,
		"briefing":  orinRecoveryBriefingBox(nano),
	} {
		if strings.Contains(text, "Force Recovery") || strings.Contains(text, "Reset") {
			t.Errorf("Nano %s mentions buttons the devkit does not have: %q", name, text)
		}
		if !strings.Contains(text, "FC REC") || !strings.Contains(text, "GND") {
			t.Errorf("Nano %s omits the FC REC/GND jumper: %q", name, text)
		}
	}
	agx := t234InstallOptions{DeviceType: orinDeviceType, DeviceName: "Jetson AGX Orin", Storage: "nvme"}
	if bl := orinRecoveryHints(agx).buttonLine; !strings.Contains(bl, "Force Recovery") {
		t.Errorf("AGX wait hint lost its button sequence: %q", bl)
	}
	if b := orinRecoveryBriefingBox(agx); !strings.Contains(b, "Force Recovery") || strings.Contains(b, "FC REC") {
		t.Errorf("AGX briefing shows the wrong recovery entry: %q", b)
	}
}
