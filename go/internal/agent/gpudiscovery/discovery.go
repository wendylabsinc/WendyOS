// Package gpudiscovery shares host GPU evidence between metadata and inventory.
package gpudiscovery

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type Device struct {
	Path, Vendor, Driver string
	ComputeBackends      []string
}

func Host() []Device { return Discover("/") }

// Discover reads a supplied filesystem root, allowing deterministic sysfs/devfs
// fixtures. Device presence and supported host compute backends are independent.
func Discover(root string) []Device {
	path := func(p string) string { return filepath.Join(root, p) }
	exists := func(p string) bool { _, err := os.Stat(path(p)); return err == nil }
	read := func(p string) string { data, _ := os.ReadFile(path(p)); return strings.TrimSpace(string(data)) }
	glob := func(p string) []string { matches, _ := filepath.Glob(path(p)); return matches }
	var devices []Device
	seen := map[string]bool{}
	add := func(sysPath, devPath string) {
		identity, err := filepath.EvalSymlinks(path(sysPath))
		if err != nil {
			identity = path(sysPath)
		}
		if seen[identity] {
			return
		}
		seen[identity] = true
		driverPath, _ := os.Readlink(path(filepath.Join(sysPath, "driver")))
		driver := filepath.Base(driverPath)
		if driverPath == "" {
			driver = ""
		}
		vendor := vendorID(read(filepath.Join(sysPath, "vendor")))
		if vendor == "" {
			vendor = driverVendor(driver)
		}
		devices = append(devices, Device{Path: devPath, Vendor: vendor, Driver: driver})
	}
	entries, _ := os.ReadDir(path("/sys/class/drm"))
	for _, entry := range entries {
		name := entry.Name()
		if (strings.HasPrefix(name, "card") || strings.HasPrefix(name, "renderD")) && !strings.Contains(name, "-") {
			add("/sys/class/drm/"+name+"/device", "/dev/dri/"+name)
		}
	}
	entries, _ = os.ReadDir(path("/sys/bus/pci/devices"))
	for _, entry := range entries {
		sysPath := "/sys/bus/pci/devices/" + entry.Name()
		if strings.HasPrefix(read(sysPath+"/class"), "0x03") {
			add(sysPath, sysPath)
		}
	}
	hasVendor := func(vendor string) bool {
		return slices.ContainsFunc(devices, func(d Device) bool { return d.Vendor == vendor })
	}
	if !hasVendor("nvidia") && (exists("/etc/nv_tegra_release") || exists("/dev/nvidia0")) {
		devices = append(devices, Device{Path: "/dev/nvidia0", Vendor: "nvidia"})
	}
	if !hasVendor("amd") && exists("/dev/kfd") {
		devices = append(devices, Device{Path: "/dev/kfd", Vendor: "amd"})
	}
	if len(devices) == 0 {
		for _, node := range glob("/dev/dri/*") {
			if strings.HasPrefix(filepath.Base(node), "card") || strings.HasPrefix(filepath.Base(node), "renderD") {
				devices = append(devices, Device{Path: "/dev/dri/" + filepath.Base(node)})
			}
		}
	}
	// CUDA requires host driver evidence, not just a PCI vendor or toolkit.
	cuda := exists("/dev/nvidiactl") || exists("/dev/nvhost-gpu") || exists("/dev/nvhost-ctrl-gpu")
	for _, pattern := range []string{"/usr/lib*/libcuda.so*", "/usr/lib/*/libcuda.so*", "/usr/lib/*/tegra/libcuda.so*", "/usr/lib/*/nvidia*/libcuda.so*"} {
		cuda = cuda || len(glob(pattern)) > 0
	}
	// The Qualcomm Hexagon NPU (Dragonwing) is reached over FastRPC, and its
	// non-secure nodes are the app-usable transport: that is the driver-level
	// evidence for the qnn backend, the same bar /dev/nvidiactl sets for CUDA.
	// The root-only "-secure" nodes are the signed-PD path and prove nothing
	// about what an app can reach. The QNN runtime itself ships in the app
	// image, so no host library is required.
	qnn := slices.ContainsFunc(glob("/dev/fastrpc-*"), func(node string) bool {
		return !strings.HasSuffix(node, "-secure")
	})
	for i := range devices {
		switch devices[i].Vendor {
		case "nvidia":
			if cuda {
				devices[i].ComputeBackends = []string{"cuda"}
			}
		case "amd":
			if exists("/dev/kfd") {
				devices[i].ComputeBackends = []string{"rocm"}
			}
		case "qualcomm":
			if qnn {
				devices[i].ComputeBackends = []string{"qnn"}
			}
		}
	}
	return devices
}

func vendorID(id string) string {
	return map[string]string{"0x10de": "nvidia", "0x1002": "amd", "0x1022": "amd", "0x8086": "intel", "0x14e4": "broadcom", "0x13b5": "arm", "0x5143": "qualcomm"}[strings.ToLower(id)]
}

func driverVendor(driver string) string {
	switch strings.ToLower(driver) {
	case "nvidia", "nouveau", "nvgpu", "tegra":
		return "nvidia"
	case "amdgpu", "radeon":
		return "amd"
	case "i915", "xe":
		return "intel"
	case "vc4", "v3d":
		return "broadcom"
	case "panfrost", "panthor", "mali", "mali_kbase", "lima":
		return "arm"
	case "msm", "msm_dpu", "adreno", "kgsl":
		// Dragonwing IQ-8275 (CONFIG_DRM_MSM): display and GPU are one msm DRM
		// device, and the live board binds both nodes to msm_dpu.
		return "qualcomm"
	case "etnaviv", "galcore", "vivante":
		return "vivante"
	case "virtio_gpu":
		// QEMU ARM64 and both VM targets, whose virtual GPU is virtio. Named so
		// a VM reports what it has instead of an unknown vendor.
		return "virtio"
	}
	return ""
}

// Summary uses a compute-capable device as the legacy singular vendor hint,
// while retaining all detected backends for hosts with more than one GPU.
func Summary(devices []Device) (vendor string, backends []string) {
	backends = []string{}
	for _, d := range devices {
		if vendor == "" || len(d.ComputeBackends) > 0 {
			vendor = d.Vendor
		}
		for _, backend := range d.ComputeBackends {
			if !slices.Contains(backends, backend) {
				backends = append(backends, backend)
			}
		}
	}
	sort.Strings(backends)
	return vendor, backends
}
