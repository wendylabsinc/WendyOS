# Protobuf v2 Service Separation

## Problem

`WendyAgentService` in `wendy/agent/services/v1/wendy_agent_v1_service.proto` is a super service with 17 RPCs across five unrelated concerns: agent management, WiFi, Bluetooth, hardware info, and OS updates. The v2 API breaks these into focused single-responsibility services while keeping v1 intact for existing callers.

## Approach

New directory `Proto/wendy/agent/services/v2/`, package `wendy.agent.services.v2`. v1 is untouched — both versions are registered on the server simultaneously during the transition period. Generated Go code lands in `go/proto/gen/agentpb/v2/`.

## File Layout

```
Proto/wendy/agent/services/v2/
  shared.proto                  # RestartPolicy, AppContainer (redefined; no v1 import)
  device_info_service.proto     # WendyDeviceInfoService
  agent_update_service.proto    # WendyAgentUpdateService
  os_update_service.proto       # WendyOSUpdateService
  wifi_service.proto            # WendyWiFiService
  bluetooth_service.proto       # WendyBluetoothService
  container_service.proto       # WendyContainerService (lifecycle only)
  provisioning_service.proto    # WendyProvisioningService (ported from v1)
  audio_service.proto           # WendyAudioService (ported from v1, updates in a later PR)
  telemetry_service.proto       # WendyTelemetryService (ported from v1)
  file_sync_service.proto       # WendyFileSyncService (ported from v1)
```

## Service Definitions

### WendyDeviceInfoService (new)
Merges agent version and hardware capabilities — answers "what is this device and what can it do?"
- `GetDeviceInfo` (renamed from `GetAgentVersion`) — same response fields
- `ListHardwareCapabilities`

### WendyAgentUpdateService (extracted from WendyAgentService)
- `UpdateAgent` — unchanged

### WendyOSUpdateService (extracted from WendyAgentService)
Separated from agent updates because OS updates only apply to WendyOS; agent updates apply to all platforms including macOS.
- `UpdateOS` — unchanged

### WendyWiFiService (extracted from WendyAgentService)
All 8 RPCs moved verbatim:
- `ListWiFiNetworks`, `ConnectToWiFi`, `GetWiFiStatus`, `DisconnectWiFi`
- `ListKnownWiFiNetworks`, `SetWiFiNetworkPriority`, `ReorderKnownWiFiNetworks`, `ForgetWiFiNetwork`

### WendyBluetoothService (extracted from WendyAgentService)
All 4 RPCs moved verbatim:
- `ScanBluetoothPeripherals`, `ConnectBluetoothPeripheral`, `DisconnectBluetoothPeripheral`, `ForgetBluetoothPeripheral`

### WendyContainerService (trimmed from v1 WendyContainerService)
Layer upload RPCs are removed — layers are no longer transferred over gRPC. The old tarball-streaming `RunContainer` from v1 `WendyAgentService` is also not ported. Removed: `WriteLayer`, `CreateContainer`, `CreateContainerWithProgress`, `RunContainer` (both variants), `ListLayers`. Kept:
- `StartContainer`, `AttachContainer`, `StopContainer`, `DeleteContainer`
- `ListContainers`, `ListVolumes`, `RemoveVolume`, `ListContainerStats`

### WendyProvisioningService (ported unchanged)
- `StartProvisioning`, `IsProvisioned`

### WendyAudioService (ported unchanged for now)
- `ListAudioDevices`, `SetDefaultAudioDevice`, `StreamAudioLevels`, `StreamAudio`
- Updates deferred to a later PR.

### WendyTelemetryService (ported unchanged)
- `StreamLogs`, `StreamMetrics`, `StreamTraces`

### WendyFileSyncService (ported unchanged, Linux disabled for now)
- `SyncFiles`
- Will be defined in proto but not registered on the Linux agent server-side until needed.

## Shared Types

`shared.proto` redefines `RestartPolicy` and `AppContainer` independently — no imports from v1. This avoids cross-version coupling.

`wendy_agent_v1_bluetooth.proto` (the L2CAP BLE transport envelope) currently imports WiFi types from v1. Updating it to import from v2 is deferred to a follow-up PR.

## Go Implementation

New v2 service structs live alongside v1 in `go/internal/agent/services/`, one file per service (e.g. `wifi_service.go`, `bluetooth_service.go`, `device_info_service.go`, `agent_update_service.go`, `os_update_service.go`). The container, provisioning, telemetry, and file-sync services get new v2 structs as well.

**Dependency sharing:** The concrete managers (`NMCLINetworkManager`, `HardwareDiscoverer`, `BluetoothManager`) are typed to v1 proto types throughout `interfaces.go` and their implementations. Rather than updating all of that in this PR, v2 service structs accept the same v1-typed interfaces and do a mechanical v1 → v2 proto mapping at the gRPC response boundary. Small mapper functions (e.g. `mapWiFiNetworkV1toV2`) live in the v2 service file they serve. v1 code is completely untouched.

Interface cleanup (updating `interfaces.go` to use internal domain types shared by both versions) is deferred to the v1 removal PR.

**Server registration:** `go/cmd/wendy-agent/main.go` registers all v2 services alongside their v1 counterparts on the same gRPC server. `WendyFileSyncService` v2 is registered on all platforms except Linux.

## Migration

v1 remains registered on the server alongside v2. CLI and other clients migrate to v2 incrementally. v1 removal is a separate cleanup PR once all callers are on v2.
