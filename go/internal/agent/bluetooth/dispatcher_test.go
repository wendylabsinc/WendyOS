// go/internal/agent/bluetooth/dispatcher_test.go
package bluetooth_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/bluetooth"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

// ── Mocks ────────────────────────────────────────────────────────────────────

type mockNet struct {
	networks  []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	connected bool
	ssid      string
	err       error
	request   *agentpb.ConnectToWiFiRequest
	known     []*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork
	prioSSID  string
	priority  int32
	order     []string
	forgot    string
}

func (m *mockNet) ListWiFiNetworks(_ context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	return m.networks, m.err
}
func (m *mockNet) ConnectToWiFi(_ context.Context, req *agentpb.ConnectToWiFiRequest) error {
	m.request = req
	return m.err
}
func (m *mockNet) GetWiFiStatus(_ context.Context) (bool, string, error) {
	return m.connected, m.ssid, m.err
}
func (m *mockNet) DisconnectWiFi(_ context.Context) error { return m.err }
func (m *mockNet) ListKnownWiFiNetworks(_ context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error) {
	return m.known, m.err
}
func (m *mockNet) SetWiFiNetworkPriority(_ context.Context, ssid string, priority int32) error {
	m.prioSSID, m.priority = ssid, priority
	return m.err
}
func (m *mockNet) ReorderKnownWiFiNetworks(_ context.Context, order []string) error {
	m.order = order
	return m.err
}
func (m *mockNet) ForgetWiFiNetwork(_ context.Context, ssid string) error {
	m.forgot = ssid
	return m.err
}

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
func (m *mockBluetooth) Connect(_ context.Context, addr string, _, _ bool) (bool, error) {
	m.connected = addr
	return m.err == nil, m.err
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

func TestDispatch_WifiList_CarriesEveryField(t *testing.T) {
	signal, zero, priority, rssi := int32(87), int32(0), int32(-3), int32(-52)
	net := &mockNet{networks: []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{
			Ssid:           "Office",
			SignalStrength: &signal,
			Security:       agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE,
			IsKnown:        true,
			IsConnected:    true,
			Priority:       &priority,
			RssiDbm:        &rssi,
		},
		// A reading of zero is still a reading and must not become "absent".
		{Ssid: "Faint", SignalStrength: &zero, IsKnown: true, Priority: &zero, RssiDbm: &zero},
		// A row without the optional readings must not gain them.
		{Ssid: "Bare", Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN},
	}}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiList{WifiList: &agentpb.WifiListCommand{}},
	})
	want := []*agentpb.WifiNetworkInfo{
		{
			Ssid:           "Office",
			SignalStrength: &signal,
			Security:       agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE,
			IsKnown:        true,
			IsConnected:    true,
			Priority:       &priority,
			RssiDbm:        &rssi,
		},
		{Ssid: "Faint", SignalStrength: &zero, IsKnown: true, Priority: &zero, RssiDbm: &zero},
		{Ssid: "Bare", Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN},
	}
	got := resp.GetWifiList().GetNetworks()
	if len(got) != len(want) {
		t.Fatalf("got %d networks, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		// proto.Equal tells an unset optional field from one set to zero.
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("network %d: got {%v}, want {%v}", i, got[i], want[i])
		}
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

func TestDispatch_WifiConnect_SecurityAndHidden(t *testing.T) {
	sae := agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE
	unspecified := agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED
	yes, no := true, false
	for _, tc := range []struct {
		name         string
		security     *agentpb.WiFiSecurityType
		hidden       *bool
		wantSecurity *agentpb.WiFiSecurityType
		wantHidden   *bool
	}{
		{name: "security and hidden given", security: &sae, hidden: &yes, wantSecurity: &sae, wantHidden: &yes},
		{name: "security only", security: &sae, wantSecurity: &sae},
		{name: "hidden only", hidden: &yes, wantHidden: &yes},
		// With neither set, the network manager picks the security from its scan.
		{name: "neither given"},
		{name: "unspecified security and not hidden", security: &unspecified, hidden: &no},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := &mockNet{}
			d := newTestDispatcher(net, nil, nil, nil)
			resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
				Command: &agentpb.BluetoothCommand_WifiConnect{
					WifiConnect: &agentpb.WifiConnectCommand{Ssid: "net", Password: "pass", Security: tc.security, Hidden: tc.hidden},
				},
			})
			if !resp.GetWifiConnect().GetSuccess() {
				t.Fatalf("expected success, got %v", resp)
			}
			want := &agentpb.ConnectToWiFiRequest{Ssid: "net", Password: "pass", Security: tc.wantSecurity, Hidden: tc.wantHidden}
			// proto.Equal tells an unset optional field from one set to its zero value.
			if !proto.Equal(net.request, want) {
				t.Errorf("request = {%v}, want {%v}", net.request, want)
			}
		})
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

