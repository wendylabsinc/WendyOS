package grpcclient

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// readOnlyKey marks a context set by WithReadOnly.
type readOnlyKey struct{}

// WithReadOnly marks ctx so that agent RPCs made with it may only read: an RPC
// that readRPCs does not list fails with PermissionDenied before it is sent.
// It covers an AgentConnection's service clients and Client(). wendy chat's
// turns that a watch report started use it, so a tool marked read-only by
// mistake still cannot change the device.
func WithReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

// IsReadOnly reports whether ctx was marked by WithReadOnly.
func IsReadOnly(ctx context.Context) bool {
	readOnly, _ := ctx.Value(readOnlyKey{}).(bool)
	return readOnly
}

func checkReadOnly(ctx context.Context, method string) error {
	if !IsReadOnly(ctx) || readRPCs[method] {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "%s can change the device, and this call may only read it", method)
}

// guardedConn applies checkReadOnly to every call on the connection.
type guardedConn struct{ conn grpc.ClientConnInterface }

func (g guardedConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if err := checkReadOnly(ctx, method); err != nil {
		return err
	}
	return g.conn.Invoke(ctx, method, args, reply, opts...)
}

func (g guardedConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	if err := checkReadOnly(ctx, method); err != nil {
		return nil, err
	}
	return g.conn.NewStream(ctx, desc, method, opts...)
}

// readRPCs are the agent RPCs that change nothing on the device. Scans and
// probes count as reads: RefreshCameras and the discovery RPCs rescan,
// TestCameraCredentials tries a stored login, and ROS2Service reads may start
// the agent's own inspector. Every other RPC is in writeRPCs, and a read-only
// context refuses it and anything unclassified. TestEveryAgentRPCIsClassified
// fails on an RPC in neither list; put one here only after reading its
// handler.
var readRPCs = rpcSet(map[string][]string{
	"v1.WendyAgentService": {"DumpKernelLog", "GetAgentVersion", "GetOSUpdateStatus", "GetPowerMode", "GetWiFiStatus",
		"ListHardwareCapabilities", "ListKnownWiFiNetworks", "ListWiFiNetworks", "ScanBluetoothPeripherals"},
	"v1.WendyAudioService": {"ListAudioDevices", "StreamAudio", "StreamAudioLevels"},
	"v1.WendyContainerService": {"GetContainerPorts", "GetResourceStats", "ListContainerStats", "ListContainers", "ListLayers",
		"ListVolumes", "QueryChunks", "QueryLayers", "ValidateContainer"},
	"v1.WendyProvisioningService": {"IsProvisioned"},
	"v1.WendyTelemetryService":    {"StreamLogs", "StreamMetrics", "StreamTraces"},
	"v1.WendyVideoService": {"GetCameraControls", "GetCameraDHCPStatus", "ListVideoDevices", "RefreshCameras", "StreamVideo",
		"TestCameraCredentials"},
	"v2.DataService": {"CampaignInspect", "Campaigns", "Documents", "Download", "Episodes", "Events", "ExportRecording", "Inspect",
		"Sources", "Status"},
	"v2.ROS2Service": {"Doctor", "DownloadBag", "EchoTopic", "GetActionInfo", "GetGraph", "GetLifecycleState", "GetParam",
		"GetTopicInfo", "ListActions", "ListBags", "ListComponents", "ListLifecycleNodes", "ListLifecycleTransitions", "ListNodes",
		"ListParams", "ListServices", "ListTopics", "MonitorHz"},
	"v2.WendyAudioService":           {"ListAudioDevices", "StreamAudio", "StreamAudioLevels"},
	"v2.WendyBluetoothService":       {"ScanBluetoothPeripherals"},
	"v2.WendyBuildService":           {"GetBuildCapabilities"},
	"v2.WendyCalibratedFrameService": {"ListCalibratedSources", "StreamCalibratedFrames"},
	"v2.WendyContainerService":       {"ListContainerStats", "ListContainers", "ListVolumes"},
	"v2.WendyDeviceInfoService":      {"GetDeviceInfo", "ListHardwareCapabilities"},
	"v2.WendyDriverService":          {"ListDrivers"},
	"v2.WendyLocalMeshAdminService":  {"GetLocalMeshStatus", "GetLocalMeshTopology"},
	"v2.WendyOSUpdateService":        {"GetOSUpdateStatus"},
	"v2.WendyProvisioningService":    {"CheckACMERevocation", "IsProvisioned"},
	"v2.WendyRobotArmService":        {"ListRobotArms"},
	"v2.WendySensorPairingService":   {"DiscoverSensorSources", "ListSensorPairings"},
	"v2.WendySensorService":          {"GetSensorManifest", "StreamSensors"},
	"v2.WendyTelemetryService":       {"StreamLogs", "StreamMetrics", "StreamTraces"},
	"v2.WendyTimeSyncService":        {"GetClock"},
	"v2.WendyUnitreeRobotService":    {"Stream"},
	"v2.WendyUnitreeService":         {"DiscoverUnitreeRobots", "ListUnitreePairings"},
	"v2.WendyWiFiService":            {"GetWiFiRegulatory", "GetWiFiStatus", "ListKnownWiFiNetworks", "ListWiFiNetworks"},
})

