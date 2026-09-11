package commands

import (
	"strings"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// formatGPUCompute renders the per-GPU compute backends for `device info`.
// One GPU prints just its backends ("cuda", or "none detected"); more than one
// prints each GPU with its vendor and path so a mixed AMD+NVIDIA host reads
// unambiguously. Empty when the agent sent no entries (an older agent).
func formatGPUCompute(gpus []*agentpb.GpuCapabilities) string {
	switch len(gpus) {
	case 0:
		return ""
	case 1:
		if backends := strings.Join(gpus[0].GetComputeBackends(), ", "); backends != "" {
			return backends
		}
		return "none detected"
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
