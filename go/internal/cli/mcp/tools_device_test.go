package mcp

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

func TestDeviceList_ReturnsConfiguredDevices(t *testing.T) {
	cfg := &config.Config{
		DefaultDevice: "mydevice.local:50051",
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
	if len(devices) != 1 || devices[0]["address"] != cfg.DefaultDevice {
		t.Fatalf("configured device missing: %v", devices)
	}
	if warnings := structuredMap(t, result)["warnings"]; warnings != nil {
		t.Fatalf("local-only discovery should not require cloud login: %v", warnings)
	}
}

func TestDeviceList_SourceFieldOnConfigEntries(t *testing.T) {
	cfg := &config.Config{
		DefaultDevice: "mydevice.local:50051",
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
	srv := New(&config.Config{DefaultDevice: "some-long-device-hostname-for-padding.local:50051"}, nil)

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

func TestDeviceList_IncludesCloudDevicesByDefault(t *testing.T) {
	fake := &fakeCloudAssetServer{
		assets: []*cloudpb.Asset{
			{Id: 42, OrganizationId: 7, Name: "beecam", IsComputeDevice: true},
			{Id: 43, OrganizationId: 7, Name: "g1", IsComputeDevice: true},
		},
		pageSize: 1,
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		DefaultDevice: "local-device:50051",
		Auth:          []config.AuthConfig{{CloudGRPC: addr, Certificates: []config.CertificateInfo{{OrganizationID: 7}}}},
	}, nil)
	srv.discoverLANFn = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		t.Error("default device_list must not scan the LAN")
		return nil, nil
	}
	result, err := srv.callTool(context.Background(), "device_list", nil)
	if err != nil || result.IsError {
		t.Fatalf("device_list failed: %v, %v", result, err)
	}
	devices := listPayload(t, result, "devices")
	if len(devices) != 3 || devices[0]["address"] != "local-device:50051" {
		t.Fatalf("expected configured device and both cloud pages: %v", devices)
	}
	for i, device := range devices[1:] {
		if device["id"] != float64(42+i) || device["name"] != fake.assets[i].GetName() ||
			device["type"] != "cloud" || device["source"] != "cloud" || device["online"] != true ||
			device["cloud_grpc"] != addr || device["organization_id"] != float64(7) {
			t.Errorf("cloud entry missing identity or connection metadata: %v", device)
		}
		if _, exists := device["address"]; exists {
			t.Errorf("cloud control-plane endpoint must not be presented as a direct device address: %v", device)
		}
	}
	if len(fake.reqs) != 2 || !fake.req.GetOnlineOnly() || !fake.req.GetIsComputeDevice() {
		t.Fatalf("expected online compute devices from all pages: %v", fake.reqs)
	}
	if warnings := structuredMap(t, result)["warnings"]; warnings != nil {
		t.Fatalf("unexpected discovery warnings: %v", warnings)
	}
}

func TestDeviceList_CloudSessionSelection(t *testing.T) {
	first := &fakeCloudAssetServer{assets: []*cloudpb.Asset{{Id: 1, Name: "first"}}}
	second := &fakeCloudAssetServer{assets: []*cloudpb.Asset{{Id: 2, Name: "second"}}}
	firstAddr := startFakeCloudAssetServer(t, first)
	secondAddr := startFakeCloudAssetServer(t, second)
	cfg := &config.Config{
		DefaultCloudGRPC: secondAddr,
		Auth: []config.AuthConfig{
			{CloudGRPC: firstAddr, Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			{CloudGRPC: secondAddr, Certificates: []config.CertificateInfo{{OrganizationID: 2}}},
		},
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "selected session", want: "second"},
		{name: "explicit endpoint", args: map[string]any{"cloud_grpc": firstAddr}, want: "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := New(cfg, nil).callTool(context.Background(), "device_list", tc.args)
			if err != nil || result.IsError {
				t.Fatalf("device_list failed: %v, %v", result, err)
			}
			devices := listPayload(t, result, "devices")
			if len(devices) != 1 || devices[0]["name"] != tc.want {
				t.Fatalf("devices = %v; want %s from selected session", devices, tc.want)
			}
		})
	}
}

