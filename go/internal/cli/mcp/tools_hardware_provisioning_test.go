package mcp

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeHWProvisioningOSServer implements hardware, provisioning, and OS methods
// of WendyAgentServiceServer and WendyProvisioningServiceServer.
type fakeHWProvisioningOSServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	agentpb.UnimplementedWendyProvisioningServiceServer
	capabilities  []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	isProvisioned *agentpb.IsProvisionedResponse
	osResponses   []*agentpb.UpdateOSResponse
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
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "hardware_capabilities", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	caps := listPayload(t, result, "capabilities")
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

func TestHardwareCapabilities_HasStructuredContent(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{
		capabilities: []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			{Category: "gpu", DevicePath: "/dev/gpu0", Description: "NVIDIA GPU"},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "hardware_capabilities", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if _, ok := structuredMap(t, result)["capabilities"]; !ok {
		t.Error("hardware_capabilities envelope is missing the capabilities key")
	}
}

func TestHardwareCapabilities_EmptyList(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "hardware_capabilities", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	// An empty result must still be an object envelope with the key present,
	// not a bare [] — conformant clients reject a non-object structuredContent.
	if caps := listPayload(t, result, "capabilities"); len(caps) != 0 {
		t.Errorf("expected no capabilities, got %v", caps)
	}
}

func TestHardwareCapabilities_MaxBytesTruncates(t *testing.T) {
	caps := make([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, 0, 200)
	for i := 0; i < 200; i++ {
		caps = append(caps, &agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    "gpu",
			DevicePath:  "/dev/gpu0",
			Description: "some padding description text",
		})
	}
	fake := &fakeHWProvisioningOSServer{capabilities: caps}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "hardware_capabilities", map[string]any{"max_bytes": 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("truncation is not an error result: %v", result.Content)
	}
	sc, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent has unexpected type %T", result.StructuredContent)
	}
	if sc["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", sc["truncated"])
	}
}

// --- Provisioning tests ---

func TestProvisioningStatus_NotConnected(t *testing.T) {
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
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
					CloudHost:      "cloud.wendy.dev",
					OrganizationId: 42,
					AssetId:        7,
				},
			},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
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
	if status["cloud_host"] != "cloud.wendy.dev" {
		t.Errorf("cloud_host = %v, want cloud.wendy.dev", status["cloud_host"])
	}
}

func TestProvisioningStatus_HasStructuredContent(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{
		isProvisioned: &agentpb.IsProvisionedResponse{
			Response: &agentpb.IsProvisionedResponse_Provisioned{
				Provisioned: &agentpb.ProvisionedResponse{
					CloudHost:      "cloud.wendy.dev",
					OrganizationId: 42,
					AssetId:        7,
				},
			},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "provisioning_status", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if result.StructuredContent == nil {
		t.Fatal("provisioning_status should return structuredContent")
	}
	sc, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent has unexpected type %T", result.StructuredContent)
	}
	if sc["provisioned"] != true {
		t.Errorf("provisioned = %v, want true", sc["provisioned"])
	}
	if sc["cloud_host"] != "cloud.wendy.dev" {
		t.Errorf("cloud_host = %v, want cloud.wendy.dev", sc["cloud_host"])
	}
}

func TestProvisioningStart_Success(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{
		isProvisioned: &agentpb.IsProvisionedResponse{
			Response: &agentpb.IsProvisionedResponse_Provisioned{
				Provisioned: &agentpb.ProvisionedResponse{
					CloudHost:      "cloud.wendy.dev",
					OrganizationId: 1,
				},
			},
		},
	}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "provisioning_start", map[string]any{
		"enrollment_token": "tok123",
		"cloud_host":       "cloud.wendy.dev",
		"organization_id":  float64(1),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(text, "cloud.wendy.dev") {
		t.Errorf("expected cloud host in result, got %q", text)
	}
}

func TestProvisioningStart_MissingRequired(t *testing.T) {
	fake := &fakeHWProvisioningOSServer{}
	conn := startFakeHWProvisioningServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
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

// --- OS Update tests ---

func TestOSUpdate_NotConnected(t *testing.T) {
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
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
