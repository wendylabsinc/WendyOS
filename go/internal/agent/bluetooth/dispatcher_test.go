// go/internal/agent/bluetooth/dispatcher_test.go
package bluetooth_test

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/internal/agent/bluetooth"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ── Mocks ────────────────────────────────────────────────────────────────────

type mockNet struct {
	networks  []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	connected bool
	ssid      string
	err       error
}

func (m *mockNet) ListWiFiNetworks(_ context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	return m.networks, m.err
}
func (m *mockNet) ConnectToWiFi(_ context.Context, _, _ string) error { return m.err }
func (m *mockNet) GetWiFiStatus(_ context.Context) (bool, string, error) {
	return m.connected, m.ssid, m.err
}
func (m *mockNet) DisconnectWiFi(_ context.Context) error { return m.err }

type mockContainer struct {
	containers []*agentpb.AppContainer
	err        error
	stopped    string
	deleted    string
}

func (m *mockContainer) ListContainers(_ context.Context) ([]*agentpb.AppContainer, error) {
	return m.containers, m.err
}
func (m *mockContainer) StopContainer(_ context.Context, name string) error {
	m.stopped = name
	return m.err
}
func (m *mockContainer) DeleteContainer(_ context.Context, name string, _ bool) error {
	m.deleted = name
	return m.err
}

type mockHardware struct {
	caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	err  error
}

func (m *mockHardware) Discover(_ context.Context, _ string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error) {
	return m.caps, m.err
}

type mockBluetooth struct {
	peripherals  []*agentpb.DiscoveredBluetoothPeripheral
	err          error
	connected    string
	disconnected string
	forgotten    string
}

func (m *mockBluetooth) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral, 1)
	if len(m.peripherals) > 0 {
		ch <- m.peripherals
	}
	close(ch)
	return ch, nil
}
func (m *mockBluetooth) Connect(_ context.Context, addr string, _, _ bool) error {
	m.connected = addr
	return m.err
}
func (m *mockBluetooth) Disconnect(_ context.Context, addr string) error {
	m.disconnected = addr
	return m.err
}
func (m *mockBluetooth) Forget(_ context.Context, addr string) error {
	m.forgotten = addr
	return m.err
}

func newTestDispatcher(net *mockNet, ctr *mockContainer, hw *mockHardware, bt *mockBluetooth) *bluetooth.Dispatcher {
	return bluetooth.NewDispatcher(net, ctr, hw, bt)
}

// ── WiFi tests ────────────────────────────────────────────────────────────────

func TestDispatch_WifiList(t *testing.T) {
	net := &mockNet{networks: []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "MyNet"},
	}}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiList{WifiList: &agentpb.WifiListCommand{}},
	})
	wl := resp.GetWifiList()
	if wl == nil {
		t.Fatal("expected WifiListResponse, got nil")
	}
	if len(wl.GetNetworks()) != 1 || wl.GetNetworks()[0].GetSsid() != "MyNet" {
		t.Errorf("unexpected networks: %v", wl.GetNetworks())
	}
}

func TestDispatch_WifiList_NilManager(t *testing.T) {
	d := newTestDispatcher(nil, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiList{WifiList: &agentpb.WifiListCommand{}},
	})
	if resp.GetError() == nil {
		t.Fatal("expected error response for nil network manager")
	}
}

func TestDispatch_WifiConnect(t *testing.T) {
	net := &mockNet{}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiConnect{
			WifiConnect: &agentpb.WifiConnectCommand{Ssid: "net", Password: "pass"},
		},
	})
	if resp.GetWifiConnect() == nil {
		t.Fatal("expected WifiConnectResponse")
	}
	if !resp.GetWifiConnect().GetSuccess() {
		t.Error("expected success=true")
	}
}

func TestDispatch_WifiStatus(t *testing.T) {
	net := &mockNet{connected: true, ssid: "HomeNet"}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiStatus{WifiStatus: &agentpb.WifiStatusCommand{}},
	})
	ws := resp.GetWifiStatus()
	if ws == nil {
		t.Fatal("expected WifiStatusResponse")
	}
	if !ws.GetConnected() || ws.GetSsid() != "HomeNet" {
		t.Errorf("unexpected status: connected=%v ssid=%q", ws.GetConnected(), ws.GetSsid())
	}
}

func TestDispatch_WifiDisconnect(t *testing.T) {
	net := &mockNet{}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiDisconnect{WifiDisconnect: &agentpb.WifiDisconnectCommand{}},
	})
	if resp.GetWifiDisconnect() == nil {
		t.Fatal("expected WifiDisconnectResponse")
	}
	if !resp.GetWifiDisconnect().GetSuccess() {
		t.Error("expected success=true")
	}
}

// ── Apps tests ────────────────────────────────────────────────────────────────

func TestDispatch_AppsList(t *testing.T) {
	ctr := &mockContainer{containers: []*agentpb.AppContainer{
		{AppName: "myapp", AppVersion: "1.0"},
	}}
	d := newTestDispatcher(nil, ctr, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AppsList{AppsList: &agentpb.AppsListCommand{}},
	})
	al := resp.GetAppsList()
	if al == nil {
		t.Fatal("expected AppsListResponse")
	}
	if len(al.GetApps()) != 1 || al.GetApps()[0].GetAppName() != "myapp" {
		t.Errorf("unexpected apps: %v", al.GetApps())
	}
}

