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

// fakeWiFiBluetoothServer implements the WiFi and Bluetooth methods of
// WendyAgentServiceServer for tests.
type fakeWiFiBluetoothServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	wifiNetworks      []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	knownNetworks     []*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork
	wifiConnectResp   *agentpb.ConnectToWiFiResponse
	wifiStatusResp    *agentpb.GetWiFiStatusResponse
	wifiDisconnectErr error
	btPeripherals     []*agentpb.DiscoveredBluetoothPeripheral
}

func (s *fakeWiFiBluetoothServer) ListWiFiNetworks(_ context.Context, _ *agentpb.ListWiFiNetworksRequest) (*agentpb.ListWiFiNetworksResponse, error) {
	return &agentpb.ListWiFiNetworksResponse{Networks: s.wifiNetworks}, nil
}

func (s *fakeWiFiBluetoothServer) ConnectToWiFi(_ context.Context, _ *agentpb.ConnectToWiFiRequest) (*agentpb.ConnectToWiFiResponse, error) {
	if s.wifiConnectResp != nil {
		return s.wifiConnectResp, nil
	}
	return &agentpb.ConnectToWiFiResponse{Success: true}, nil
}

func (s *fakeWiFiBluetoothServer) GetWiFiStatus(_ context.Context, _ *agentpb.GetWiFiStatusRequest) (*agentpb.GetWiFiStatusResponse, error) {
	if s.wifiStatusResp != nil {
		return s.wifiStatusResp, nil
	}
	return &agentpb.GetWiFiStatusResponse{Connected: false}, nil
}

func (s *fakeWiFiBluetoothServer) DisconnectWiFi(_ context.Context, _ *agentpb.DisconnectWiFiRequest) (*agentpb.DisconnectWiFiResponse, error) {
	return &agentpb.DisconnectWiFiResponse{Success: s.wifiDisconnectErr == nil}, nil
}

func (s *fakeWiFiBluetoothServer) ListKnownWiFiNetworks(_ context.Context, _ *agentpb.ListKnownWiFiNetworksRequest) (*agentpb.ListKnownWiFiNetworksResponse, error) {
	return &agentpb.ListKnownWiFiNetworksResponse{Networks: s.knownNetworks}, nil
}

func (s *fakeWiFiBluetoothServer) ScanBluetoothPeripherals(stream agentpb.WendyAgentService_ScanBluetoothPeripheralsServer) error {
	_, _ = stream.Recv()
	if len(s.btPeripherals) > 0 {
		_ = stream.Send(&agentpb.ScanBluetoothPeripheralsResponse{
			DiscoveredDevices: s.btPeripherals,
		})
	}
	return nil
}

func (s *fakeWiFiBluetoothServer) ConnectBluetoothPeripheral(_ context.Context, _ *agentpb.ConnectBluetoothPeripheralRequest) (*agentpb.ConnectBluetoothPeripheralResponse, error) {
	return &agentpb.ConnectBluetoothPeripheralResponse{}, nil
}

func (s *fakeWiFiBluetoothServer) DisconnectBluetoothPeripheral(_ context.Context, _ *agentpb.DisconnectBluetoothPeripheralRequest) (*agentpb.DisconnectBluetoothPeripheralResponse, error) {
	return &agentpb.DisconnectBluetoothPeripheralResponse{}, nil
}

func startFakeAgentWiFiServer(t *testing.T, fake *fakeWiFiBluetoothServer) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(g, fake)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop() })

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &grpcclient.AgentConnection{
		Conn:         conn,
		AgentService: agentpb.NewWendyAgentServiceClient(conn),
	}
}

func TestWiFiList_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "wifi_list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestWiFiList_ReturnsNetworks(t *testing.T) {
	fake := &fakeWiFiBluetoothServer{
		wifiNetworks: []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
			{Ssid: "MyNetwork", IsConnected: true},
		},
	}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "wifi_list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var networks []map[string]any
	if err := json.Unmarshal([]byte(text), &networks); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if len(networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(networks))
	}
	if networks[0]["ssid"] != "MyNetwork" {
		t.Errorf("ssid = %v, want MyNetwork", networks[0]["ssid"])
	}
}

func TestWiFiConnect_Success(t *testing.T) {
	fake := &fakeWiFiBluetoothServer{
		wifiConnectResp: &agentpb.ConnectToWiFiResponse{Success: true},
	}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "wifi_connect", map[string]any{
		"ssid":     "MyNetwork",
		"password": "secret",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "connected to MyNetwork" {
		t.Errorf("text = %q, want %q", text, "connected to MyNetwork")
	}
}

func TestWiFiStatus_ReturnsJSON(t *testing.T) {
	ssid := "Home"
	fake := &fakeWiFiBluetoothServer{
		wifiStatusResp: &agentpb.GetWiFiStatusResponse{
			Connected: true,
			Ssid:      &ssid,
		},
	}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "wifi_status", nil)
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
	if status["ssid"] != "Home" {
		t.Errorf("ssid = %v, want Home", status["ssid"])
	}
	if status["connected"] != true {
		t.Errorf("connected = %v, want true", status["connected"])
	}
}

func TestWiFiDisconnect_Success(t *testing.T) {
	fake := &fakeWiFiBluetoothServer{}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "wifi_disconnect", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "disconnected from WiFi" {
		t.Errorf("text = %q, want %q", text, "disconnected from WiFi")
	}
}

func TestWiFiKnownNetworks_ReturnsNetworks(t *testing.T) {
	fake := &fakeWiFiBluetoothServer{
		knownNetworks: []*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork{
			{Ssid: "HomeNet", Priority: 1},
		},
	}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "wifi_known_networks", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var networks []map[string]any
	if err := json.Unmarshal([]byte(text), &networks); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if len(networks) != 1 || networks[0]["ssid"] != "HomeNet" {
		t.Errorf("unexpected networks: %v", networks)
	}
}

func TestBluetoothScan_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "bluetooth_scan", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestBluetoothScan_ReturnsPeripherals(t *testing.T) {
	fake := &fakeWiFiBluetoothServer{
		btPeripherals: []*agentpb.DiscoveredBluetoothPeripheral{
			{Name: "HeadPhones", Address: "AA:BB:CC:DD:EE:FF", Rssi: -60},
		},
	}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "bluetooth_scan", map[string]any{"timeout_seconds": 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var devices []map[string]any
	if err := json.Unmarshal([]byte(text), &devices); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0]["name"] != "HeadPhones" {
		t.Errorf("name = %v, want HeadPhones", devices[0]["name"])
	}
}

func TestBluetoothConnect_Success(t *testing.T) {
	fake := &fakeWiFiBluetoothServer{}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "bluetooth_connect", map[string]any{"address": "AA:BB:CC:DD:EE:FF"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "connected to AA:BB:CC:DD:EE:FF" {
		t.Errorf("text = %q", text)
	}
}

func TestBluetoothDisconnect_Success(t *testing.T) {
	fake := &fakeWiFiBluetoothServer{}
	conn := startFakeAgentWiFiServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "bluetooth_disconnect", map[string]any{"address": "AA:BB:CC:DD:EE:FF"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "disconnected from AA:BB:CC:DD:EE:FF" {
		t.Errorf("text = %q", text)
	}
}
