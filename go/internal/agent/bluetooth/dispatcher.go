// Package bluetooth contains the BLE peripheral subsystem for the wendy-agent.
package bluetooth

import (
	"context"
	"fmt"
	"reflect"
	"runtime"

	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Narrow interfaces so this file doesn't import the services package directly.

type networkOps interface {
	ListWiFiNetworks(ctx context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error)
	ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error
	GetWiFiStatus(ctx context.Context) (connected bool, ssid string, err error)
	DisconnectWiFi(ctx context.Context) error
	ListKnownWiFiNetworks(ctx context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error)
	SetWiFiNetworkPriority(ctx context.Context, ssid string, priority int32) error
	ReorderKnownWiFiNetworks(ctx context.Context, orderedSSIDs []string) error
	ForgetWiFiNetwork(ctx context.Context, ssid string) error
}

type containerOps interface {
	ListContainers(ctx context.Context) ([]*agentpb.AppContainer, error)
	StopContainer(ctx context.Context, appName string) error
	DeleteContainer(ctx context.Context, appName string, deleteImage bool) error
}

type hardwareOps interface {
	Discover(ctx context.Context, categoryFilter string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error)
}

type bluetoothOps interface {
	Scan(ctx context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error)
	Connect(ctx context.Context, address string, pair, trust bool) (paired bool, err error)
	Disconnect(ctx context.Context, address string) error
	Forget(ctx context.Context, address string) error
}

// Dispatcher deserializes a BluetoothCommand and routes it to the appropriate
// service, returning a BluetoothResponse.
type Dispatcher struct {
	network   networkOps
	container containerOps
	hardware  hardwareOps
	bluetooth bluetoothOps
}

func NewDispatcher(net networkOps, ctr containerOps, hw hardwareOps, bt bluetoothOps) *Dispatcher {
	return &Dispatcher{
		network:   net,
		container: ctr,
		hardware:  hw,
		bluetooth: bt,
	}
}

// Dispatch routes cmd to the appropriate handler and returns a response.
// It never returns nil.
func (d *Dispatcher) Dispatch(ctx context.Context, cmd *agentpb.BluetoothCommand) *agentpb.BluetoothResponse {
	switch c := cmd.Command.(type) {

	// ── WiFi ──────────────────────────────────────────────────────────────────

	case *agentpb.BluetoothCommand_WifiList:
		return d.wifiList(ctx, c.WifiList)
	case *agentpb.BluetoothCommand_WifiConnect:
		return d.wifiConnect(ctx, c.WifiConnect)
	case *agentpb.BluetoothCommand_WifiStatus:
		return d.wifiStatus(ctx, c.WifiStatus)
	case *agentpb.BluetoothCommand_WifiDisconnect:
		return d.wifiDisconnect(ctx, c.WifiDisconnect)
	case *agentpb.BluetoothCommand_WifiKnownList:
		return d.wifiKnownList(ctx, c.WifiKnownList)
	case *agentpb.BluetoothCommand_WifiSetPriority:
		return d.wifiSetPriority(ctx, c.WifiSetPriority)
	case *agentpb.BluetoothCommand_WifiReorder:
		return d.wifiReorder(ctx, c.WifiReorder)
	case *agentpb.BluetoothCommand_WifiForget:
		return d.wifiForget(ctx, c.WifiForget)

	// ── Apps ──────────────────────────────────────────────────────────────────

	case *agentpb.BluetoothCommand_AppsList:
		return d.appsList(ctx, c.AppsList)
	case *agentpb.BluetoothCommand_AppsStop:
		return d.appsStop(ctx, c.AppsStop)
	case *agentpb.BluetoothCommand_AppsRemove:
		return d.appsRemove(ctx, c.AppsRemove)

	// ── Agent ─────────────────────────────────────────────────────────────────

	case *agentpb.BluetoothCommand_AgentVersion:
		return d.agentVersion(ctx, c.AgentVersion)

	// ── Hardware ──────────────────────────────────────────────────────────────

	case *agentpb.BluetoothCommand_HardwareList:
		return d.hardwareList(ctx, c.HardwareList)

	// ── Bluetooth ─────────────────────────────────────────────────────────────

	case *agentpb.BluetoothCommand_BluetoothList:
		return d.bluetoothList(ctx, c.BluetoothList)
	case *agentpb.BluetoothCommand_BluetoothConnect:
		return d.bluetoothConnect(ctx, c.BluetoothConnect)
	case *agentpb.BluetoothCommand_BluetoothDisconnect:
		return d.bluetoothDisconnect(ctx, c.BluetoothDisconnect)
	case *agentpb.BluetoothCommand_BluetoothForget:
		return d.bluetoothForget(ctx, c.BluetoothForget)

	default:
		return errResp("unknown command type")
	}
}

// ── WiFi handlers ─────────────────────────────────────────────────────────────

func (d *Dispatcher) wifiList(ctx context.Context, _ *agentpb.WifiListCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	nets, err := d.network.ListWiFiNetworks(ctx)
	if err != nil {
		return errResp(err.Error())
	}
	infos := make([]*agentpb.WifiNetworkInfo, 0, len(nets))
	for _, n := range nets {
		// Optional readings are copied as pointers so an unset one stays unset.
		infos = append(infos, &agentpb.WifiNetworkInfo{
			Ssid:           n.GetSsid(),
			SignalStrength: n.SignalStrength,
			Security:       n.GetSecurity(),
			IsKnown:        n.GetIsKnown(),
			IsConnected:    n.GetIsConnected(),
			Priority:       n.Priority,
			RssiDbm:        n.RssiDbm,
		})
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiList{
			WifiList: &agentpb.WifiListResponse{Networks: infos},
		},
	}
}

func (d *Dispatcher) wifiConnect(ctx context.Context, cmd *agentpb.WifiConnectCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	req := &agentpb.ConnectToWiFiRequest{Ssid: cmd.GetSsid(), Password: cmd.GetPassword()}
	// Without a hint the network manager picks the security from its own scan,
	// so only a real choice is passed on.
	if cmd.GetSecurity() != agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED {
		req.Security = cmd.Security
	}
	if cmd.GetHidden() {
		req.Hidden = cmd.Hidden
	}
	if err := d.network.ConnectToWiFi(ctx, req); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiConnect{
			WifiConnect: &agentpb.WifiConnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) wifiStatus(ctx context.Context, _ *agentpb.WifiStatusCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	connected, ssid, err := d.network.GetWiFiStatus(ctx)
	if err != nil {
		return errResp(err.Error())
	}
	var ssidPtr *string
	if connected && ssid != "" {
		ssidPtr = &ssid
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiStatus{
			WifiStatus: &agentpb.WifiStatusResponse{Connected: connected, Ssid: ssidPtr},
		},
	}
}

func (d *Dispatcher) wifiDisconnect(ctx context.Context, _ *agentpb.WifiDisconnectCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	if err := d.network.DisconnectWiFi(ctx); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiDisconnect{
			WifiDisconnect: &agentpb.WifiDisconnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) wifiKnownList(ctx context.Context, _ *agentpb.WifiKnownListCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	known, err := d.network.ListKnownWiFiNetworks(ctx)
	if err != nil {
		return errResp(err.Error())
	}
	infos := make([]*agentpb.KnownWifiNetworkInfo, 0, len(known))
	for _, k := range known {
		infos = append(infos, &agentpb.KnownWifiNetworkInfo{
			Ssid:     k.GetSsid(),
			Uuid:     k.GetUuid(),
			Priority: k.GetPriority(),
			Security: k.GetSecurity(),
		})
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiKnownList{
			WifiKnownList: &agentpb.WifiKnownListResponse{Networks: infos},
		},
	}
}

func (d *Dispatcher) wifiSetPriority(ctx context.Context, cmd *agentpb.WifiSetPriorityCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	if err := d.network.SetWiFiNetworkPriority(ctx, cmd.GetSsid(), cmd.GetPriority()); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiSetPriority{
			WifiSetPriority: &agentpb.WifiSetPriorityResponse{Success: true},
		},
	}
}

func (d *Dispatcher) wifiReorder(ctx context.Context, cmd *agentpb.WifiReorderCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	if err := d.network.ReorderKnownWiFiNetworks(ctx, cmd.GetOrderSsids()); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiReorder{
			WifiReorder: &agentpb.WifiReorderResponse{Success: true},
		},
	}
}

func (d *Dispatcher) wifiForget(ctx context.Context, cmd *agentpb.WifiForgetCommand) *agentpb.BluetoothResponse {
	if isNil(d.network) {
		return errResp("network manager unavailable")
	}
	if err := d.network.ForgetWiFiNetwork(ctx, cmd.GetSsid()); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiForget{
			WifiForget: &agentpb.WifiForgetResponse{Success: true},
		},
	}
}

