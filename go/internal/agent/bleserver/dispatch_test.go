package bleserver

import (
	"context"
	"fmt"
	"io"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/internal/agent/services"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// mockNetworkManager implements services.NetworkManager for testing.
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
func (m *mockNetworkManager) ConnectToWiFi(_ context.Context, _, _ string) error {
	return m.connectErr
}
func (m *mockNetworkManager) GetWiFiStatus(_ context.Context) (bool, string, error) {
	return m.connected, m.ssid, m.statusErr
}
func (m *mockNetworkManager) DisconnectWiFi(_ context.Context) error {
	return m.disconnErr
}

// mockHardwareDiscoverer implements services.HardwareDiscoverer for testing.
type mockHardwareDiscoverer struct {
	caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	err  error
}

func (m *mockHardwareDiscoverer) Discover(_ context.Context, _ string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error) {
	return m.caps, m.err
}

// mockBluetoothManager implements services.BluetoothManager for testing.
type mockBluetoothManager struct {
	peripherals []*agentpb.DiscoveredBluetoothPeripheral
	scanErr     error
	connectErr  error
	disconnErr  error
	forgetErr   error
}

func (m *mockBluetoothManager) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	if m.scanErr != nil {
		return nil, m.scanErr
	}
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral, 1)
	ch <- m.peripherals
	close(ch)
	return ch, nil
}
func (m *mockBluetoothManager) Connect(_ context.Context, _ string, _, _ bool) error {
	return m.connectErr
}
func (m *mockBluetoothManager) Disconnect(_ context.Context, _ string) error {
	return m.disconnErr
}
func (m *mockBluetoothManager) Forget(_ context.Context, _ string) error {
	return m.forgetErr
}

// mockContainerdClient implements services.ContainerdClient for testing.
type mockContainerdClient struct {
	containers []*agentpb.AppContainer
	listErr    error
	stopErr    error
	deleteErr  error
}

func (m *mockContainerdClient) ListLayers(_ context.Context) ([]*agentpb.LayerHeader, error) {
	return nil, nil
}
func (m *mockContainerdClient) WriteLayer(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return nil
}
func (m *mockContainerdClient) AssembleImage(_ context.Context, _ string, _ []*agentpb.RunContainerLayerHeader) error {
	return nil
}
func (m *mockContainerdClient) CreateContainer(_ context.Context, _ *agentpb.CreateContainerRequest, _ *appconfig.AppConfig) error {
	return nil
}
func (m *mockContainerdClient) StartContainer(_ context.Context, _ string) (<-chan services.ContainerOutput, error) {
	return nil, nil
}
func (m *mockContainerdClient) StopContainer(_ context.Context, appName string) error {
	return m.stopErr
}
func (m *mockContainerdClient) DeleteContainer(_ context.Context, appName string, _ bool) error {
	return m.deleteErr
}
func (m *mockContainerdClient) ListContainers(_ context.Context) ([]*agentpb.AppContainer, error) {
	return m.containers, m.listErr
}

func TestDispatchWifiList(t *testing.T) {
	sig := int32(-50)
	nm := &mockNetworkManager{
		networks: []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
			{Ssid: "TestNet", SignalStrength: &sig},
		},
	}
	d := NewDispatcher(zap.NewNop(), nm, nil, nil, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiList{WifiList: &agentpb.WifiListCommand{}},
	})

	wl := resp.GetWifiList()
	if wl == nil {
		t.Fatal("expected WifiList response")
	}
	if len(wl.GetNetworks()) != 1 {
		t.Fatalf("expected 1 network, got %d", len(wl.GetNetworks()))
	}
	if wl.GetNetworks()[0].GetSsid() != "TestNet" {
		t.Errorf("expected SSID TestNet, got %s", wl.GetNetworks()[0].GetSsid())
	}
}

func TestDispatchWifiListNilNetwork(t *testing.T) {
	d := NewDispatcher(zap.NewNop(), nil, nil, nil, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiList{WifiList: &agentpb.WifiListCommand{}},
	})

	if resp.GetError() == nil {
		t.Fatal("expected error response for nil network manager")
	}
}

func TestDispatchWifiConnect(t *testing.T) {
	nm := &mockNetworkManager{}
	d := NewDispatcher(zap.NewNop(), nm, nil, nil, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiConnect{
			WifiConnect: &agentpb.WifiConnectCommand{Ssid: "Test", Password: "pass"},
		},
	})

	wc := resp.GetWifiConnect()
	if wc == nil {
		t.Fatal("expected WifiConnect response")
	}
	if !wc.GetSuccess() {
		t.Error("expected success")
	}
}

func TestDispatchWifiConnectError(t *testing.T) {
	nm := &mockNetworkManager{connectErr: fmt.Errorf("auth failed")}
	d := NewDispatcher(zap.NewNop(), nm, nil, nil, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiConnect{
			WifiConnect: &agentpb.WifiConnectCommand{Ssid: "Test", Password: "wrong"},
		},
	})

	wc := resp.GetWifiConnect()
	if wc == nil {
		t.Fatal("expected WifiConnect response")
	}
	if wc.GetSuccess() {
		t.Error("expected failure")
	}
}

func TestDispatchAgentVersion(t *testing.T) {
	d := NewDispatcher(zap.NewNop(), nil, nil, nil, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AgentVersion{AgentVersion: &agentpb.AgentVersionCommand{}},
	})

	av := resp.GetAgentVersion()
	if av == nil {
		t.Fatal("expected AgentVersion response")
	}
	if av.GetOs() == "" {
		t.Error("expected OS to be set")
	}
}

func TestDispatchAppsListNilContainer(t *testing.T) {
	d := NewDispatcher(zap.NewNop(), nil, nil, nil, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AppsList{AppsList: &agentpb.AppsListCommand{}},
	})

	if resp.GetError() == nil {
		t.Fatal("expected error response for nil container client")
	}
}

func TestDispatchBluetoothConnect(t *testing.T) {
	bt := &mockBluetoothManager{}
	d := NewDispatcher(zap.NewNop(), nil, nil, bt, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_BluetoothConnect{
			BluetoothConnect: &agentpb.BluetoothConnectCommand{Address: "AA:BB:CC:DD:EE:FF"},
		},
	})

	bc := resp.GetBluetoothConnect()
	if bc == nil {
		t.Fatal("expected BluetoothConnect response")
	}
	if !bc.GetSuccess() {
		t.Error("expected success")
	}
}

func TestDispatchBluetoothConnectError(t *testing.T) {
	bt := &mockBluetoothManager{connectErr: fmt.Errorf("connection refused")}
	d := NewDispatcher(zap.NewNop(), nil, nil, bt, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_BluetoothConnect{
			BluetoothConnect: &agentpb.BluetoothConnectCommand{Address: "AA:BB:CC:DD:EE:FF"},
		},
	})

	bc := resp.GetBluetoothConnect()
	if bc == nil {
		t.Fatal("expected BluetoothConnect response")
	}
	if bc.GetSuccess() {
		t.Error("expected failure")
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	d := NewDispatcher(zap.NewNop(), nil, nil, nil, nil)

	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{})

	if resp.GetError() == nil {
		t.Fatal("expected error response for nil command")
	}
}
