package services

import (
	"context"
	"net"
	"runtime"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

func startDeviceInfoServer(t *testing.T, hd HardwareDiscoverer) (agentpbv2.WendyDeviceInfoServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize) // bufSize = 1024*1024, defined in agent_service_test.go
	srv := grpc.NewServer()
	svc := NewDeviceInfoService(zap.NewNop(), hd)
	agentpbv2.RegisterWendyDeviceInfoServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyDeviceInfoServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestDeviceInfoService_GetDeviceInfo(t *testing.T) {
	client, cleanup := startDeviceInfoServer(t, &mockHardwareDiscoverer{})
	defer cleanup()

	resp, err := client.GetDeviceInfo(context.Background(), &agentpbv2.GetDeviceInfoRequest{})
	if err != nil {
		t.Fatalf("GetDeviceInfo: %v", err)
	}
	if resp.Version != version.Version {
		t.Errorf("version = %q; want %q", resp.Version, version.Version)
	}
	if resp.Os != runtime.GOOS {
		t.Errorf("os = %q; want %q", resp.Os, runtime.GOOS)
	}
	if resp.CpuArchitecture != runtime.GOARCH {
		t.Errorf("arch = %q; want %q", resp.CpuArchitecture, runtime.GOARCH)
	}
}

func TestDeviceInfoService_ListHardwareCapabilities(t *testing.T) {
	caps := []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
		{Category: "gpu", DevicePath: "/dev/nvidia0", Description: "NVIDIA GPU"},
	}
	client, cleanup := startDeviceInfoServer(t, &mockHardwareDiscoverer{caps: caps})
	defer cleanup()

	resp, err := client.ListHardwareCapabilities(context.Background(), &agentpbv2.ListHardwareCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ListHardwareCapabilities: %v", err)
	}
	if len(resp.Capabilities) != 1 {
		t.Fatalf("len(capabilities) = %d; want 1", len(resp.Capabilities))
	}
	if resp.Capabilities[0].Category != "gpu" {
		t.Errorf("category = %q; want gpu", resp.Capabilities[0].Category)
	}
	if resp.Capabilities[0].DevicePath != "/dev/nvidia0" {
		t.Errorf("device_path = %q; want /dev/nvidia0", resp.Capabilities[0].DevicePath)
	}
}
