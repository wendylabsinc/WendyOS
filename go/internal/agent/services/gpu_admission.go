package services

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type appCgroupsProvider interface {
	AppCgroups(context.Context, string) ([]string, error)
}

func (s *ContainerService) gpuAdmission(ctx context.Context, appID string) *agentpb.GpuAdmissionStats {
	result := &agentpb.GpuAdmissionStats{}
	provider, ok := s.containerd.(appCgroupsProvider)
	if !ok {
		result.UnavailableReason = "app cgroup ownership unsupported"
		return result
	}
	groups, err := provider.AppCgroups(ctx, appID)
	if err != nil {
		result.UnavailableReason = "app cgroup ownership unavailable"
		return result
	}
	devices, err := hoststats.ReadGPUAdmission(ctx, groups)
	if err != nil {
		result.UnavailableReason = err.Error()
		return result
	}
	for _, g := range devices {
		result.Devices = append(result.Devices, &agentpb.GpuAdmissionDevice{
			Index: g.Index, Uuid: g.UUID, ForeignProcess: g.Foreign, Supported: g.Supported,
			UnifiedMemory: g.Unified, UtilizationPercent: g.Utilization, MemoryFreeBytes: g.FreeBytes,
		})
	}
	result.Valid = len(devices) > 0
	return result
}
