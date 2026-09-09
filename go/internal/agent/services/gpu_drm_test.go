package services

import (
	"os"
	"path/filepath"
	"testing"
)

// drmTree builds a /sys/class/drm-shaped fixture: node name -> driver name.
// An empty driver means the node has no driver symlink, like a connector child.
func drmTree(t *testing.T, nodes map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for node, driver := range nodes {
		dev := filepath.Join(root, node, "device")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if driver == "" {
			continue
		}
		target := filepath.Join(root, "drivers", driver)
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dev, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDRMVendor(t *testing.T) {
	for name, tc := range map[string]struct {
		nodes map[string]string
		want  string
	}{
		// The real Dragonwing: display and GPU are one msm DRM device, and the
		// connector children carry no driver of their own.
		"dragonwing": {
			nodes: map[string]string{
				"card0":             "msm_dpu",
				"renderD128":        "msm_dpu",
				"card0-DP-1":        "",
				"card0-Writeback-1": "",
			},
			want: "qualcomm",
		},
		// A Raspberry Pi splits them: vc4 drives the display, v3d the GPU.
		// The render node must win, or the vendor would come from the display.
		"raspberry pi": {
			nodes: map[string]string{"card0": "vc4", "renderD128": "v3d"},
			want:  "broadcom",
		},
		"amd":          {nodes: map[string]string{"card0": "amdgpu", "renderD128": "amdgpu"}, want: "amd"},
		"intel":        {nodes: map[string]string{"card0": "i915", "renderD128": "i915"}, want: "intel"},
		"vm":           {nodes: map[string]string{"card0": "virtio_gpu", "renderD128": "virtio_gpu"}, want: "virtio"},
		"untested gpu": {nodes: map[string]string{"card0": "vmwgfx"}, want: ""},
		"unknown":      {nodes: map[string]string{"card0": "somethingnew"}, want: ""},
		"no driver":    {nodes: map[string]string{"card0": ""}, want: ""},
		"display only": {nodes: map[string]string{"card0": "vc4"}, want: "broadcom"},
	} {
		t.Run(name, func(t *testing.T) {
			old := drmSysfsRoot
			drmSysfsRoot = drmTree(t, tc.nodes)
			defer func() { drmSysfsRoot = old }()

			if got := drmVendor(); got != tc.want {
				t.Errorf("drmVendor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDRMVendorMissingSysfs(t *testing.T) {
	old := drmSysfsRoot
	drmSysfsRoot = filepath.Join(t.TempDir(), "absent")
	defer func() { drmSysfsRoot = old }()
	if got := drmVendor(); got != "" {
		t.Errorf("drmVendor() = %q on a host with no DRM, want empty", got)
	}
}

func TestDetectAdrenoArch(t *testing.T) {
	// The device-tree property is a NUL-separated string list, exactly as the
	// Dragonwing reports it.
	for name, tc := range map[string]struct {
		device     string
		compatible string
		want       string
	}{
		"adreno 623": {"3d00000.gpu", "qcom,adreno-623.0\x00qcom,adreno\x00", "a623"},
		"adreno 650": {"5000000.gpu", "qcom,adreno-650.2\x00qcom,adreno\x00", "a650"},
		// The GMU sits alongside the GPU and must not be mistaken for it.
		"not a gpu suffix": {"3d6a000.gmu", "qcom,adreno-gmu-623.0\x00", ""},
		"unrelated":        {"1000000.gpu", "arm,mali-t760\x00", ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, tc.device, "of_node")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "compatible"), []byte(tc.compatible), 0o644); err != nil {
				t.Fatal(err)
			}
			old := platformDevicesRoot
			platformDevicesRoot = root
			defer func() { platformDevicesRoot = old }()

			if got := detectAdrenoArch(); got != tc.want {
				t.Errorf("detectAdrenoArch() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectAdrenoArchMissingSysfs(t *testing.T) {
	old := platformDevicesRoot
	platformDevicesRoot = filepath.Join(t.TempDir(), "absent")
	defer func() { platformDevicesRoot = old }()
	if got := detectAdrenoArch(); got != "" {
		t.Errorf("detectAdrenoArch() = %q, want empty", got)
	}
}

func TestDRMDriverVendorsNeverNameNvidia(t *testing.T) {
	// Reaching the DRM branch means neither /etc/nv_tegra_release nor
	// /dev/nvidia0 exists, so the JetPack/CUDA/nvidia-smi probes keyed on
	// vendor=="nvidia" would report a runtime that is not installed.
	for driver, vendor := range drmDriverVendors {
		if vendor == "nvidia" {
			t.Errorf("%q maps to nvidia; the probes assume the proprietary stack", driver)
		}
	}
}

func TestDetectAdrenoArchIgnoresChipIDCompatible(t *testing.T) {
	// Some device trees name the GPU by chip id rather than version; parsing
	// "qcom,adreno-43050a01" as a version yields a nonsense "a43050".
	for name, tc := range map[string]struct{ compatible, want string }{
		"version form": {"qcom,adreno-623.0\x00qcom,adreno\x00", "a623"},
		"chip id form": {"qcom,adreno-43050a01\x00qcom,adreno\x00", ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "3d00000.gpu", "of_node")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "compatible"), []byte(tc.compatible), 0o644); err != nil {
				t.Fatal(err)
			}
			old := platformDevicesRoot
			platformDevicesRoot = root
			defer func() { platformDevicesRoot = old }()
			if got := detectAdrenoArch(); got != tc.want {
				t.Errorf("detectAdrenoArch() = %q, want %q", got, tc.want)
			}
		})
	}
}
