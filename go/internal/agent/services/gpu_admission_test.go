package services

import (
	"context"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGPUAdmissionUnsupportedBackendFailsClosed(t *testing.T) {
	s := &ContainerService{containerd: &mockContainerdClient{}}
	got := s.gpuAdmission(context.Background(), "test.app")
	if got.Valid || got.UnavailableReason == "" || len(got.Devices) != 0 {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestGPUAdmissionInvalidAppRejected(t *testing.T) {
	s := &ContainerService{containerd: &mockContainerdClient{}, logger: zap.NewNop()}
	_, err := s.GetResourceStats(context.Background(), &agentpb.GetResourceStatsRequest{GpuAdmissionAppId: "../host"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}