// ── Apps handlers ─────────────────────────────────────────────────────────────

func (d *Dispatcher) appsList(ctx context.Context, _ *agentpb.AppsListCommand) *agentpb.BluetoothResponse {
	if isNil(d.container) {
		return errResp("container client unavailable")
	}
	containers, err := d.container.ListContainers(ctx)
	if err != nil {
		return errResp(err.Error())
	}
	apps := make([]*agentpb.AppInfo, 0, len(containers))
	for _, c := range containers {
		apps = append(apps, &agentpb.AppInfo{
			AppName:    c.GetAppName(),
			AppVersion: c.GetAppVersion(),
		})
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AppsList{
			AppsList: &agentpb.AppsListResponse{Apps: apps},
		},
	}
}

func (d *Dispatcher) appsStop(ctx context.Context, cmd *agentpb.AppsStopCommand) *agentpb.BluetoothResponse {
	if isNil(d.container) {
		return errResp("container client unavailable")
	}
	if err := d.container.StopContainer(ctx, cmd.GetAppName()); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AppsStop{
			AppsStop: &agentpb.AppsStopResponse{Success: true},
		},
	}
}

func (d *Dispatcher) appsRemove(ctx context.Context, cmd *agentpb.AppsRemoveCommand) *agentpb.BluetoothResponse {
	if isNil(d.container) {
		return errResp("container client unavailable")
	}
	if err := d.container.DeleteContainer(ctx, cmd.GetAppName(), cmd.GetPurgeImage()); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AppsRemove{
			AppsRemove: &agentpb.AppsRemoveResponse{Success: true},
		},
	}
}