func TestDeviceList_CloudAuthFailurePreservesLocalDevices(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth []config.AuthConfig
		grpc string
		want string
	}{
		{name: "explicit cloud without login", grpc: "cloud.example:443", want: "wendy auth login"},
		{name: "missing credentials", auth: []config.AuthConfig{{CloudGRPC: "cloud.example:443"}}, want: "no certificates"},
		{name: "ambiguous sessions", auth: []config.AuthConfig{
			{CloudGRPC: "first.example:443", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			{CloudGRPC: "second.example:443", Certificates: []config.CertificateInfo{{OrganizationID: 2}}},
		}, want: "cloud_grpc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(&config.Config{DefaultDevice: "local-device:50051", Auth: tc.auth}, nil)
			srv.discoverLANFn = func(context.Context, time.Duration) ([]models.LANDevice, error) {
				return []models.LANDevice{{DisplayName: "lan-device", IPAddress: "192.0.2.1", Port: 50051}}, nil
			}
			result, err := srv.callTool(context.Background(), "device_list", map[string]any{"scan": true, "cloud_grpc": tc.grpc})
			if err != nil || result.IsError {
				t.Fatalf("cloud auth failure must not fail local discovery: %v, %v", result, err)
			}
			devices := listPayload(t, result, "devices")
			if len(devices) != 2 || devices[0]["address"] != "local-device:50051" || devices[1]["address"] != "192.0.2.1:50051" {
				t.Fatalf("expected configured and LAN devices without cloud endpoint placeholders: %v", devices)
			}
			warnings := listPayload(t, result, "warnings")
			if len(warnings) != 1 || warnings[0]["source"] != "cloud" || !strings.Contains(warnings[0]["message"].(string), tc.want) {
				t.Fatalf("expected actionable cloud warning containing %q: %v", tc.want, warnings)
			}
		})
	}
}

type unavailableDeviceListCloudServer struct {
	cloudpb.UnimplementedAssetServiceServer
	requestDeadline chan time.Time
	lanStarted      <-chan struct{}
}

func (s *unavailableDeviceListCloudServer) ListAssets(_ *cloudpb.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpb.ListAssetsResponse]) error {
	deadline, _ := stream.Context().Deadline()
	s.requestDeadline <- deadline
	select {
	case <-s.lanStarted:
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	return status.Error(codes.Unavailable, "cloud temporarily unavailable")
}

func TestDeviceList_CloudFailureIsBoundedAndConcurrentWithLAN(t *testing.T) {
	lanStarted := make(chan struct{})
	deadline := make(chan time.Time, 1)
	fake := &unavailableDeviceListCloudServer{requestDeadline: deadline, lanStarted: lanStarted}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	cloudpb.RegisterAssetServiceServer(g, fake)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	srv := New(&config.Config{
		DefaultDevice: "local-device:50051",
		Auth:          []config.AuthConfig{{CloudGRPC: ln.Addr().String(), Certificates: []config.CertificateInfo{{OrganizationID: 7}}}},
	}, nil)
	srv.discoverLANFn = func(ctx context.Context, _ time.Duration) ([]models.LANDevice, error) {
		select {
		case got := <-deadline:
			if got.IsZero() || time.Until(got) > 5*time.Second {
				t.Errorf("cloud request must have a deadline within 5s, got %v", got)
			}
		case <-ctx.Done():
			t.Error("cloud discovery did not run alongside LAN scan")
		}
		close(lanStarted)
		return []models.LANDevice{{DisplayName: "lan-device", Hostname: "lan-device.local", Port: 50051}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := srv.callTool(ctx, "device_list", map[string]any{"scan": true})
	if err != nil || result.IsError {
		t.Fatalf("cloud failure must not fail device discovery: %v, %v", result, err)
	}
	if devices := listPayload(t, result, "devices"); len(devices) != 2 {
		t.Fatalf("expected configured and LAN devices: %v", devices)
	}
	warnings := listPayload(t, result, "warnings")
	if len(warnings) != 1 || warnings[0]["source"] != "cloud" || !strings.Contains(warnings[0]["message"].(string), "cloud temporarily unavailable") {
		t.Fatalf("expected cloud service failure warning: %v", warnings)
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