func TestDispatch_AppsStop(t *testing.T) {
	ctr := &mockContainer{}
	d := newTestDispatcher(nil, ctr, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AppsStop{AppsStop: &agentpb.AppsStopCommand{AppName: "myapp"}},
	})
	if resp.GetAppsStop() == nil {
		t.Fatal("expected AppsStopResponse")
	}
	if !resp.GetAppsStop().GetSuccess() {
		t.Error("expected success=true")
	}
	if ctr.stopped != "myapp" {
		t.Errorf("expected stopped=myapp, got %q", ctr.stopped)
	}
}

func TestDispatch_AppsRemove(t *testing.T) {
	ctr := &mockContainer{}
	d := newTestDispatcher(nil, ctr, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AppsRemove{
			AppsRemove: &agentpb.AppsRemoveCommand{AppName: "myapp", PurgeImage: true},
		},
	})
	if resp.GetAppsRemove() == nil {
		t.Fatal("expected AppsRemoveResponse")
	}
	if !resp.GetAppsRemove().GetSuccess() {
		t.Error("expected success=true")
	}
	if ctr.deleted != "myapp" {
		t.Errorf("expected deleted=myapp, got %q", ctr.deleted)
	}
}

// ── Agent version ──────────────────────────────────────────────────────────────

func TestDispatch_AgentVersion(t *testing.T) {
	d := newTestDispatcher(nil, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AgentVersion{AgentVersion: &agentpb.AgentVersionCommand{}},
	})
	av := resp.GetAgentVersion()
	if av == nil {
		t.Fatal("expected AgentVersionResponse")
	}
	if av.GetVersion() == "" {
		t.Error("expected non-empty version")
	}
}

// ── Hardware ───────────────────────────────────────────────────────────────────

func TestDispatch_HardwareList(t *testing.T) {
	hw := &mockHardware{caps: []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
		{Category: "gpu", Description: "NVIDIA GPU"},
	}}
	d := newTestDispatcher(nil, nil, hw, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_HardwareList{HardwareList: &agentpb.HardwareListCommand{}},
	})
	hl := resp.GetHardwareList()
	if hl == nil {
		t.Fatal("expected HardwareListResponse")
	}
	if len(hl.GetCapabilities()) != 1 || hl.GetCapabilities()[0].GetType() != "gpu" {
		t.Errorf("unexpected capabilities: %v", hl.GetCapabilities())
	}
}

// ── Bluetooth ──────────────────────────────────────────────────────────────────

func TestDispatch_BluetoothList(t *testing.T) {
	bt := &mockBluetooth{peripherals: []*agentpb.DiscoveredBluetoothPeripheral{
		{Name: "Headphones", Address: "AA:BB:CC:DD:EE:FF"},
	}}
	d := newTestDispatcher(nil, nil, nil, bt)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_BluetoothList{BluetoothList: &agentpb.BluetoothListCommand{}},
	})
	bl := resp.GetBluetoothList()
	if bl == nil {
		t.Fatal("expected BluetoothListResponse")
	}
	if len(bl.GetDevices()) != 1 || bl.GetDevices()[0].GetName() != "Headphones" {
		t.Errorf("unexpected devices: %v", bl.GetDevices())
	}
}

func TestDispatch_BluetoothConnect(t *testing.T) {
	bt := &mockBluetooth{}
	d := newTestDispatcher(nil, nil, nil, bt)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_BluetoothConnect{
			BluetoothConnect: &agentpb.BluetoothConnectCommand{Address: "AA:BB:CC:DD:EE:FF"},
		},
	})
	if resp.GetBluetoothConnect() == nil {
		t.Fatal("expected BluetoothConnectResponse")
	}
	if !resp.GetBluetoothConnect().GetSuccess() {
		t.Error("expected success=true")
	}
	if bt.connected != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("expected connected addr, got %q", bt.connected)
	}
}

func TestDispatch_BluetoothDisconnect(t *testing.T) {
	bt := &mockBluetooth{}
	d := newTestDispatcher(nil, nil, nil, bt)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_BluetoothDisconnect{
			BluetoothDisconnect: &agentpb.BluetoothDisconnectCommand{Address: "AA:BB:CC:DD:EE:FF"},
		},
	})
	if resp.GetBluetoothDisconnect() == nil {
		t.Fatal("expected BluetoothDisconnectResponse")
	}
	if !resp.GetBluetoothDisconnect().GetSuccess() {
		t.Error("expected success=true")
	}
	if bt.disconnected != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("expected disconnected addr, got %q", bt.disconnected)
	}
}

func TestDispatch_BluetoothForget(t *testing.T) {
	bt := &mockBluetooth{}
	d := newTestDispatcher(nil, nil, nil, bt)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_BluetoothForget{
			BluetoothForget: &agentpb.BluetoothForgetCommand{Address: "AA:BB:CC:DD:EE:FF"},
		},
	})
	if resp.GetBluetoothForget() == nil {
		t.Fatal("expected BluetoothForgetResponse")
	}
	if !resp.GetBluetoothForget().GetSuccess() {
		t.Error("expected success=true")
	}
	if bt.forgotten != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("expected forgotten addr, got %q", bt.forgotten)
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	d := newTestDispatcher(nil, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{})
	if resp.GetError() == nil {
		t.Fatal("expected error response for unknown command")
	}
}
