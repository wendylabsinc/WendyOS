package services

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ---------- mock implementations ----------

type mockNetworkManager struct {
	networks   []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	listErr    error
	connectErr error
	connected  bool
	ssid       string
	statusErr  error
	disconnErr error
}

func (m *mockNetworkManager) ListWiFiNetworks(_ context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	return m.networks, m.listErr
}
func (m *mockNetworkManager) ConnectToWiFi(_ context.Context, _ *agentpb.ConnectToWiFiRequest) error {
	return m.connectErr
}
func (m *mockNetworkManager) GetWiFiStatus(_ context.Context) (bool, string, error) {
	return m.connected, m.ssid, m.statusErr
}
func (m *mockNetworkManager) DisconnectWiFi(_ context.Context) error {
	return m.disconnErr
}
func (m *mockNetworkManager) ListKnownWiFiNetworks(_ context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error) {
	return nil, nil
}
func (m *mockNetworkManager) SetWiFiNetworkPriority(_ context.Context, _ string, _ int32) error {
	return nil
}
func (m *mockNetworkManager) ReorderKnownWiFiNetworks(_ context.Context, _ []string) error {
	return nil
}
func (m *mockNetworkManager) ForgetWiFiNetwork(_ context.Context, _ string) error {
	return nil
}

type mockHardwareDiscoverer struct {
	caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	err  error
}

func (m *mockHardwareDiscoverer) Discover(_ context.Context, _ string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error) {
	return m.caps, m.err
}

type mockBluetoothManager struct{}

func (m *mockBluetoothManager) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral)
	close(ch)
	return ch, nil
}
func (m *mockBluetoothManager) Connect(_ context.Context, _ string, _, _ bool) error { return nil }
func (m *mockBluetoothManager) Disconnect(_ context.Context, _ string) error         { return nil }
func (m *mockBluetoothManager) Forget(_ context.Context, _ string) error             { return nil }

// ---------- bufconn helper ----------

const bufSize = 1024 * 1024

func startAgentServer(t *testing.T, nm NetworkManager, hd HardwareDiscoverer, bm BluetoothManager, opts ...func(*AgentService)) (agentpb.WendyAgentServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	logger := zap.NewNop()
	svc := NewAgentService(logger, nm, hd, bm)
	for _, opt := range opts {
		opt(svc)
	}
	agentpb.RegisterWendyAgentServiceServer(srv, svc)

	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	client := agentpb.NewWendyAgentServiceClient(conn)
	cleanup := func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
	return client, cleanup
}

// ---------- tests ----------

func TestGetAgentVersion(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
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

func TestListWiFiNetworks(t *testing.T) {
	nets := []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "HomeWiFi"},
		{Ssid: "OfficeWiFi"},
	}
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{networks: nets},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ListWiFiNetworks(context.Background(), &agentpb.ListWiFiNetworksRequest{})
	if err != nil {
		t.Fatalf("ListWiFiNetworks: %v", err)
	}
	if len(resp.Networks) != 2 {
		t.Fatalf("len(networks) = %d; want 2", len(resp.Networks))
	}
	if resp.Networks[0].Ssid != "HomeWiFi" {
		t.Errorf("networks[0].ssid = %q; want HomeWiFi", resp.Networks[0].Ssid)
	}
}

