package services

import (
	"context"
	"runtime"
	"time"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// SystemInfoCollector gathers point-in-time resource information from the
// agent host.
type SystemInfoCollector interface {
	Collect(ctx context.Context) (*agentpb.GetSystemInfoResponse, error)
}

type defaultSystemInfoCollector struct{}

func newSystemInfoCollector() SystemInfoCollector {
	return defaultSystemInfoCollector{}
}

func (defaultSystemInfoCollector) Collect(ctx context.Context) (*agentpb.GetSystemInfoResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &agentpb.GetSystemInfoResponse{
		Cpu:                    collectCPUInfo(ctx),
		Memory:                 collectMemoryInfo(ctx),
		Disks:                  collectDiskInfo(ctx),
		CollectedAtUnixSeconds: time.Now().Unix(),
	}, nil
}

func newBaseCPUInfo() *agentpb.GetSystemInfoResponse_CPUInfo {
	return &agentpb.GetSystemInfoResponse_CPUInfo{
		Architecture: runtime.GOARCH,
		LogicalCores: uint32(runtime.NumCPU()),
	}
}
