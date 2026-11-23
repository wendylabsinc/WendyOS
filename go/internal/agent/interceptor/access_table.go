package interceptor

import "github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"

const (
	// anyone may call the method on a Closed device, with or without a key.
	anyone   = trustedkeys.RoleNone
	viewer   = trustedkeys.RoleViewer
	operator = trustedkeys.RoleOperator
	owner    = trustedkeys.RoleOwner
)

// methodRoles is the one method-to-role table of a Closed device: the minimum
// role per agent call. A call that is not listed is denied to every caller
// (default deny); TestEveryAgentMethodHasARole fails the build on an agent
// call without a role, and a Closed agent refuses to start serving one.
//
// owner: everything, plus the trusted keys, the shape, enrollment, unenroll
// and the host shell. operator: deploy and run apps, and the device settings
// apps need. viewer: reads only.
var methodRoles = map[string]trustedkeys.Role{
	// wendy.agent.services.v1.WendyAgentService
	"/wendy.agent.services.v1.WendyAgentService/ConnectBluetoothPeripheral":    operator,
	"/wendy.agent.services.v1.WendyAgentService/ConnectToWiFi":                 operator,
	"/wendy.agent.services.v1.WendyAgentService/DisconnectBluetoothPeripheral": operator,
	"/wendy.agent.services.v1.WendyAgentService/DisconnectWiFi":                operator,
	"/wendy.agent.services.v1.WendyAgentService/DumpKernelLog":                 viewer,
	"/wendy.agent.services.v1.WendyAgentService/ForgetBluetoothPeripheral":     operator,
	"/wendy.agent.services.v1.WendyAgentService/ForgetWiFiNetwork":             operator,
	"/wendy.agent.services.v1.WendyAgentService/GetAgentVersion":               viewer,
	"/wendy.agent.services.v1.WendyAgentService/GetOSUpdateStatus":             viewer,
	"/wendy.agent.services.v1.WendyAgentService/GetPowerMode":                  viewer,
	"/wendy.agent.services.v1.WendyAgentService/GetWiFiStatus":                 viewer,
	"/wendy.agent.services.v1.WendyAgentService/ListHardwareCapabilities":      viewer,
	"/wendy.agent.services.v1.WendyAgentService/ListKnownWiFiNetworks":         viewer,
	"/wendy.agent.services.v1.WendyAgentService/ListWiFiNetworks":              viewer,
	"/wendy.agent.services.v1.WendyAgentService/ReorderKnownWiFiNetworks":      operator,
	"/wendy.agent.services.v1.WendyAgentService/RunContainer":                  operator,
	"/wendy.agent.services.v1.WendyAgentService/ScanBluetoothPeripherals":      viewer,
	"/wendy.agent.services.v1.WendyAgentService/SetHostname":                   operator,
	"/wendy.agent.services.v1.WendyAgentService/SetPowerMode":                  operator,
	"/wendy.agent.services.v1.WendyAgentService/SetWiFiNetworkPriority":        operator,
	"/wendy.agent.services.v1.WendyAgentService/UpdateAgent":                   operator,
	"/wendy.agent.services.v1.WendyAgentService/UpdateOS":                      operator,

	// wendy.agent.services.v1.WendyAudioService
	"/wendy.agent.services.v1.WendyAudioService/ListAudioDevices":      viewer,
	"/wendy.agent.services.v1.WendyAudioService/SetDefaultAudioDevice": operator,
	"/wendy.agent.services.v1.WendyAudioService/StreamAudio":           operator,
	"/wendy.agent.services.v1.WendyAudioService/StreamAudioLevels":     viewer,

	// wendy.agent.services.v1.WendyContainerService
	"/wendy.agent.services.v1.WendyContainerService/AttachContainer":             operator,
	"/wendy.agent.services.v1.WendyContainerService/CreateContainer":             operator,
	"/wendy.agent.services.v1.WendyContainerService/CreateContainerWithProgress": operator,
	"/wendy.agent.services.v1.WendyContainerService/DeleteContainer":             operator,
	"/wendy.agent.services.v1.WendyContainerService/ExecContainer":               operator,
	"/wendy.agent.services.v1.WendyContainerService/GetContainerPorts":           viewer,
	"/wendy.agent.services.v1.WendyContainerService/GetNetworkStats":             viewer,
	"/wendy.agent.services.v1.WendyContainerService/GetResourceStats":            viewer,
	"/wendy.agent.services.v1.WendyContainerService/ListContainerStats":          viewer,
	"/wendy.agent.services.v1.WendyContainerService/ListContainers":              viewer,
	"/wendy.agent.services.v1.WendyContainerService/ListLayers":                  viewer,
	"/wendy.agent.services.v1.WendyContainerService/ListVolumes":                 viewer,
	"/wendy.agent.services.v1.WendyContainerService/PrepareImage":                operator,
	"/wendy.agent.services.v1.WendyContainerService/QueryChunks":                 operator,
	"/wendy.agent.services.v1.WendyContainerService/QueryLayers":                 operator,
	"/wendy.agent.services.v1.WendyContainerService/RemoveVolume":                operator,
	"/wendy.agent.services.v1.WendyContainerService/RunContainer":                operator,
	"/wendy.agent.services.v1.WendyContainerService/StartContainer":              operator,
	"/wendy.agent.services.v1.WendyContainerService/StopContainer":               operator,
	"/wendy.agent.services.v1.WendyContainerService/StreamMCP":                   operator,
	"/wendy.agent.services.v1.WendyContainerService/ValidateContainer":           operator,
	"/wendy.agent.services.v1.WendyContainerService/WriteChunks":                 operator,
	"/wendy.agent.services.v1.WendyContainerService/WriteLayer":                  operator,

	// wendy.agent.services.v1.WendyFileSyncService
	"/wendy.agent.services.v1.WendyFileSyncService/SyncFiles": operator,

	// wendy.agent.services.v1.WendyProvisioningService
	"/wendy.agent.services.v1.WendyProvisioningService/IsProvisioned":     viewer,
	"/wendy.agent.services.v1.WendyProvisioningService/StartProvisioning": owner,
	"/wendy.agent.services.v1.WendyProvisioningService/Unprovision":       owner,

	// wendy.agent.services.v1.WendyShellService
	"/wendy.agent.services.v1.WendyShellService/HostShell": owner,

	// wendy.agent.services.v1.WendyTelemetryService
	"/wendy.agent.services.v1.WendyTelemetryService/StreamLogs":    viewer,
	"/wendy.agent.services.v1.WendyTelemetryService/StreamMetrics": viewer,
	"/wendy.agent.services.v1.WendyTelemetryService/StreamTraces":  viewer,

	// wendy.agent.services.v1.WendyVideoService
	"/wendy.agent.services.v1.WendyVideoService/ForgetCamera":          operator,
	"/wendy.agent.services.v1.WendyVideoService/GetCameraControls":     viewer,
	"/wendy.agent.services.v1.WendyVideoService/GetCameraDHCPStatus":   viewer,
	"/wendy.agent.services.v1.WendyVideoService/ListVideoDevices":      viewer,
	"/wendy.agent.services.v1.WendyVideoService/RefreshCameras":        operator,
	"/wendy.agent.services.v1.WendyVideoService/ResetCameraControls":   operator,
	"/wendy.agent.services.v1.WendyVideoService/SetCameraControls":     operator,
	"/wendy.agent.services.v1.WendyVideoService/SetCameraCredentials":  operator,
	"/wendy.agent.services.v1.WendyVideoService/SetCameraDHCP":         operator,
	"/wendy.agent.services.v1.WendyVideoService/StreamVideo":           operator,
	"/wendy.agent.services.v1.WendyVideoService/TestCameraCredentials": operator,

	// wendy.agent.services.v2.DataService
	"/wendy.agent.services.v2.DataService/AcknowledgeRecordingExport": operator,
	"/wendy.agent.services.v2.DataService/CampaignArm":                operator,
	"/wendy.agent.services.v2.DataService/CampaignDeploy":             operator,
	"/wendy.agent.services.v2.DataService/CampaignDisarm":             operator,
	"/wendy.agent.services.v2.DataService/CampaignInspect":            viewer,
	"/wendy.agent.services.v2.DataService/CampaignRemove":             operator,
	"/wendy.agent.services.v2.DataService/CampaignTrigger":            operator,
	"/wendy.agent.services.v2.DataService/Campaigns":                  viewer,
	"/wendy.agent.services.v2.DataService/DeleteEpisode":              operator,
	"/wendy.agent.services.v2.DataService/Download":                   viewer,
	"/wendy.agent.services.v2.DataService/Episodes":                   viewer,
	"/wendy.agent.services.v2.DataService/Events":                     viewer,
	"/wendy.agent.services.v2.DataService/ExportRecording":            operator,
	"/wendy.agent.services.v2.DataService/Inspect":                    viewer,
	"/wendy.agent.services.v2.DataService/Sources":                    viewer,
	"/wendy.agent.services.v2.DataService/Start":                      operator,
	"/wendy.agent.services.v2.DataService/Status":                     viewer,
	"/wendy.agent.services.v2.DataService/Stop":                       operator,

	// wendy.agent.services.v2.ROS2Service
	"/wendy.agent.services.v2.ROS2Service/CallService":              operator,
	"/wendy.agent.services.v2.ROS2Service/Doctor":                   viewer,
	"/wendy.agent.services.v2.ROS2Service/DownloadBag":              viewer,
	"/wendy.agent.services.v2.ROS2Service/EchoTopic":                viewer,
	"/wendy.agent.services.v2.ROS2Service/Exec":                     operator,
	"/wendy.agent.services.v2.ROS2Service/GetActionInfo":            viewer,
	"/wendy.agent.services.v2.ROS2Service/GetGraph":                 viewer,
	"/wendy.agent.services.v2.ROS2Service/GetLifecycleState":        viewer,
	"/wendy.agent.services.v2.ROS2Service/GetParam":                 viewer,
	"/wendy.agent.services.v2.ROS2Service/GetTopicInfo":             viewer,
	"/wendy.agent.services.v2.ROS2Service/ListActions":              viewer,
	"/wendy.agent.services.v2.ROS2Service/ListBags":                 viewer,
	"/wendy.agent.services.v2.ROS2Service/ListComponents":           viewer,
	"/wendy.agent.services.v2.ROS2Service/ListLifecycleNodes":       viewer,
	"/wendy.agent.services.v2.ROS2Service/ListLifecycleTransitions": viewer,
	"/wendy.agent.services.v2.ROS2Service/ListNodes":                viewer,
	"/wendy.agent.services.v2.ROS2Service/ListParams":               viewer,
	"/wendy.agent.services.v2.ROS2Service/ListServices":             viewer,
	"/wendy.agent.services.v2.ROS2Service/ListTopics":               viewer,
	"/wendy.agent.services.v2.ROS2Service/LoadComponent":            operator,
	"/wendy.agent.services.v2.ROS2Service/MonitorHz":                viewer,
	"/wendy.agent.services.v2.ROS2Service/RecordBag":                operator,
	"/wendy.agent.services.v2.ROS2Service/SendActionGoal":           operator,
	"/wendy.agent.services.v2.ROS2Service/SetLifecycleState":        operator,
	"/wendy.agent.services.v2.ROS2Service/SetParam":                 operator,
	"/wendy.agent.services.v2.ROS2Service/UnloadComponent":          operator,

	// wendy.agent.services.v2.WendyAccessService
	"/wendy.agent.services.v2.WendyAccessService/AddTrustedKey":    owner,
	"/wendy.agent.services.v2.WendyAccessService/CloseDevice":      owner,
	"/wendy.agent.services.v2.WendyAccessService/GetAccess":        anyone,
	"/wendy.agent.services.v2.WendyAccessService/ListTrustedKeys":  owner,
	"/wendy.agent.services.v2.WendyAccessService/OpenDevice":       owner,
	"/wendy.agent.services.v2.WendyAccessService/RemoveTrustedKey": owner,
	"/wendy.agent.services.v2.WendyAccessService/SetKeylessApps":   owner,

	// wendy.agent.services.v2.WendyAgentUpdateService
	"/wendy.agent.services.v2.WendyAgentUpdateService/CheckOnlineUpdate":     viewer,
	"/wendy.agent.services.v2.WendyAgentUpdateService/GetOnlineUpdateStatus": viewer,
	"/wendy.agent.services.v2.WendyAgentUpdateService/StartOnlineUpdate":     operator,
	"/wendy.agent.services.v2.WendyAgentUpdateService/UpdateAgent":           operator,

	// wendy.agent.services.v2.WendyArduinoMcuService
	"/wendy.agent.services.v2.WendyArduinoMcuService/FlashSketch": operator,

	// wendy.agent.services.v2.WendyAudioService
	"/wendy.agent.services.v2.WendyAudioService/ListAudioDevices":      viewer,
	"/wendy.agent.services.v2.WendyAudioService/SetAudioVolume":        operator,
	"/wendy.agent.services.v2.WendyAudioService/SetDefaultAudioDevice": operator,
	"/wendy.agent.services.v2.WendyAudioService/StreamAudio":           operator,
	"/wendy.agent.services.v2.WendyAudioService/StreamAudioLevels":     viewer,

	// wendy.agent.services.v2.WendyBluetoothService
	"/wendy.agent.services.v2.WendyBluetoothService/ConfigureBluetoothRadio":       operator,
	"/wendy.agent.services.v2.WendyBluetoothService/ConnectBluetoothPeripheral":    operator,
	"/wendy.agent.services.v2.WendyBluetoothService/DisconnectBluetoothPeripheral": operator,
	"/wendy.agent.services.v2.WendyBluetoothService/ForgetBluetoothPeripheral":     operator,
	"/wendy.agent.services.v2.WendyBluetoothService/GetBluetoothRadio":             viewer,
	"/wendy.agent.services.v2.WendyBluetoothService/ScanBluetoothPeripherals":      viewer,

	// wendy.agent.services.v2.WendyBuildDeliveryService
	"/wendy.agent.services.v2.WendyBuildDeliveryService/AdmitDelivery":           operator,
	"/wendy.agent.services.v2.WendyBuildDeliveryService/GetDeliveryCapabilities": viewer,
	"/wendy.agent.services.v2.WendyBuildDeliveryService/GetDeliveryReference":    viewer,
	"/wendy.agent.services.v2.WendyBuildDeliveryService/GetDeliveryStatus":       viewer,

	// wendy.agent.services.v2.WendyBuildService
	"/wendy.agent.services.v2.WendyBuildService/BuildImage":           operator,
	"/wendy.agent.services.v2.WendyBuildService/GetBuildCapabilities": viewer,
	"/wendy.agent.services.v2.WendyBuildService/SetBuildHostEnabled":  operator,

	// wendy.agent.services.v2.WendyCalibratedFrameService
	"/wendy.agent.services.v2.WendyCalibratedFrameService/ListCalibratedSources":  viewer,
	"/wendy.agent.services.v2.WendyCalibratedFrameService/ListCameraMetadata":     viewer,
	"/wendy.agent.services.v2.WendyCalibratedFrameService/SetCameraMetadata":      operator,
	"/wendy.agent.services.v2.WendyCalibratedFrameService/StreamCalibratedFrames": operator,

	// wendy.agent.services.v2.WendyContainerService
	"/wendy.agent.services.v2.WendyContainerService/AttachContainer":    operator,
	"/wendy.agent.services.v2.WendyContainerService/DeleteContainer":    operator,
	"/wendy.agent.services.v2.WendyContainerService/ListContainerStats": viewer,
	"/wendy.agent.services.v2.WendyContainerService/ListContainers":     viewer,
	"/wendy.agent.services.v2.WendyContainerService/ListVolumes":        viewer,
	"/wendy.agent.services.v2.WendyContainerService/PruneCache":         operator,
	"/wendy.agent.services.v2.WendyContainerService/RemoveVolume":       operator,
	"/wendy.agent.services.v2.WendyContainerService/StartContainer":     operator,
	"/wendy.agent.services.v2.WendyContainerService/StopContainer":      operator,

	// wendy.agent.services.v2.WendyDeviceInfoService
	"/wendy.agent.services.v2.WendyDeviceInfoService/GetDeviceInfo":            viewer,
	"/wendy.agent.services.v2.WendyDeviceInfoService/ListHardwareCapabilities": viewer,

	// wendy.agent.services.v2.WendyDriverService
	"/wendy.agent.services.v2.WendyDriverService/InstallDriver": operator,
	"/wendy.agent.services.v2.WendyDriverService/ListDrivers":   viewer,
	"/wendy.agent.services.v2.WendyDriverService/RemoveDriver":  operator,

	// wendy.agent.services.v2.WendyFileSyncService
	"/wendy.agent.services.v2.WendyFileSyncService/SyncFiles": operator,

	// wendy.agent.services.v2.WendyLocalMeshAdminService
	"/wendy.agent.services.v2.WendyLocalMeshAdminService/ConfigureLocalMesh":   operator,
	"/wendy.agent.services.v2.WendyLocalMeshAdminService/GetLocalMeshStatus":   viewer,
	"/wendy.agent.services.v2.WendyLocalMeshAdminService/GetLocalMeshTopology": viewer,

	// wendy.agent.services.v2.WendyMeshService
	"/wendy.agent.services.v2.WendyMeshService/MeshDial": operator,

	// wendy.agent.services.v2.WendyOSUpdateService
	"/wendy.agent.services.v2.WendyOSUpdateService/GetOSUpdateStatus": viewer,
	"/wendy.agent.services.v2.WendyOSUpdateService/UpdateOS":          operator,

	// wendy.agent.services.v2.WendyPowerService
	"/wendy.agent.services.v2.WendyPowerService/Reboot": operator,

	// wendy.agent.services.v2.WendyProvisioningService
	"/wendy.agent.services.v2.WendyProvisioningService/CheckACMERevocation":   viewer,
	"/wendy.agent.services.v2.WendyProvisioningService/IsProvisioned":         viewer,
	"/wendy.agent.services.v2.WendyProvisioningService/RevokeACMECertificate": owner,
	"/wendy.agent.services.v2.WendyProvisioningService/StartACMEProvisioning": owner,
	"/wendy.agent.services.v2.WendyProvisioningService/StartProvisioning":     owner,
	"/wendy.agent.services.v2.WendyProvisioningService/Unprovision":           owner,

	// wendy.agent.services.v2.WendyRobotArmService
	"/wendy.agent.services.v2.WendyRobotArmService/BindRobotArm":   operator,
	"/wendy.agent.services.v2.WendyRobotArmService/ForgetRobotArm": operator,
	"/wendy.agent.services.v2.WendyRobotArmService/ListRobotArms":  viewer,

	// wendy.agent.services.v2.WendySensorPairingService
	"/wendy.agent.services.v2.WendySensorPairingService/AddSensorPairing":      operator,
	"/wendy.agent.services.v2.WendySensorPairingService/DiscoverSensorSources": viewer,
	"/wendy.agent.services.v2.WendySensorPairingService/ListSensorPairings":    viewer,
	"/wendy.agent.services.v2.WendySensorPairingService/RemoveSensorPairing":   operator,

	// wendy.agent.services.v2.WendySensorService
	"/wendy.agent.services.v2.WendySensorService/GetSensorManifest": viewer,
	"/wendy.agent.services.v2.WendySensorService/StreamSensors":     viewer,

	// wendy.agent.services.v2.WendyTelemetryService
	"/wendy.agent.services.v2.WendyTelemetryService/StreamLogs":    viewer,
	"/wendy.agent.services.v2.WendyTelemetryService/StreamMetrics": viewer,
	"/wendy.agent.services.v2.WendyTelemetryService/StreamTraces":  viewer,

	// wendy.agent.services.v2.WendyTimeSyncService
	"/wendy.agent.services.v2.WendyTimeSyncService/GetClock":  viewer,
	"/wendy.agent.services.v2.WendyTimeSyncService/SyncClock": operator,

	// wendy.agent.services.v2.WendyTunnelService
	"/wendy.agent.services.v2.WendyTunnelService/DatagramTunnel": operator,
	"/wendy.agent.services.v2.WendyTunnelService/Tunnel":         operator,

	// wendy.agent.services.v2.WendyUnitreeRobotService
	"/wendy.agent.services.v2.WendyUnitreeRobotService/Command": operator,
	"/wendy.agent.services.v2.WendyUnitreeRobotService/Stream":  viewer,

	// wendy.agent.services.v2.WendyUnitreeService
	"/wendy.agent.services.v2.WendyUnitreeService/DiscoverUnitreeRobots": viewer,
	"/wendy.agent.services.v2.WendyUnitreeService/ForgetUnitree":         operator,
	"/wendy.agent.services.v2.WendyUnitreeService/ListUnitreePairings":   viewer,
	"/wendy.agent.services.v2.WendyUnitreeService/PairUnitree":           operator,

	// wendy.agent.services.v2.WendyWiFiService
	"/wendy.agent.services.v2.WendyWiFiService/ConnectToWiFi":              operator,
	"/wendy.agent.services.v2.WendyWiFiService/DisconnectWiFi":             operator,
	"/wendy.agent.services.v2.WendyWiFiService/DisconnectWiFiAcknowledged": operator,
	"/wendy.agent.services.v2.WendyWiFiService/ForgetWiFiNetwork":          operator,
	"/wendy.agent.services.v2.WendyWiFiService/GetWiFiRegulatory":          viewer,
	"/wendy.agent.services.v2.WendyWiFiService/GetWiFiStatus":              viewer,
	"/wendy.agent.services.v2.WendyWiFiService/ListKnownWiFiNetworks":      viewer,
	"/wendy.agent.services.v2.WendyWiFiService/ListWiFiNetworks":           viewer,
	"/wendy.agent.services.v2.WendyWiFiService/ReorderKnownWiFiNetworks":   operator,
	"/wendy.agent.services.v2.WendyWiFiService/RequestWiFiCountry":         operator,
	"/wendy.agent.services.v2.WendyWiFiService/SetWiFiNetworkPriority":     operator,
}