// writeRPCs are the agent RPCs that change the device, or open a channel
// through which something else can: a shell, a tunnel, an app's MCP stream.
var writeRPCs = rpcSet(map[string][]string{
	"v1.WendyAgentService": {"ConnectBluetoothPeripheral", "ConnectToWiFi", "DisconnectBluetoothPeripheral", "DisconnectWiFi",
		"ForgetBluetoothPeripheral", "ForgetWiFiNetwork", "ReorderKnownWiFiNetworks", "RunContainer", "SetHostname", "SetPowerMode",
		"SetWiFiNetworkPriority", "UpdateAgent", "UpdateOS"},
	"v1.WendyAudioService": {"SetDefaultAudioDevice"},
	"v1.WendyContainerService": {"AttachContainer", "CreateContainer", "CreateContainerWithProgress", "DeleteContainer",
		"ExecContainer", "PrepareImage", "RemoveVolume", "RunContainer", "StartContainer", "StopContainer", "StreamMCP",
		"WriteChunks", "WriteLayer"},
	"v1.WendyFileSyncService":     {"SyncFiles"},
	"v1.WendyProvisioningService": {"StartProvisioning", "Unprovision"},
	"v1.WendyShellService":        {"HostShell"},
	"v1.WendyVideoService": {"ForgetCamera", "ResetCameraControls", "SetCameraControls", "SetCameraCredentials",
		"SetCameraDHCP"},
	"v2.DataService": {"AcknowledgeRecordingExport", "CampaignDeploy", "CampaignTrigger", "DocumentApply", "DocumentRemove",
		"DocumentRenew", "Start", "Stop"},
	"v2.ROS2Service": {"CallService", "Exec", "LoadComponent", "RecordBag", "SendActionGoal", "SetLifecycleState", "SetParam",
		"UnloadComponent"},
	"v2.WendyAgentUpdateService":    {"UpdateAgent"},
	"v2.WendyAudioService":          {"SetAudioVolume", "SetDefaultAudioDevice"},
	"v2.WendyBluetoothService":      {"ConnectBluetoothPeripheral", "DisconnectBluetoothPeripheral", "ForgetBluetoothPeripheral"},
	"v2.WendyBuildService":          {"BuildImage", "SetBuildHostEnabled"},
	"v2.WendyContainerService":      {"AttachContainer", "DeleteContainer", "PruneCache", "RemoveVolume", "StartContainer", "StopContainer"},
	"v2.WendyDriverService":         {"InstallDriver", "RemoveDriver"},
	"v2.WendyFileSyncService":       {"SyncFiles"},
	"v2.WendyLocalMeshAdminService": {"ConfigureLocalMesh"},
	"v2.WendyMeshService":           {"MeshDial"},
	"v2.WendyOSUpdateService":       {"UpdateOS"},
	"v2.WendyProvisioningService": {"RevokeACMECertificate", "StartACMEProvisioning", "StartProvisioning",
		"Unprovision"},
	"v2.WendyRobotArmService":      {"BindRobotArm", "ForgetRobotArm"},
	"v2.WendySensorPairingService": {"AddSensorPairing", "RemoveSensorPairing"},
	"v2.WendyTimeSyncService":      {"SyncClock"},
	"v2.WendyTunnelService":        {"DatagramTunnel", "Tunnel"},
	"v2.WendyUnitreeRobotService":  {"Command"},
	"v2.WendyUnitreeService":       {"ForgetUnitree", "PairUnitree"},
	"v2.WendyWiFiService": {"ConnectToWiFi", "DisconnectWiFi", "ForgetWiFiNetwork", "ReorderKnownWiFiNetworks",
		"RequestWiFiCountry", "SetWiFiNetworkPriority"},
})

// rpcSet turns service -> method names into gRPC full method names.
func rpcSet(services map[string][]string) map[string]bool {
	set := map[string]bool{}
	for service, methods := range services {
		for _, method := range methods {
			set["/wendy.agent.services."+service+"/"+method] = true
		}
	}
	return set
}
