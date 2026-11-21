export type Operation = {
  title: string; service: string; method: string; read?: boolean; stream?: boolean;
  download?: boolean; help?: string; initial?: Record<string, unknown>; automatic?: boolean;
};
export type Section = { name: string; description: string; operations: Operation[] };
const service = (name: string, v = 2) => `wendy.agent.services.v${v}.${name}`;
const operationTitles: Record<string, string> = {
  ListNodes: "Nodes", ListTopics: "Topics", ListServices: "Services", ListActions: "Actions",
  ListLifecycleNodes: "Managed nodes", ListComponents: "Components", ListBags: "ROS recordings",
  GetTopicInfo: "Topic details", ListParams: "Node parameters", GetParam: "Parameter value",
  GetGraph: "Node connections", Doctor: "Check ROS health", GetActionInfo: "Action details",
  GetLifecycleState: "Node state", ListLifecycleTransitions: "Available node transitions",
  SetParam: "Change parameter", CallService: "Call ROS service", SetLifecycleState: "Change node state",
  LoadComponent: "Load component", UnloadComponent: "Unload component",
  EchoTopic: "Watch topic messages", MonitorHz: "Measure topic frequency",
  RecordBag: "Record ROS topics", SendActionGoal: "Run ROS action", Exec: "Run ROS command",
  Sources: "Recording sources", Status: "Recording activity", Episodes: "Saved recordings", Campaigns: "Recording campaigns",
  Inspect: "Recording details", CampaignInspect: "Campaign details", Start: "Start recording", Stop: "Stop recording",
  DeleteEpisode: "Delete recording", CampaignDeploy: "Install campaign", CampaignTrigger: "Trigger campaign recording",
  CampaignArm: "Enable campaign triggers", CampaignDisarm: "Pause campaign triggers", CampaignRemove: "Remove campaign",
};
function group(name: string, operations: Omit<Operation, "service">[], v = 2): Operation[] {
  return operations.map((operation) => ({ ...operation, service: service(name, v) }));
}
export const sections: Section[] = [
  { name: "Device", description: "System information, storage and device settings.", operations: [
    ...group("WendyDeviceInfoService", [
      { title: "Device information", method: "GetDeviceInfo", read: true, automatic: true },
      { title: "Connected hardware", method: "ListHardwareCapabilities", read: true, automatic: true },
    ]),
    ...group("WendyAgentService", [
      { title: "Power mode", method: "GetPowerMode", read: true, automatic: true },
      { title: "Change power mode", method: "SetPowerMode", help: "Choose one of the power modes reported by this device." },
      { title: "Rename device", method: "SetHostname", help: "Changing the hostname may disconnect this page. Reconnect using the new name or the device IP address." },
    ], 1),
    ...group("WendyTimeSyncService", [{ title: "Device clock", method: "GetClock", read: true, automatic: true }]),
    ...group("WendyBuildService", [
      { title: "Build capacity", method: "GetBuildCapabilities", read: true, automatic: true },
      { title: "Build host", method: "SetBuildHostEnabled", help: "Allow this device to build applications." },
    ]),
  ] },
  { name: "Applications", description: "Installed applications and their resource usage.", operations: [
    ...group("WendyContainerService", [
      { title: "Installed applications", method: "ListContainers", read: true, stream: true, automatic: true },
      { title: "Create application", method: "CreateContainer", help: "Pull a container image and create an application. Start it separately after reviewing its configuration." },
      { title: "Start application", method: "StartContainer", stream: true },
      { title: "Stop application", method: "StopContainer" },
      { title: "Remove application", method: "DeleteContainer", help: "Deleting volumes permanently removes the application's saved data." },
      { title: "Application ports", method: "GetContainerPorts", read: true },
    ], 1),
  ] },
  { name: "Network", description: "Ethernet, Wi-Fi and your device’s network addresses.", operations: group("WendyWiFiService", [
    { title: "Connection", method: "GetWiFiStatus", read: true, automatic: true },
    { title: "Saved networks", method: "ListKnownWiFiNetworks", read: true, automatic: true },
    { title: "Find networks", method: "ListWiFiNetworks", read: true },
    { title: "Join network", method: "ConnectToWiFi", help: "Changing networks can disconnect this page. Leave the password and security unchanged to reuse a saved network." },
    { title: "Network priority", method: "SetWiFiNetworkPriority" },
    { title: "Reorder saved networks", method: "ReorderKnownWiFiNetworks" },
    { title: "Forget network", method: "ForgetWiFiNetwork", help: "Remove this network and its saved credentials." },
    { title: "Disconnect Wi-Fi", method: "DisconnectWiFi", help: "This page may disconnect. Use Ethernet or reconnect to the device's new address." },
    { title: "Country settings", method: "GetWiFiRegulatory", read: true, automatic: true },
    { title: "Set Wi-Fi country", method: "RequestWiFiCountry", help: "Use the two-letter country code for the device's physical location." },
  ]) },
  { name: "Mesh", description: "Connections to other devices on your mesh network.", operations: group("WendyLocalMeshAdminService", [
    { title: "Mesh status", method: "GetLocalMeshStatus", read: true, automatic: true },
    { title: "Neighbours and routes", method: "GetLocalMeshTopology", read: true, automatic: true, initial: { includeServices: true } },
    { title: "Configure mesh", method: "ConfigureLocalMesh", help: "Choose how this device participates in the mesh network." },
  ]) },
  { name: "Bluetooth", description: "Bluetooth adapters and paired devices.", operations: group("WendyBluetoothService", [
    { title: "Bluetooth status", method: "GetBluetoothRadio", read: true, automatic: true },
    { title: "Bluetooth adapters", method: "ConfigureBluetoothRadio" },
    { title: "Find Bluetooth devices", method: "ScanBluetoothPeripherals", read: true, stream: true },
    { title: "Connect Bluetooth device", method: "ConnectBluetoothPeripheral" },
    { title: "Disconnect Bluetooth device", method: "DisconnectBluetoothPeripheral" },
    { title: "Forget Bluetooth device", method: "ForgetBluetoothPeripheral" },
  ]) },
  { name: "Cameras", description: "Camera previews and capture settings.", operations: group("WendyVideoService", [
    { title: "Cameras", method: "ListVideoDevices", read: true, automatic: true, initial: { includeUnpaired: true } },
    { title: "Watch camera", method: "StreamVideo", read: true, stream: true, initial: { codec: "VIDEO_CODEC_VP8", framerate: 15 }, help: "Select a camera from the device list to preview its video." },
    { title: "Discover cameras", method: "RefreshCameras", read: true },
    { title: "Camera controls", method: "GetCameraControls", read: true },
    { title: "Set camera controls", method: "SetCameraControls" },
    { title: "Reset camera controls", method: "ResetCameraControls" },
    { title: "Camera sign-in", method: "SetCameraCredentials" },
    { title: "Test camera sign-in", method: "TestCameraCredentials", read: true },
    { title: "Forget camera", method: "ForgetCamera" },
    { title: "Camera network", method: "GetCameraDHCPStatus", read: true, automatic: true },
    { title: "Automatic camera addresses", method: "SetCameraDHCP", help: "Assign addresses automatically on a dedicated camera network. Do not enable this on your main LAN." },
  ], 1) },
  { name: "Audio", description: "Microphones, speakers, default devices and volume.", operations: group("WendyAudioService", [
    { title: "Audio devices", method: "ListAudioDevices", read: true, automatic: true },
    { title: "Set default device", method: "SetDefaultAudioDevice" },
    { title: "Set volume", method: "SetAudioVolume", help: "Volume is a percentage from 0 to 100." },
    { title: "Audio levels", method: "StreamAudioLevels", read: true, stream: true, initial: { updateRateHz: 5 } },
    { title: "Listen to microphone", method: "StreamAudio", read: true, stream: true, initial: { sampleRate: 48000, channels: 1 } },
  ]) },
  { name: "Sensors & pairing", description: "Connected sensors and robots.", operations: [
    ...group("WendySensorService", [
      { title: "Available sensors", method: "GetSensorManifest", read: true, automatic: true },
      { title: "Live sensor values", method: "StreamSensors", read: true, stream: true },
    ]),
    ...group("WendySensorPairingService", [
      { title: "Paired sensors", method: "ListSensorPairings", read: true, automatic: true },
      { title: "Find sensors", method: "DiscoverSensorSources", read: true, initial: { timeoutMs: 5000 } },
      { title: "Pair sensor", method: "AddSensorPairing" },
      { title: "Unpair sensor", method: "RemoveSensorPairing" },
    ]),
    ...group("WendyUnitreeService", [
      { title: "Paired robots", method: "ListUnitreePairings", read: true, automatic: true },
      { title: "Find Unitree robots", method: "DiscoverUnitreeRobots", read: true },
      { title: "Pair Unitree robot", method: "PairUnitree" },
      { title: "Forget Unitree robot", method: "ForgetUnitree" },
    ]),
    ...group("WendyRobotArmService", [
      { title: "Robot arms", method: "ListRobotArms", read: true, automatic: true },
      { title: "Pair robot arm", method: "BindRobotArm" },
      { title: "Forget robot arm", method: "ForgetRobotArm" },
    ]),
  ] },
  { name: "Enrollment", description: "Connect this device to your Wendy Cloud organization.", operations: group("WendyProvisioningService", [
    { title: "Enrollment status", method: "IsProvisioned", read: true, automatic: true },
    { title: "Enroll with a token", method: "StartProvisioning", help: "Use enrollment details issued by your Wendy Cloud organization." },
    { title: "Enroll with ACME", method: "StartACMEProvisioning", help: "Use the device registration and external account credentials issued by your organization." },
    { title: "Disconnect from Wendy Cloud", method: "Unprovision", help: "Removes this device's local identity and Cloud access. Removing the Cloud inventory record requires your organization's Cloud account." },
  ]) },
  { name: "Storage", description: "Saved application data and cached files.", operations: group("WendyContainerService", [
    { title: "Volumes", method: "ListVolumes", read: true, automatic: true },
    { title: "Remove volume", method: "RemoveVolume", help: "Permanently delete this volume and all its data." },
    { title: "Preview cache cleanup", method: "PruneCache", read: true, initial: { dryRun: true } },
    { title: "Clean unused cache", method: "PruneCache", initial: { dryRun: false }, help: "Remove unused cached application data. Run the preview first." },
  ]) },
  { name: "Updates & drivers", description: "Software updates and installed hardware drivers.", operations: [
    ...group("WendyAgentUpdateService", [
      { title: "Agent update status", method: "GetOnlineUpdateStatus", read: true, automatic: true },
      { title: "Check for agent update", method: "CheckOnlineUpdate", read: true },
      { title: "Install agent update", method: "StartOnlineUpdate", help: "Check for an update first, then choose its version. The agent restarts during installation." },
    ]),
    ...group("WendyOSUpdateService", [
      { title: "OS update status", method: "GetOSUpdateStatus", read: true, automatic: true, initial: { includeEngineStatus: true } },
      { title: "Install OS update", method: "UpdateOS", stream: true, help: "Choose an operating system update for this board. The device may restart during installation." },
    ]),
    ...group("WendyDriverService", [
      { title: "Installed drivers", method: "ListDrivers", read: true, automatic: true },
      { title: "Install driver", method: "InstallDriver", stream: true, help: "Choose the driver and its download URL." },
      { title: "Remove driver", method: "RemoveDriver", stream: true, help: "Removing a driver can disconnect hardware. Purge also removes its persistent state." },
    ]),
  ] },
  { name: "ROS 2", description: "ROS nodes, topics and recordings.", operations: group("ROS2Service", [
    ...["ListNodes", "ListTopics", "ListServices", "ListActions", "ListLifecycleNodes", "ListComponents", "ListBags"].map((method) => ({ title: operationTitles[method], method, read: true, automatic: true })),
    ...["GetTopicInfo", "ListParams", "GetParam", "GetGraph", "Doctor", "GetActionInfo", "GetLifecycleState", "ListLifecycleTransitions"].map((method) => ({ title: operationTitles[method], method, read: true })),
    ...["SetParam", "CallService", "SetLifecycleState", "LoadComponent", "UnloadComponent"].map((method) => ({ title: operationTitles[method], method })),
    { title: "Download ROS bag", method: "DownloadBag", stream: true, read: true, download: true },
    ...["EchoTopic", "MonitorHz"].map((method) => ({ title: operationTitles[method], method, stream: true, read: true })),
    ...["RecordBag", "SendActionGoal", "Exec"].map((method) => ({ title: operationTitles[method], method, stream: true })),
  ]) },
  { name: "Recordings", description: "Recorded data and automatic recording campaigns.", operations: group("DataService", [
    ...["Sources", "Status", "Episodes", "Campaigns"].map((method) => ({ title: operationTitles[method], method, read: true, automatic: true })),
    { title: "Download recording file", method: "Download", stream: true, read: true, download: true, help: "Choose a recording and a file listed in Recording details. Browser downloads support files up to 134 MB." },
    ...["Inspect", "CampaignInspect"].map((method) => ({ title: operationTitles[method], method, read: true })),
    ...["Start", "Stop", "DeleteEpisode", "CampaignDeploy", "CampaignTrigger", "CampaignArm", "CampaignDisarm", "CampaignRemove"].map((method) => ({ title: operationTitles[method], method })),
  ]) },
  { name: "System logs", description: "Messages from the operating system.", operations: group("WendyAgentService", [
    { title: "Kernel log", method: "DumpKernelLog", read: true, stream: true, automatic: true },
  ], 1) },
];
