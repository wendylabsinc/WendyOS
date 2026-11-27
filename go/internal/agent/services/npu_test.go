package services

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// installFakeNPU points the FastRPC glob and the platform-device tree at fixtures.
// nodes are /dev/fastrpc-* basenames; compatible is the DSP remoteproc's device-tree
// value, NUL-separated exactly as the Dragonwing reports it.
func installFakeNPU(t *testing.T, nodes []string, device, compatible string) {
	t.Helper()
	devDir := t.TempDir()
	for _, n := range nodes {
		if err := os.WriteFile(filepath.Join(devDir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sysRoot := t.TempDir()
	if device != "" {
		dir := filepath.Join(sysRoot, device, "of_node")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "compatible"), []byte(compatible), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldGlob, oldRoot, oldAccel := fastrpcDeviceGlob, platformDevicesRoot, accelNPUs
	t.Cleanup(func() { fastrpcDeviceGlob, platformDevicesRoot, accelNPUs = oldGlob, oldRoot, oldAccel })
	fastrpcDeviceGlob = filepath.Join(devDir, "fastrpc-*")
	platformDevicesRoot = sysRoot
	accelNPUs = func() []oci.AccelNPU { return nil }
}

// The mainline Rockchip NPU is a DRM accel node whose parent, "rknn", no driver
// binds. Naming no runtime beats naming one this board has not been checked with.
func TestDetectNPUInfoRockchipAccel(t *testing.T) {
	root := t.TempDir()
	for name, parent := range map[string]string{
		"accel0": "devices/platform/rknn",
		"accel1": "devices/pci0000:00/0000:00:0b.0",
	} {
		minor := filepath.Join(root, "sys", parent, "accel", name)
		for _, dir := range []string{minor, filepath.Join(root, "sys/class/accel"), filepath.Join(root, "dev/accel")} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("../../../"+filepath.Base(parent), filepath.Join(minor, "device")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("../..", parent, "accel", name), filepath.Join(root, "sys/class/accel", name)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "dev/accel", name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installFakeNPU(t, nil, "", "")
	accelNPUs = func() []oci.AccelNPU { return oci.AccelNPUs(root) }
	got := detectNPUInfo()
	if !got.hasNPU || got.vendor != "rockchip" || got.backends != nil {
		t.Errorf("detectNPUInfo() = %+v, want a rockchip NPU with no named runtime", got)
	}

	// An accel node of a device the npu entitlement does not grant is not reported.
	if err := os.Remove(filepath.Join(root, "dev/accel/accel0")); err != nil {
		t.Fatal(err)
	}
	if got := detectNPUInfo(); got.hasNPU {
		t.Errorf("detectNPUInfo() = %+v, want no NPU", got)
	}
}

func TestDetectNPUInfo(t *testing.T) {
	for name, tc := range map[string]struct {
		nodes        []string
		device       string
		compatible   string
		wantHas      bool
		wantVendor   string
		wantBackends []string
	}{
		"dragonwing": {
			[]string{"fastrpc-adsp", "fastrpc-cdsp", "fastrpc-gdsp0", "fastrpc-cdsp-secure"},
			"26300000.remoteproc", "qcom,qcs8300-cdsp-pas\x00qcom,sa8775p-cdsp0-pas\x00",
			true, "qualcomm", []string{"qnn"},
		},
		// The signed-PD nodes are root-only, so a board exposing nothing else has
		// no NPU an app can reach.
		"secure nodes only": {
			[]string{"fastrpc-adsp-secure", "fastrpc-cdsp-secure"},
			"26300000.remoteproc", "qcom,qcs8300-cdsp-pas\x00",
			false, "", nil,
		},
		"no fastrpc nodes": {nil, "", "", false, "", nil},
		// An audio DSP's FastRPC node is not an NPU: the UNO Q exposes only this one.
		"audio dsp only": {
			[]string{"fastrpc-adsp"},
			"ab00000.remoteproc", "qcom,qcm2290-adsp-pas\x00qcom,sm6115-adsp-pas\x00",
			false, "", nil,
		},
		// The qcom entry is not always the first in the compatible list.
		"vendor entry not first": {
			[]string{"fastrpc-cdsp"}, "26300000.remoteproc",
			"generic,dsp\x00qcom,qcs8300-cdsp-pas\x00", true, "qualcomm", []string{"qnn"},
		},
		// A DSP whose vendor we cannot name still counts as present; the vendor is
		// reported honestly as unknown rather than guessed, and naming no runtime
		// beats naming the wrong one.
		"unrecognised vendor": {
			[]string{"fastrpc-cdsp"}, "10000000.remoteproc", "acme,dsp\x00", true, "", nil,
		},
		"nodes but no remoteproc": {
			[]string{"fastrpc-cdsp"}, "", "", true, "", nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			installFakeNPU(t, tc.nodes, tc.device, tc.compatible)
			got := detectNPUInfo()
			if got.hasNPU != tc.wantHas {
				t.Errorf("hasNPU = %v, want %v", got.hasNPU, tc.wantHas)
			}
			if got.vendor != tc.wantVendor {
				t.Errorf("vendor = %q, want %q", got.vendor, tc.wantVendor)
			}
			if !slices.Equal(got.backends, tc.wantBackends) {
				t.Errorf("backends = %v, want %v", got.backends, tc.wantBackends)
			}
		})
	}
}

func TestDetectNPUInfoMissingSysfs(t *testing.T) {
	installFakeNPU(t, []string{"fastrpc-cdsp"}, "", "")
	platformDevicesRoot = filepath.Join(t.TempDir(), "absent")
	if got := detectNPUInfo(); !got.hasNPU || got.vendor != "" {
		t.Errorf("detectNPUInfo() = %+v, want present with an empty vendor", got)
	}
}
