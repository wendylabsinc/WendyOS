// Package board centralizes detection of the physical SBC (Jetson, Raspberry
// Pi, or Generic) the agent is running on. Other packages that need to gate
// platform-specific behavior should call Detect() instead of replicating the
// underlying file-system probes.
//
// Existing inline call sites in agent_service.go and cdi/generate.go are not
// migrated by this package's introduction; that consolidation is a separate
// follow-up.
package board

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Kind identifies the host SBC.
type Kind int

const (
	Generic Kind = iota
	Jetson
	RaspberryPi
)

// Info bundles the detected board kind with any descriptive strings that
// happen to be cheap to read at detection time.
type Info struct {
	Kind         Kind
	Model        string   // /proc/device-tree/model when available
	Compatible   []string // /proc/device-tree/compatible, most specific first
	SerialNumber string   // /proc/device-tree/serial-number when available
	SoCFamily    string   // /sys/devices/soc0/family when available
}

// IsJetson reports whether the host is an NVIDIA Jetson.
func (i Info) IsJetson() bool { return i.Kind == Jetson }

// IsRaspberryPi reports whether the host is a Raspberry Pi.
func (i Info) IsRaspberryPi() bool { return i.Kind == RaspberryPi }

// Detection paths are package vars so tests can substitute them.
var (
	tegraReleasePath = "/etc/nv_tegra_release"
	socFamilyPath    = "/sys/devices/soc0/family"
	deviceTreeDir    = "/proc/device-tree"

	cached Info
	once   sync.Once
)

// Detect returns the host board info. The first call probes the filesystem;
// subsequent calls return the cached result. The result does not change at
// runtime.
func Detect() Info {
	once.Do(func() {
		cached = detect()
	})
	return cached
}

// resetForTest clears the cache. Test-only; not exported. Tests that call this
// must not run Detect() concurrently with the reset: replacing once is not
// synchronized, by design — detection is a cheap, deterministic filesystem
// probe, so tests reset-then-detect sequentially.
func resetForTest() {
	once = sync.Once{}
	cached = Info{}
}

func detect() Info {
	info := readDeviceTree(deviceTreeDir)
	if b, err := os.ReadFile(socFamilyPath); err == nil {
		info.SoCFamily = strings.TrimSpace(string(b))
	}
	if _, err := os.Stat(tegraReleasePath); err == nil {
		info.Kind = Jetson
		return info
	}
	if strings.Contains(strings.ToLower(info.SoCFamily), "tegra") {
		info.Kind = Jetson
		return info
	}
	if strings.Contains(info.Model, "Raspberry Pi") {
		info.Kind = RaspberryPi
		return info
	}
	info.Kind = Generic
	return info
}

// readDeviceTree reads the board model, compatible list and serial number from
// a device-tree directory. Device-tree string properties are NUL-terminated and
// a list property is NUL-separated. A missing file leaves its field empty; it is
// never an error, since x86 hosts and most VMs have no device tree at all.
func readDeviceTree(dir string) Info {
	prop := func(name string) []string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		var out []string
		for _, s := range strings.Split(string(b), "\x00") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	info := Info{Compatible: prop("compatible")}
	if m := prop("model"); len(m) > 0 {
		info.Model = m[0]
	}
	if s := prop("serial-number"); len(s) > 0 {
		info.SerialNumber = s[0]
	}
	return info
}

// nvidiaGPUArch maps a Jetson SoC's device-tree compatible to its CUDA compute
// capability, for when nvidia-smi is absent (it is not shipped on L4T). Source:
// https://developer.nvidia.com/cuda-gpus (Jetson T5000/T4000 = 11.0, Jetson
// AGX Orin/Orin NX/Orin Nano = 8.7) and
// https://developer.nvidia.com/cuda-legacy-gpus (Jetson AGX Xavier/Xavier NX
// = 7.2).
var nvidiaGPUArch = map[string]string{
	"nvidia,tegra264": "sm_110", // Thor
	"nvidia,tegra234": "sm_87",  // Orin
	"nvidia,tegra194": "sm_72",  // Xavier
}

// NvidiaGPUArch returns the CUDA architecture ("sm_87") of a known Jetson SoC
// named in the compatible list, or "" for any other board.
func (i Info) NvidiaGPUArch() string {
	for _, c := range i.Compatible {
		if arch, ok := nvidiaGPUArch[c]; ok {
			return arch
		}
	}
	return ""
}
