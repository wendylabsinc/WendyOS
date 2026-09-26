package commands

import (
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// formatGPUCompute renders the per-GPU compute backends for `device info`.
// One GPU prints just its backends; more than one prints each GPU with its
// vendor and path so a mixed AMD+NVIDIA host reads unambiguously. Empty when no
// GPU reports one.
func formatGPUCompute(gpus []*agentpb.GpuCapabilities) string {
	if !slices.ContainsFunc(gpus, func(gpu *agentpb.GpuCapabilities) bool {
		return len(gpu.GetComputeBackends()) > 0
	}) {
		return ""
	}
	if len(gpus) == 1 {
		return strings.Join(gpus[0].GetComputeBackends(), ", ")
	}
	parts := make([]string, 0, len(gpus))
	for _, gpu := range gpus {
		backends := strings.Join(gpu.GetComputeBackends(), ", ")
		if backends == "" {
			backends = "none"
		}
		ident := gpu.GetVendor()
		if ident == "" {
			ident = "unknown"
		}
		if path := gpu.GetPath(); path != "" {
			ident += " " + path
		}
		parts = append(parts, backends+" ("+ident+")")
	}
	return strings.Join(parts, "; ")
}

// formatNPU renders the NPU vendor and the runtimes an app can use on it.
func formatNPU(vendor string, backends []string) string {
	if vendor == "" {
		vendor = "unknown"
	}
	if len(backends) == 0 {
		return vendor
	}
	return vendor + " (" + strings.Join(backends, ", ") + ")"
}

// gpuCapabilitiesJSON is the `device info --json` shape of the GPU list: one
// object per GPU with a non-null computeBackends array.
func gpuCapabilitiesJSON(gpus []*agentpb.GpuCapabilities) []map[string]any {
	out := make([]map[string]any, 0, len(gpus))
	for _, gpu := range gpus {
		out = append(out, map[string]any{
			"vendor":          gpu.GetVendor(),
			"path":            gpu.GetPath(),
			"computeBackends": append([]string{}, gpu.GetComputeBackends()...),
		})
	}
	return out
}