// ── Agent handler ─────────────────────────────────────────────────────────────

func (d *Dispatcher) agentVersion(_ context.Context, _ *agentpb.AgentVersionCommand) *agentpb.BluetoothResponse {
	platform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AgentVersion{
			AgentVersion: &agentpb.AgentVersionResponse{
				Version: version.Version,
				Os:      &platform,
			},
		},
	}
}

// ── Hardware handler ──────────────────────────────────────────────────────────

func (d *Dispatcher) hardwareList(ctx context.Context, cmd *agentpb.HardwareListCommand) *agentpb.BluetoothResponse {
	if isNil(d.hardware) {
		return errResp("hardware discoverer unavailable")
	}
	caps, err := d.hardware.Discover(ctx, "")
	if err != nil {
		return errResp(err.Error())
	}
	_ = cmd // HardwareListCommand has no category field in this proto version
	infos := make([]*agentpb.HardwareInfo, 0, len(caps))
	for _, c := range caps {
		infos = append(infos, &agentpb.HardwareInfo{
			Type: c.GetCategory(),
			Name: c.GetDescription(),
		})
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_HardwareList{
			HardwareList: &agentpb.HardwareListResponse{Capabilities: infos},
		},
	}
}

// ── Bluetooth handlers ────────────────────────────────────────────────────────

func (d *Dispatcher) bluetoothList(ctx context.Context, cmd *agentpb.BluetoothListCommand) *agentpb.BluetoothResponse {
	if isNil(d.bluetooth) {
		return errResp("bluetooth manager unavailable")
	}
	ch, err := d.bluetooth.Scan(ctx)
	if err != nil {
		return errResp(err.Error())
	}
	var devices []*agentpb.BluetoothDeviceInfo
loop:
	for {
		select {
		case batch, ok := <-ch:
			if !ok {
				break loop
			}
			for _, p := range batch {
				if cmd.GetPairedOnly() && !p.GetPaired() {
					continue
				}
				devices = append(devices, &agentpb.BluetoothDeviceInfo{
					Name:    p.GetName(),
					Address: p.GetAddress(),
				})
			}
		case <-ctx.Done():
			break loop
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothList{
			BluetoothList: &agentpb.BluetoothListResponse{Devices: devices},
		},
	}
}

func (d *Dispatcher) bluetoothConnect(ctx context.Context, cmd *agentpb.BluetoothConnectCommand) *agentpb.BluetoothResponse {
	if isNil(d.bluetooth) {
		return errResp("bluetooth manager unavailable")
	}
	if _, err := d.bluetooth.Connect(ctx, cmd.GetAddress(), true, true); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothConnect{
			BluetoothConnect: &agentpb.BluetoothConnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) bluetoothDisconnect(ctx context.Context, cmd *agentpb.BluetoothDisconnectCommand) *agentpb.BluetoothResponse {
	if isNil(d.bluetooth) {
		return errResp("bluetooth manager unavailable")
	}
	if err := d.bluetooth.Disconnect(ctx, cmd.GetAddress()); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothDisconnect{
			BluetoothDisconnect: &agentpb.BluetoothDisconnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) bluetoothForget(ctx context.Context, cmd *agentpb.BluetoothForgetCommand) *agentpb.BluetoothResponse {
	if isNil(d.bluetooth) {
		return errResp("bluetooth manager unavailable")
	}
	if err := d.bluetooth.Forget(ctx, cmd.GetAddress()); err != nil {
		return errResp(err.Error())
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothForget{
			BluetoothForget: &agentpb.BluetoothForgetResponse{Success: true},
		},
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// isNil returns true if v is a nil interface or a typed nil pointer wrapped in an interface.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Ptr && rv.IsNil()
}

func errResp(msg string) *agentpb.BluetoothResponse {
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_Error{
			Error: &agentpb.ErrorResponse{Message: msg},
		},
	}
}
