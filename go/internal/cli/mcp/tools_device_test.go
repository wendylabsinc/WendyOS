package mcp

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
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
	text := result.Content[0].(mcpgo.TextContent).Text
	var devices []map[string]any
	if err := json.Unmarshal([]byte(text), &devices); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("expected at least one device")
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
