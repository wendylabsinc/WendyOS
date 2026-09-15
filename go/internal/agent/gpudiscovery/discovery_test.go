package gpudiscovery

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFixture(t *testing.T, root, path, text string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverVendorsAndCompute(t *testing.T) {
	for _, tc := range []struct {
		vendor, id, driver, evidence string
		backends                     []string
	}{
		{"broadcom", "", "v3d", "", []string{}},
		{"broadcom", "", "vc4", "", []string{}},
		{"arm", "", "panfrost", "", []string{}},
		{"qualcomm", "", "msm", "", []string{}},
		// Dragonwing: the Hexagon NPU is reachable once a non-secure FastRPC
		// node exists; the root-only -secure node alone proves nothing.
		{"qualcomm", "", "msm_dpu", "/dev/fastrpc-cdsp", []string{"qnn"}},
		{"qualcomm", "", "kgsl", "/dev/fastrpc-cdsp-secure", []string{}},
		{"vivante", "", "etnaviv", "", []string{}},
		{"intel", "0x8086", "i915", "", []string{}},
		{"nvidia", "0x10de", "nvidia", "/dev/nvidiactl", []string{"cuda"}},
		{"nvidia", "0x10de", "nouveau", "", []string{}},
		{"amd", "0x1002", "amdgpu", "/dev/kfd", []string{"rocm"}},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "/sys/class/drm/card0/device/vendor", tc.id)
			if err := os.Symlink("/sys/bus/platform/drivers/"+tc.driver, filepath.Join(root, "sys/class/drm/card0/device/driver")); err != nil {
				t.Fatal(err)
			}
			if tc.evidence != "" {
				writeFixture(t, root, tc.evidence, "")
			}
			devices := Discover(root)
			vendor, backends := Summary(devices)
			if len(devices) != 1 || vendor != tc.vendor || !reflect.DeepEqual(backends, tc.backends) {
				t.Fatalf("discovered %+v, summary %s %v; want %s %v", devices, vendor, backends, tc.vendor, tc.backends)
			}
		})
	}
}

func TestDiscoverPCIWithoutDRMAndNoFalseCUDA(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "/sys/bus/pci/devices/0000:01:00.0/class", "0x030200")
	writeFixture(t, root, "/sys/bus/pci/devices/0000:01:00.0/vendor", "0x10de")
	writeFixture(t, root, "/usr/local/cuda/version.txt", "CUDA 13.0")
	devices := Discover(root)
	if len(devices) != 1 || devices[0].Vendor != "nvidia" || len(devices[0].ComputeBackends) != 0 {
		t.Fatalf("PCI hardware/toolkit should not imply a working CUDA driver: %+v", devices)
	}
	if devices := Discover(t.TempDir()); len(devices) != 0 {
		t.Fatalf("empty host: %+v", devices)
	}
}

// drmTree lays out /sys/class/drm-shaped nodes under root: node name -> driver
// name. An empty driver means the node has no driver symlink, like a connector
// child. Each node gets its own device directory, as sysfs presents them.
func drmTree(t *testing.T, root string, nodes map[string]string) {
	t.Helper()
	for node, driver := range nodes {
		dev := filepath.Join(root, "sys/class/drm", node, "device")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if driver == "" {
			continue
		}
		target := filepath.Join(root, "sys/bus/platform/drivers", driver)
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dev, "driver")); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDiscoverDRMDriverVendors covers the boards WendyOS ships, laid out as
// their live sysfs trees. An untested driver reports a present device with an
// honest empty vendor rather than a guess.
func TestDiscoverDRMDriverVendors(t *testing.T) {
	for name, tc := range map[string]struct {
		nodes   map[string]string
		vendor  string
		devices int
	}{
		// The real Dragonwing IQ-8275: display and GPU are one msm DRM device,
		// both nodes bind to msm_dpu, and the connector children carry no
		// driver of their own.
		"dragonwing": {
			nodes: map[string]string{
				"card0":             "msm_dpu",
				"renderD128":        "msm_dpu",
				"card0-DP-1":        "",
				"card0-Writeback-1": "",
			},
			vendor: "qualcomm", devices: 2,
		},
		// A Raspberry Pi splits them: vc4 drives the display, v3d the GPU.
		"raspberry pi": {nodes: map[string]string{"card0": "vc4", "renderD128": "v3d"}, vendor: "broadcom", devices: 2},
		"display only": {nodes: map[string]string{"card0": "vc4"}, vendor: "broadcom", devices: 1},
		"amd":          {nodes: map[string]string{"card0": "amdgpu", "renderD128": "amdgpu"}, vendor: "amd", devices: 2},
		"intel":        {nodes: map[string]string{"card0": "i915", "renderD128": "i915"}, vendor: "intel", devices: 2},
		// QEMU ARM64 and both VM targets, whose virtual GPU is virtio.
		"vm":           {nodes: map[string]string{"card0": "virtio_gpu", "renderD128": "virtio_gpu"}, vendor: "virtio", devices: 2},
		"untested gpu": {nodes: map[string]string{"card0": "vmwgfx"}, vendor: "", devices: 1},
		"no driver":    {nodes: map[string]string{"card0": ""}, vendor: "", devices: 1},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			drmTree(t, root, tc.nodes)
			devices := Discover(root)
			vendor, backends := Summary(devices)
			if len(devices) != tc.devices || vendor != tc.vendor || len(backends) != 0 {
				t.Fatalf("discovered %+v, summary %q %v; want %d devices, vendor %q, no backends", devices, vendor, backends, tc.devices, tc.vendor)
			}
		})
	}
}
