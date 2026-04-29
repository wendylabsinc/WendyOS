//go:build windows

package services

import (
	"context"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func collectCPUInfo(_ context.Context) *agentpb.GetSystemInfoResponse_CPUInfo {
	return newBaseCPUInfo()
}

func collectMemoryInfo(_ context.Context) *agentpb.GetSystemInfoResponse_MemoryInfo {
	return &agentpb.GetSystemInfoResponse_MemoryInfo{}
}

func collectDiskInfo(_ context.Context) []*agentpb.GetSystemInfoResponse_DiskInfo {
	return nil
}
