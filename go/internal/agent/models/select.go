package models

import (
	"slices"
	"strings"
)

// DeviceProfile is what variant selection knows about this device.
type DeviceProfile struct {
	Arch            string   // runtime.GOARCH, e.g. "arm64"
	GPUVendor       string   // e.g. "nvidia"; empty when there is no GPU
	GPUArch         string   // e.g. "sm_87"; empty when unknown
	ComputeBackends []string // GPU compute backends, e.g. ["cuda"]
	NPUBackends     []string // e.g. ["qnn"]
}

// SelectVariant returns the first variant, in catalog order, whose
// requirements the device meets. When none fits, the string says what each
// variant needed.
func SelectVariant(m Model, d DeviceProfile) (Variant, string, bool) {
	var needs []string
	for _, v := range m.Variants {
		unmet := v.Requires.unmet(d)
		if unmet == "" {
			return v, "", true
		}
		needs = append(needs, v.ID+" needs "+unmet)
	}
	if len(needs) == 0 {
		return Variant{}, "the model has no variants", false
	}
	return Variant{}, strings.Join(needs, "; "), false
}

func (r Requires) unmet(d DeviceProfile) string {
	switch {
	case r.Arch != "" && r.Arch != d.Arch:
		return "arch " + r.Arch
	case r.GPUVendor != "" && r.GPUVendor != d.GPUVendor:
		return "a " + r.GPUVendor + " GPU"
	case r.ComputeBackend != "" && !slices.Contains(d.ComputeBackends, r.ComputeBackend):
		return "GPU compute backend " + r.ComputeBackend
	case r.NPUBackend != "" && !slices.Contains(d.NPUBackends, r.NPUBackend):
		return "NPU backend " + r.NPUBackend
	}
	return ""
}
