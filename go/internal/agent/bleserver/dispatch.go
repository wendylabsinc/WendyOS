package bleserver

import (
	"context"
	"fmt"
	"runtime"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/internal/agent/services"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// Dispatcher routes BluetoothCommand messages to the appropriate service interfaces.
type Dispatcher struct {
	logger    *zap.Logger
	network   services.NetworkManager
	hardware  services.HardwareDiscoverer
	bluetooth services.BluetoothManager
	container services.ContainerdClient
}

// NewDispatcher creates a new command dispatcher. Nil dependencies are allowed;
// commands targeting unavailable services return an ErrorResponse.
func NewDispatcher(
	logger *zap.Logger,
	network services.NetworkManager,
	hardware services.HardwareDiscoverer,
	bluetooth services.BluetoothManager,
	container services.ContainerdClient,
) *Dispatcher {
	return &Dispatcher{
		logger:    logger,
		network:   network,
		hardware:  hardware,
		bluetooth: bluetooth,
		container: container,
	}
}

// Dispatch handles a single BluetoothCommand and returns the corresponding BluetoothResponse.
func (d *Dispatcher) Dispatch(ctx context.Context, cmd *agentpb.BluetoothCommand) *agentpb.BluetoothResponse {
	switch c := cmd.GetCommand().(type) {
	case *agentpb.BluetoothCommand_WifiList:
		return d.handleWifiList(ctx)
	case *agentpb.BluetoothCommand_WifiConnect:
		return d.handleWifiConnect(ctx, c.WifiConnect)
	case *agentpb.BluetoothCommand_WifiStatus:
		return d.handleWifiStatus(ctx)
	case *agentpb.BluetoothCommand_WifiDisconnect:
		return d.handleWifiDisconnect(ctx)
	case *agentpb.BluetoothCommand_AppsList:
		return d.handleAppsList(ctx)
	case *agentpb.BluetoothCommand_AppsStop:
		return d.handleAppsStop(ctx, c.AppsStop)
	case *agentpb.BluetoothCommand_AppsRemove:
		return d.handleAppsRemove(ctx, c.AppsRemove)
	case *agentpb.BluetoothCommand_AgentVersion:
		return d.handleAgentVersion()
	case *agentpb.BluetoothCommand_HardwareList:
		return d.handleHardwareList(ctx)
	case *agentpb.BluetoothCommand_BluetoothList:
		return d.handleBluetoothList(ctx)
	case *agentpb.BluetoothCommand_BluetoothConnect:
		return d.handleBluetoothConnect(ctx, c.BluetoothConnect)
	case *agentpb.BluetoothCommand_BluetoothDisconnect:
		return d.handleBluetoothDisconnect(ctx, c.BluetoothDisconnect)
	case *agentpb.BluetoothCommand_BluetoothForget:
		return d.handleBluetoothForget(ctx, c.BluetoothForget)
	default:
		return errorResponse("unknown command type")
	}
}

func (d *Dispatcher) handleWifiList(ctx context.Context) *agentpb.BluetoothResponse {
	if d.network == nil {
		return errorResponse("WiFi management is not available")
	}
	networks, err := d.network.ListWiFiNetworks(ctx)
	if err != nil {
		return errorResponse(fmt.Sprintf("failed to list WiFi networks: %v", err))
	}
	var infos []*agentpb.WifiNetworkInfo
	for _, n := range networks {
		info := &agentpb.WifiNetworkInfo{Ssid: n.GetSsid()}
		if n.GetSignalStrength() != 0 {
			sig := n.GetSignalStrength()
			info.SignalStrength = &sig
		}
		infos = append(infos, info)
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiList{
			WifiList: &agentpb.WifiListResponse{Networks: infos},
		},
	}
}

func (d *Dispatcher) handleWifiConnect(ctx context.Context, cmd *agentpb.WifiConnectCommand) *agentpb.BluetoothResponse {
	if d.network == nil {
		return errorResponse("WiFi management is not available")
	}
	if err := d.network.ConnectToWiFi(ctx, cmd.GetSsid(), cmd.GetPassword()); err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_WifiConnect{
				WifiConnect: &agentpb.WifiConnectResponse{Success: false, ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiConnect{
			WifiConnect: &agentpb.WifiConnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) handleWifiStatus(ctx context.Context) *agentpb.BluetoothResponse {
	if d.network == nil {
		return errorResponse("WiFi management is not available")
	}
	connected, ssid, err := d.network.GetWiFiStatus(ctx)
	if err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_WifiStatus{
				WifiStatus: &agentpb.WifiStatusResponse{ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiStatus{
			WifiStatus: &agentpb.WifiStatusResponse{Connected: connected, Ssid: &ssid},
		},
	}
}

func (d *Dispatcher) handleWifiDisconnect(ctx context.Context) *agentpb.BluetoothResponse {
	if d.network == nil {
		return errorResponse("WiFi management is not available")
	}
	if err := d.network.DisconnectWiFi(ctx); err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_WifiDisconnect{
				WifiDisconnect: &agentpb.WifiDisconnectResponse{Success: false, ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_WifiDisconnect{
			WifiDisconnect: &agentpb.WifiDisconnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) handleAppsList(ctx context.Context) *agentpb.BluetoothResponse {
	if d.container == nil {
		return errorResponse("container management is not available")
	}
	containers, err := d.container.ListContainers(ctx)
	if err != nil {
		return errorResponse(fmt.Sprintf("failed to list apps: %v", err))
	}
	var apps []*agentpb.AppInfo
	for _, c := range containers {
		apps = append(apps, &agentpb.AppInfo{
			AppName:      c.GetAppName(),
			AppVersion:   c.GetAppVersion(),
			State:        c.GetRunningState().String(),
			FailureCount: int32(c.GetFailureCount()),
		})
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AppsList{
			AppsList: &agentpb.AppsListResponse{Apps: apps},
		},
	}
}

func (d *Dispatcher) handleAppsStop(ctx context.Context, cmd *agentpb.AppsStopCommand) *agentpb.BluetoothResponse {
	if d.container == nil {
		return errorResponse("container management is not available")
	}
	if err := d.container.StopContainer(ctx, cmd.GetAppName()); err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_AppsStop{
				AppsStop: &agentpb.AppsStopResponse{Success: false, ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AppsStop{
			AppsStop: &agentpb.AppsStopResponse{Success: true},
		},
	}
}

func (d *Dispatcher) handleAppsRemove(ctx context.Context, cmd *agentpb.AppsRemoveCommand) *agentpb.BluetoothResponse {
	if d.container == nil {
		return errorResponse("container management is not available")
	}
	if err := d.container.DeleteContainer(ctx, cmd.GetAppName(), cmd.GetPurgeImage()); err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_AppsRemove{
				AppsRemove: &agentpb.AppsRemoveResponse{Success: false, ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AppsRemove{
			AppsRemove: &agentpb.AppsRemoveResponse{Success: true},
		},
	}
}

func (d *Dispatcher) handleAgentVersion() *agentpb.BluetoothResponse {
	resp := &agentpb.AgentVersionResponse{
		Version:    version.Version,
		Featureset: services.DetectFeatureset(),
	}
	osStr := runtime.GOOS
	resp.Os = &osStr
	arch := runtime.GOARCH
	resp.CpuArchitecture = &arch
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_AgentVersion{
			AgentVersion: resp,
		},
	}
}

func (d *Dispatcher) handleHardwareList(ctx context.Context) *agentpb.BluetoothResponse {
	if d.hardware == nil {
		return errorResponse("hardware discovery is not available")
	}
	caps, err := d.hardware.Discover(ctx, "")
	if err != nil {
		return errorResponse(fmt.Sprintf("hardware discovery failed: %v", err))
	}
	var infos []*agentpb.HardwareInfo
	for _, c := range caps {
		infos = append(infos, &agentpb.HardwareInfo{
			Type:      c.GetCategory(),
			Name:      c.GetDescription(),
			Available: true,
		})
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_HardwareList{
			HardwareList: &agentpb.HardwareListResponse{Capabilities: infos},
		},
	}
}

func (d *Dispatcher) handleBluetoothList(ctx context.Context) *agentpb.BluetoothResponse {
	if d.bluetooth == nil {
		return errorResponse("Bluetooth is not available")
	}
	ch, err := d.bluetooth.Scan(ctx)
	if err != nil {
		return errorResponse(fmt.Sprintf("failed to scan Bluetooth: %v", err))
	}
	var devices []*agentpb.BluetoothDeviceInfo
	for peripherals := range ch {
		for _, p := range peripherals {
			dev := &agentpb.BluetoothDeviceInfo{
				Name:       p.GetName(),
				Address:    p.GetAddress(),
				Paired:     p.GetPaired(),
				Connected:  p.GetConnected(),
				DeviceType: p.GetDeviceType(),
			}
			devices = append(devices, dev)
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothList{
			BluetoothList: &agentpb.BluetoothListResponse{Devices: devices},
		},
	}
}

func (d *Dispatcher) handleBluetoothConnect(ctx context.Context, cmd *agentpb.BluetoothConnectCommand) *agentpb.BluetoothResponse {
	if d.bluetooth == nil {
		return errorResponse("Bluetooth is not available")
	}
	if err := d.bluetooth.Connect(ctx, cmd.GetAddress(), true, true); err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_BluetoothConnect{
				BluetoothConnect: &agentpb.BluetoothConnectResponse{Success: false, ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothConnect{
			BluetoothConnect: &agentpb.BluetoothConnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) handleBluetoothDisconnect(ctx context.Context, cmd *agentpb.BluetoothDisconnectCommand) *agentpb.BluetoothResponse {
	if d.bluetooth == nil {
		return errorResponse("Bluetooth is not available")
	}
	if err := d.bluetooth.Disconnect(ctx, cmd.GetAddress()); err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_BluetoothDisconnect{
				BluetoothDisconnect: &agentpb.BluetoothDisconnectResponse{Success: false, ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothDisconnect{
			BluetoothDisconnect: &agentpb.BluetoothDisconnectResponse{Success: true},
		},
	}
}

func (d *Dispatcher) handleBluetoothForget(ctx context.Context, cmd *agentpb.BluetoothForgetCommand) *agentpb.BluetoothResponse {
	if d.bluetooth == nil {
		return errorResponse("Bluetooth is not available")
	}
	if err := d.bluetooth.Forget(ctx, cmd.GetAddress()); err != nil {
		errMsg := err.Error()
		return &agentpb.BluetoothResponse{
			Response: &agentpb.BluetoothResponse_BluetoothForget{
				BluetoothForget: &agentpb.BluetoothForgetResponse{Success: false, ErrorMessage: &errMsg},
			},
		}
	}
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_BluetoothForget{
			BluetoothForget: &agentpb.BluetoothForgetResponse{Success: true},
		},
	}
}

func errorResponse(msg string) *agentpb.BluetoothResponse {
	return &agentpb.BluetoothResponse{
		Response: &agentpb.BluetoothResponse_Error{
			Error: &agentpb.ErrorResponse{Message: msg},
		},
	}
}