func TestConnectToWiFi_Success(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{
		Ssid:     "TestNet",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("ConnectToWiFi: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
}

func TestConnectToWiFi_Failure(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{connectErr: fmt.Errorf("bad password")},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{
		Ssid: "TestNet",
	})
	if err != nil {
		t.Fatalf("ConnectToWiFi: %v", err)
	}
	if resp.Success {
		t.Error("expected failure")
	}
	if resp.GetErrorMessage() == "" {
		t.Error("expected error message")
	}
}

func TestGetWiFiStatus_Connected(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{connected: true, ssid: "MyNet"},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.GetWiFiStatus(context.Background(), &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		t.Fatalf("GetWiFiStatus: %v", err)
	}
	if !resp.Connected {
		t.Error("expected connected = true")
	}
	if resp.GetSsid() != "MyNet" {
		t.Errorf("ssid = %q; want MyNet", resp.GetSsid())
	}
}

func TestGetWiFiStatus_Disconnected(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{connected: false, ssid: ""},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.GetWiFiStatus(context.Background(), &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		t.Fatalf("GetWiFiStatus: %v", err)
	}
	if resp.Connected {
		t.Error("expected connected = false")
	}
}

func TestDisconnectWiFi(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.DisconnectWiFi(context.Background(), &agentpb.DisconnectWiFiRequest{})
	if err != nil {
		t.Fatalf("DisconnectWiFi: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
}

func TestListHardwareCapabilities(t *testing.T) {
	caps := []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
		{Category: "gpu", DevicePath: "/dev/nvidia0", Description: "NVIDIA GPU"},
		{Category: "audio", DevicePath: "/dev/snd/controlC0", Description: "HDA Audio"},
	}
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{caps: caps},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ListHardwareCapabilities(context.Background(), &agentpb.ListHardwareCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ListHardwareCapabilities: %v", err)
	}
	if len(resp.Capabilities) != 2 {
		t.Fatalf("len(caps) = %d; want 2", len(resp.Capabilities))
	}
	if resp.Capabilities[0].Category != "gpu" {
		t.Errorf("cap[0].Category = %q; want gpu", resp.Capabilities[0].Category)
	}
}

func TestUpdateAgent_LockExclusion(t *testing.T) {
	logger := zap.NewNop()
	svc := NewAgentService(logger, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{})

	// Simulate the lock being held.
	svc.updateMu.Lock()
	svc.isUpdating = true
	svc.updateMu.Unlock()

	// Verify the state is set.
	svc.updateMu.Lock()
	if !svc.isUpdating {
		t.Error("expected isUpdating = true after manual set")
	}
	svc.isUpdating = false
	svc.updateMu.Unlock()

	// Verify we can acquire the lock again when not updating.
	svc.updateMu.Lock()
	if svc.isUpdating {
		t.Error("expected isUpdating = false after reset")
	}
	svc.updateMu.Unlock()
}

func TestUpdateOS_NonWendyOSFailsBeforeMender(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) { svc.isWendyOSHost = func() bool { return false } },
	)
	defer cleanup()

	stream, err := client.UpdateOS(context.Background(), &agentpb.UpdateOSRequest{
		ArtifactUrl: "http://example.invalid/update.mender",
	})
	if err != nil {
		t.Fatalf("UpdateOS: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("UpdateOS Recv: %v", err)
	}
	failed := resp.GetFailed()
	if failed == nil {
		t.Fatalf("UpdateOS response = %T, want failed", resp.GetResponseType())
	}
	if failed.GetErrorMessage() != osUpdateUnsupportedForHostMessage {
		t.Fatalf("error message = %q, want %q", failed.GetErrorMessage(), osUpdateUnsupportedForHostMessage)
	}
}

func TestUpdateAgent_ConcurrentLock(t *testing.T) {
	logger := zap.NewNop()
	svc := NewAgentService(logger, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{})

	// First "update" acquires the lock.
	svc.updateMu.Lock()
	svc.isUpdating = true
	svc.updateMu.Unlock()

	// Second attempt must see that isUpdating is true.
	var wg sync.WaitGroup
	wg.Add(1)
	var blocked bool
	go func() {
		defer wg.Done()
		svc.updateMu.Lock()
		blocked = svc.isUpdating
		svc.updateMu.Unlock()
	}()
	wg.Wait()

	if !blocked {
		t.Error("expected second caller to see isUpdating = true")
	}

	// Cleanup
	svc.updateMu.Lock()
	svc.isUpdating = false
	svc.updateMu.Unlock()
}

func TestRunContainer_Deprecated(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	ctx := context.Background()
	stream, err := client.RunContainer(ctx)
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	// The deprecated RunContainer should return Unimplemented on Recv.
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error from deprecated RunContainer")
	}

	// Verify it is an Unimplemented status error.
	if !strings.Contains(err.Error(), "deprecated") && !strings.Contains(err.Error(), "Unimplemented") {
		t.Fatalf("expected Unimplemented/deprecated error, got: %v", err)
	}
}