func TestDispatch_WifiKnownList(t *testing.T) {
	net := &mockNet{known: []*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork{
		{Ssid: "Office", Uuid: "11111111-2222-3333-4444-555555555555", Priority: 7, Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE},
		{Ssid: "Cafe", Uuid: "66666666-7777-8888-9999-000000000000"},
	}}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiKnownList{WifiKnownList: &agentpb.WifiKnownListCommand{}},
	})
	if resp.GetWifiKnownList() == nil {
		t.Fatalf("expected WifiKnownListResponse, got %v", resp)
	}
	want := []*agentpb.KnownWifiNetworkInfo{
		{Ssid: "Office", Uuid: "11111111-2222-3333-4444-555555555555", Priority: 7, Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE},
		{Ssid: "Cafe", Uuid: "66666666-7777-8888-9999-000000000000"},
	}
	got := resp.GetWifiKnownList().GetNetworks()
	if len(got) != len(want) {
		t.Fatalf("got %d networks, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("network %d: got {%v}, want {%v}", i, got[i], want[i])
		}
	}
}

func TestDispatch_WifiSetPriority(t *testing.T) {
	net := &mockNet{}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiSetPriority{
			WifiSetPriority: &agentpb.WifiSetPriorityCommand{Ssid: "Office", Priority: 7},
		},
	})
	if !resp.GetWifiSetPriority().GetSuccess() {
		t.Fatalf("expected success, got %v", resp)
	}
	if net.prioSSID != "Office" || net.priority != 7 {
		t.Errorf("set %q to priority %d, want Office to 7", net.prioSSID, net.priority)
	}
}

func TestDispatch_WifiReorder(t *testing.T) {
	net := &mockNet{}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiReorder{
			WifiReorder: &agentpb.WifiReorderCommand{OrderSsids: []string{"Office", "Cafe"}},
		},
	})
	if !resp.GetWifiReorder().GetSuccess() {
		t.Fatalf("expected success, got %v", resp)
	}
	if want := []string{"Office", "Cafe"}; !slices.Equal(net.order, want) {
		t.Errorf("reordered to %v, want %v", net.order, want)
	}
}

func TestDispatch_WifiForget(t *testing.T) {
	net := &mockNet{}
	d := newTestDispatcher(net, nil, nil, nil)
	resp := d.Dispatch(context.Background(), &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiForget{WifiForget: &agentpb.WifiForgetCommand{Ssid: "Office"}},
	})
	if !resp.GetWifiForget().GetSuccess() {
		t.Fatalf("expected success, got %v", resp)
	}
	if net.forgot != "Office" {
		t.Errorf("forgot %q, want Office", net.forgot)
	}
}

// savedWifiCommands are the commands that read or change saved networks.
func savedWifiCommands() []struct {
	name string
	cmd  *agentpb.BluetoothCommand
} {
	return []struct {
		name string
		cmd  *agentpb.BluetoothCommand
	}{
		{"known list", &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiKnownList{WifiKnownList: &agentpb.WifiKnownListCommand{}}}},
		{"set priority", &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiSetPriority{WifiSetPriority: &agentpb.WifiSetPriorityCommand{Ssid: "Office", Priority: 7}}}},
		{"reorder", &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiReorder{WifiReorder: &agentpb.WifiReorderCommand{OrderSsids: []string{"Office", "Cafe"}}}}},
		{"forget", &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiForget{WifiForget: &agentpb.WifiForgetCommand{Ssid: "Office"}}}},
	}
}

func TestDispatch_SavedWifiCommands_ManagerError(t *testing.T) {
	for _, tc := range savedWifiCommands() {
		t.Run(tc.name, func(t *testing.T) {
			net := &mockNet{err: errors.New("no saved profile")}
			d := newTestDispatcher(net, nil, nil, nil)
			resp := d.Dispatch(context.Background(), tc.cmd)
			if got := resp.GetError().GetMessage(); got != "no saved profile" {
				t.Errorf("error = %q, want the network manager's error; response {%v}", got, resp)
			}
		})
	}
}

func TestDispatch_SavedWifiCommands_NilManager(t *testing.T) {
	for _, tc := range savedWifiCommands() {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDispatcher(nil, nil, nil, nil)
			resp := d.Dispatch(context.Background(), tc.cmd)
			if got := resp.GetError().GetMessage(); got != "network manager unavailable" {
				t.Errorf("error = %q, want \"network manager unavailable\"; response {%v}", got, resp)
			}
		})
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
