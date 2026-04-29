package mcp

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeHWProvisioningOSServer implements hardware, provisioning, and OS methods
// of WendyAgentServiceServer and WendyProvisioningServiceServer.
type fakeHWProvisioningOSServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	agentpb.UnimplementedWendyProvisioningServiceServer
	capabilities    []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	isProvisioned   *agentpb.IsProvisionedResponse
	osResponses     []*agentpb.UpdateOSResponse
}

func (s *fakeHWProvisioningOSServer) ListHardwareCapabilities(_ context.Context, _ *agentpb.ListHardwareCapabilitiesRequest) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	return &agentpb.ListHardwareCapabilitiesResponse{Capabilities: s.capabilities}, nil
}

func (s *fakeHWProvisioningOSServer) IsProvisioned(_ context.Context, _ *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	if s.isProvisioned != nil {
		return s.isProvisioned, nil
	}
	return &agentpb.IsProvisionedResponse{
		Response: &agentpb.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpb.NotProvisionedResponse{}},
	}, nil
}

func (s *fakeHWProvisioningOSServer) StartProvisioning(_ context.Context, _ *agentpb.StartProvisioningRequest) (*agentpb.StartProvisioningResponse, error) {
	return &agentpb.StartProvisioningResponse{}, nil
}

func (s *fakeHWProvisioningOSServer) UpdateOS(req *agentpb.UpdateOSRequest, stream grpc.ServerStreamingServer[agentpb.UpdateOSResponse]) error {
	for _, r := range s.osResponses {
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return nil
}

func startFakeHWProvisioningServer(t *testing.T, fake *fakeHWProvisioningOSServer) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(g, fake)
	agentpb.RegisterWendyProvisioningServiceServer(g, fake)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop() })

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &grpcclient.AgentConnection{
		Conn:                conn,
		AgentService:        agentpb.NewWendyAgentServiceClient(conn),
		ProvisioningService: agentpb.NewWendyProvisioningServiceClient(conn),
	}
}

// --- Hardware tests ---

func TestHardwareCapabilities_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "hardware_capabilities", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestHardwareCapabilities_ReturnsList(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{
		capabilities: []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			{Category: "gpu", DevicePath: "/dev/gpu0", Description: "NVIDIA GPU"},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "hardware_capabilities", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var caps []map[string]any
	if err := json.Unmarshal([]byte(text), &caps); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if len(caps) != 1 {
		t.Fatalf("expected 1 capability, got %d", len(caps))
	}
	if caps[0]["category"] != "gpu" {
		t.Errorf("category = %v, want gpu", caps[0]["category"])
	}
	if caps[0]["device_path"] != "/dev/gpu0" {
		t.Errorf("device_path = %v, want /dev/gpu0", caps[0]["device_path"])
	}
}

func TestHardwareCapabilities_EmptyList(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "hardware_capabilities", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "[]" {
		t.Errorf("expected empty JSON array, got %q", text)
	}
}

// --- Provisioning tests ---

func TestProvisioningStatus_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "provisioning_status", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestProvisioningStatus_NotProvisioned(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "provisioning_status", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var status map[string]any
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if status["provisioned"] != false {
		t.Errorf("provisioned = %v, want false", status["provisioned"])
	}
}

func TestProvisioningStatus_Provisioned(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{
		isProvisioned: &agentpb.IsProvisionedResponse{
			Response: &agentpb.IsProvisionedResponse_Provisioned{
				Provisioned: &agentpb.ProvisionedResponse{
					CloudHost:      "cloud.wendy.sh",
					OrganizationId: 42,
					AssetId:        7,
				},
			},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "provisioning_status", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var status map[string]any
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if status["provisioned"] != true {
		t.Errorf("provisioned = %v, want true", status["provisioned"])
	}
	if status["cloud_host"] != "cloud.wendy.sh" {
		t.Errorf("cloud_host = %v, want cloud.wendy.sh", status["cloud_host"])
	}
}

func TestProvisioningStart_Success(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "provisioning_start", map[string]any{
		"enrollment_token": "tok123",
		"cloud_host":       "cloud.wendy.sh",
		"organization_id":  float64(1),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(text, "cloud.wendy.sh") {
		t.Errorf("expected cloud host in result, got %q", text)
	}
}

func TestProvisioningStart_MissingRequired(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "provisioning_start", map[string]any{
		"enrollment_token": "tok123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when required fields are missing")
	}
}

// --- FileSync tests ---

func TestFileSyncSync_AlwaysReturnsError(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "filesync_sync", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true — filesync is unavailable via MCP")
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(text, "wendy run") {
		t.Errorf("expected CLI redirect hint in error, got %q", text)
	}
}

// --- OS Update tests ---

func TestOSUpdate_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "os_update", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestOSUpdate_StreamsProgress(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{
		osResponses: []*agentpb.UpdateOSResponse{
			{
				ResponseType: &agentpb.UpdateOSResponse_Progress_{
					Progress: &agentpb.UpdateOSResponse_Progress{Phase: "download", Percent: 50},
				},
			},
			{
				ResponseType: &agentpb.UpdateOSResponse_Completed_{
					Completed: &agentpb.UpdateOSResponse_Completed{RebootRequired: true},
				},
			},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "os_update", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(text, "download") {
		t.Errorf("expected phase 'download' in output, got %q", text)
	}
	if !strings.Contains(text, "50") {
		t.Errorf("expected percent '50' in output, got %q", text)
	}
	if !strings.Contains(text, "reboot required") {
		t.Errorf("expected 'reboot required' in output, got %q", text)
	}
}

func TestOSUpdate_Failed(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{
		osResponses: []*agentpb.UpdateOSResponse{
			{
				ResponseType: &agentpb.UpdateOSResponse_Failed_{
					Failed: &agentpb.UpdateOSResponse_Failed{ErrorMessage: "disk full"},
				},
			},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "os_update", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when update fails")
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(text, "disk full") {
		t.Errorf("expected error message in result, got %q", text)
	}
}
