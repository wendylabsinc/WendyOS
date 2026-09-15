package services

import (
	"os"
	"path/filepath"
	"testing"
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

	oldGlob, oldRoot := fastrpcDeviceGlob, platformDevicesRoot
	t.Cleanup(func() { fastrpcDeviceGlob, platformDevicesRoot = oldGlob, oldRoot })
	fastrpcDeviceGlob = filepath.Join(devDir, "fastrpc-*")
	platformDevicesRoot = sysRoot
}

func TestDetectNPUInfo(t *testing.T) {
	for name, tc := range map[string]struct {
		nodes      []string
		device     string
		compatible string
		wantHas    bool
		wantVendor string
	}{
		"dragonwing": {
			[]string{"fastrpc-cdsp", "fastrpc-gdsp0", "fastrpc-cdsp-secure"},
			"26300000.remoteproc", "qcom,qcs8300-cdsp-pas\x00qcom,sa8775p-cdsp0-pas\x00",
			true, "qualcomm",
		},
		// The signed-PD nodes are root-only, so a board exposing nothing else has
		// no NPU an app can reach.
		"secure nodes only": {
			[]string{"fastrpc-adsp-secure", "fastrpc-cdsp-secure"},
			"26300000.remoteproc", "qcom,qcs8300-cdsp-pas\x00",
			false, "",
		},
		"no fastrpc nodes": {nil, "", "", false, ""},
		// The qcom entry is not always the first in the compatible list.
		"vendor entry not first": {
			[]string{"fastrpc-cdsp"}, "26300000.remoteproc",
			"generic,dsp\x00qcom,qcs8300-cdsp-pas\x00", true, "qualcomm",
		},
		// A DSP whose vendor we cannot name still counts as present; the vendor is
		// reported honestly as unknown rather than guessed.
		"unrecognised vendor": {
			[]string{"fastrpc-cdsp"}, "10000000.remoteproc", "acme,dsp\x00", true, "",
		},
		"nodes but no remoteproc": {
			[]string{"fastrpc-cdsp"}, "", "", true, "",
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
