package mcp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeAgentServer implements WendyAgentServiceServer for device tests.
type fakeAgentServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	versionResp *agentpb.GetAgentVersionResponse
	versionErr  error
}

func (s *fakeAgentServer) GetAgentVersion(ctx context.Context, req *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return s.versionResp, s.versionErr
}

func startFakeAgentServer(t *testing.T, srv *fakeAgentServer) (*grpcclient.AgentConnection, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(g, srv)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop() })
	addr := ln.Addr().String()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &grpcclient.AgentConnection{
		Conn:         conn,
		AgentService: agentpb.NewWendyAgentServiceClient(conn),
	}, addr
}

func TestDeviceInfo_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "device_info", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true when not connected")
	}
}

func TestDeviceInfo_ReturnsJSON(t *testing.T) {
	fake := &fakeAgentServer{
		versionResp: &agentpb.GetAgentVersionResponse{
			Version: "1.2.3",
			Os:      "linux",
		},
	}
	conn, _ := startFakeAgentServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "device_info", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if m["version"] != "1.2.3" {
		t.Errorf("version = %v, want 1.2.3", m["version"])
	}
}

func TestDeviceInfo_HasStructuredContent(t *testing.T) {
	fake := &fakeAgentServer{versionResp: &agentpb.GetAgentVersionResponse{Version: "9.9.9", Os: "linux"}}
	conn, _ := startFakeAgentServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)
	result, err := srv.callTool(context.Background(), "device_info", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.StructuredContent == nil {
		t.Fatal("device_info should return structuredContent")
	}
}

// TestDeviceInfo_ContainerStorageDegradedDerivedOnWendyOS asserts the MCP
// device_info tool derives container_storage_degraded the same way
// `device info --json` does (containerStorageDegraded(resp), explicit-or-
// derived) rather than only honouring an explicit field from newer agents
// (WDY-3127 M8).
func TestDeviceInfo_ContainerStorageDegradedDerivedOnWendyOS(t *testing.T) {
	osVersion := "WendyOS-1.0.0"
	fake := &fakeAgentServer{versionResp: &agentpb.GetAgentVersionResponse{
		Version:          "1.2.3",
		Os:               "linux",
		OsVersion:        &osVersion,
		ContainerStorage: &agentpb.DiskPartition{Mountpoint: "/", Device: "/dev/nvme0n1p1"},
		Partitions: []*agentpb.DiskPartition{
			{Mountpoint: "/", Device: "/dev/nvme0n1p1"},
			{Mountpoint: "/data", Device: "/dev/nvme0n1p2"},
		},
	}}
	conn, _ := startFakeAgentServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "device_info", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if got, ok := m["container_storage_degraded"].(bool); !ok || !got {
		t.Errorf("container_storage_degraded = %v, want true (derived: WendyOS, container storage on /, /data partition exists but isn't mounted there)", m["container_storage_degraded"])
	}
}

// TestDeviceInfo_ContainerStorageDegradedAbsentOffWendyOS asserts the field
// stays absent for a non-WendyOS agent (e.g. plain Ubuntu, where container
// storage on / is normal), matching `device info --json`'s
// isWendyOSAgent(resp) gate (WDY-3127 M8).
func TestDeviceInfo_ContainerStorageDegradedAbsentOffWendyOS(t *testing.T) {
	fake := &fakeAgentServer{versionResp: &agentpb.GetAgentVersionResponse{
		Version:          "1.2.3",
		Os:               "linux",
		ContainerStorage: &agentpb.DiskPartition{Mountpoint: "/", Device: "/dev/sda1"},
		Partitions: []*agentpb.DiskPartition{
			{Mountpoint: "/", Device: "/dev/sda1"},
			{Mountpoint: "/data", Device: "/dev/sda2"},
		},
	}}
	conn, _ := startFakeAgentServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "device_info", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if _, present := m["container_storage_degraded"]; present {
		t.Errorf("container_storage_degraded = %v, want absent on a non-WendyOS agent", m["container_storage_degraded"])
	}
}

func TestDeviceList_ReturnsConfiguredDevices(t *testing.T) {
	cfg := &config.Config{
		Auth: []config.AuthConfig{
			{CloudGRPC: "mydevice.local:50051"},
		},
	}
	srv := New(cfg, nil)
	result, err := srv.callTool(context.Background(), "device_list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result")
	}
	devices := listPayload(t, result, "devices")
	if len(devices) == 0 {
		t.Fatal("expected at least one device")
	}
}

func TestDeviceList_SourceFieldOnConfigEntries(t *testing.T) {
	cfg := &config.Config{
		Auth: []config.AuthConfig{
			{CloudGRPC: "mydevice.local:50051"},
		},
	}
	srv := New(cfg, nil)
	result, err := srv.callTool(context.Background(), "device_list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result")
	}
	devices := listPayload(t, result, "devices")
	for _, d := range devices {
		if d["source"] != "config" {
			t.Errorf("expected source=config, got %v (device: %v)", d["source"], d)
		}
	}
}

func TestDeviceList_ScanTrue_IncludesScanResults(t *testing.T) {
	cfg := &config.Config{}
	srv := New(cfg, nil)
	srv.discoverLANFn = func(_ context.Context, _ time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{
			{DisplayName: "test-device", Hostname: "test-device.local", Port: 50051, AgentVersion: "1.0.0"},
		}, nil
	}

	result, err := srv.callTool(context.Background(), "device_list", map[string]any{"scan": true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result")
	}
	devices := listPayload(t, result, "devices")
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0]["source"] != "scan" {
		t.Errorf("expected source=scan, got %v", devices[0]["source"])
	}
	if devices[0]["type"] != "lan" {
		t.Errorf("expected type=lan, got %v", devices[0]["type"])
	}
}

func TestDeviceList_MaxBytesTruncates(t *testing.T) {
	auth := make([]config.AuthConfig, 0, 50)
	for i := 0; i < 50; i++ {
		auth = append(auth, config.AuthConfig{CloudGRPC: "some-long-device-hostname-for-padding.local:50051"})
	}
	srv := New(&config.Config{Auth: auth}, nil)

	result, err := srv.callTool(context.Background(), "device_list", map[string]any{"max_bytes": 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("truncation is not an error result: %v", result.Content)
	}
	sc := structuredMap(t, result)
	if sc["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", sc["truncated"])
	}
}

func TestDeviceConnect_CallsConnectFn(t *testing.T) {
	fake := &fakeAgentServer{
		versionResp: &agentpb.GetAgentVersionResponse{Version: "1.0.0"},
	}
	conn, addr := startFakeAgentServer(t, fake)

	called := false
	connectFn := ConnectFunc(func(ctx context.Context, address string) (*grpcclient.AgentConnection, error) {
		called = true
		if address != addr {
			t.Errorf("connect called with %q, want %q", address, addr)
		}
		return &grpcclient.AgentConnection{
			Conn:         conn.Conn,
			AgentService: agentpb.NewWendyAgentServiceClient(conn.Conn),
		}, nil
	})

	srv := New(&config.Config{}, connectFn)
	result, err := srv.callTool(context.Background(), "device_connect", map[string]any{"address": addr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if !called {
		t.Fatal("connectFn was not called")
	}
}
