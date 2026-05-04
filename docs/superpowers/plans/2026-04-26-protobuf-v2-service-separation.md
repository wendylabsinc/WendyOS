# Protobuf v2 Service Separation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce `wendy.agent.services.v2` proto package that breaks `WendyAgentService` into focused single-responsibility services, keeping v1 fully intact.

**Architecture:** New `Proto/wendy/agent/services/v2/` directory with 11 proto files; generated Go code in `go/proto/gen/agentpb/v2/` (Go package `agentpbv2`). v2 Go service structs live in the existing `go/internal/agent/services/` package so they can share the v1 interface definitions (`NetworkManager`, `BluetoothManager`, etc.) and unexported helpers. v1 code is not modified. Both versions are registered on the gRPC server simultaneously.

**Tech Stack:** protobuf 3 / protoc, protoc-gen-go, protoc-gen-go-grpc, Go 1.21+, google.golang.org/grpc, `bufconn` for in-process tests, `go.uber.org/zap`.

---

## File Layout

**New proto files:**
- `Proto/wendy/agent/services/v2/shared.proto`
- `Proto/wendy/agent/services/v2/device_info_service.proto`
- `Proto/wendy/agent/services/v2/agent_update_service.proto`
- `Proto/wendy/agent/services/v2/os_update_service.proto`
- `Proto/wendy/agent/services/v2/wifi_service.proto`
- `Proto/wendy/agent/services/v2/bluetooth_service.proto`
- `Proto/wendy/agent/services/v2/container_service.proto`
- `Proto/wendy/agent/services/v2/provisioning_service.proto`
- `Proto/wendy/agent/services/v2/audio_service.proto`
- `Proto/wendy/agent/services/v2/telemetry_service.proto`
- `Proto/wendy/agent/services/v2/file_sync_service.proto`

**Updated:**
- `go/scripts/generate-proto.sh` — add v2 generation block

**Auto-generated (do not edit manually):**
- `go/proto/gen/agentpb/v2/*.pb.go`
- `go/proto/gen/agentpb/v2/*_grpc.pb.go`

**New Go service files (all in package `services`):**
- `go/internal/agent/services/device_info_service.go` + `_test.go`
- `go/internal/agent/services/wifi_service.go` + `_test.go`
- `go/internal/agent/services/bluetooth_service.go` + `_test.go`
- `go/internal/agent/services/agent_update_service.go`
- `go/internal/agent/services/os_update_service.go`
- `go/internal/agent/services/container_service_v2.go` + `_test.go`
- `go/internal/agent/services/provisioning_service_v2.go`
- `go/internal/agent/services/audio_service_v2.go`
- `go/internal/agent/services/telemetry_service_v2.go`
- `go/internal/agent/services/file_sync_service_v2.go`

**Updated:**
- `go/cmd/wendy-agent/main.go` — register v2 services

---

## Task 1: Write all 11 v2 proto files

**Files:**
- Create: `Proto/wendy/agent/services/v2/shared.proto`
- Create: `Proto/wendy/agent/services/v2/device_info_service.proto`
- Create: `Proto/wendy/agent/services/v2/agent_update_service.proto`
- Create: `Proto/wendy/agent/services/v2/os_update_service.proto`
- Create: `Proto/wendy/agent/services/v2/wifi_service.proto`
- Create: `Proto/wendy/agent/services/v2/bluetooth_service.proto`
- Create: `Proto/wendy/agent/services/v2/container_service.proto`
- Create: `Proto/wendy/agent/services/v2/provisioning_service.proto`
- Create: `Proto/wendy/agent/services/v2/audio_service.proto`
- Create: `Proto/wendy/agent/services/v2/telemetry_service.proto`
- Create: `Proto/wendy/agent/services/v2/file_sync_service.proto`

- [ ] **Step 1: Create `Proto/wendy/agent/services/v2/shared.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

enum RestartPolicyMode {
    DEFAULT = 0;
    UNLESS_STOPPED = 1;
    NO = 2;
    ON_FAILURE = 3;
}

message RestartPolicy {
    RestartPolicyMode mode = 1;
    int32 on_failure_max_retries = 2;
}

enum AppRunningState {
    STOPPED = 0;
    RUNNING = 1;
}

message AppContainer {
    string app_name = 1;
    string app_version = 2;
    AppRunningState running_state = 3;
    uint32 failure_count = 4;
}
```

- [ ] **Step 2: Create `Proto/wendy/agent/services/v2/device_info_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyDeviceInfoService {
    rpc GetDeviceInfo(GetDeviceInfoRequest) returns (GetDeviceInfoResponse);
    rpc ListHardwareCapabilities(ListHardwareCapabilitiesRequest) returns (ListHardwareCapabilitiesResponse);
}

message GetDeviceInfoRequest {}

message GetDeviceInfoResponse {
    string version = 1;
    optional string os_version = 2;
    string os = 3;
    string cpu_architecture = 4;
    optional string public_key = 5;
    repeated string featureset = 6;
    optional string device_type = 7;
    optional bool has_gpu = 8;
    optional string gpu_vendor = 9;
    optional string jetpack_version = 10;
    optional string cuda_version = 11;
}

message ListHardwareCapabilitiesRequest {
    optional string category_filter = 1;
}

message ListHardwareCapabilitiesResponse {
    repeated HardwareCapability capabilities = 1;

    message HardwareCapability {
        string category = 1;
        string device_path = 2;
        string description = 3;
        map<string, string> properties = 4;
    }
}
```

- [ ] **Step 3: Create `Proto/wendy/agent/services/v2/agent_update_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyAgentUpdateService {
    rpc UpdateAgent(stream UpdateAgentRequest) returns (stream UpdateAgentResponse);
}

message UpdateAgentRequest {
    oneof request_type {
        Chunk chunk = 1;
        ControlCommand control = 2;
    }

    message Chunk {
        bytes data = 1;
    }

    message ControlCommand {
        oneof command {
            Update update = 1;
        }

        message Update {
            string sha256 = 1;
        }
    }
}

message UpdateAgentResponse {
    oneof response_type {
        Updated updated = 1;
    }

    message Updated {}
}
```

- [ ] **Step 4: Create `Proto/wendy/agent/services/v2/os_update_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyOSUpdateService {
    rpc UpdateOS(UpdateOSRequest) returns (stream UpdateOSResponse);
}

message UpdateOSRequest {
    string artifact_url = 1;
}

message UpdateOSResponse {
    oneof response_type {
        Progress progress = 1;
        Completed completed = 2;
        Failed failed = 3;
    }

    message Progress {
        string phase = 1;
        int32 percent = 2;
    }

    message Completed {
        bool reboot_required = 1;
    }

    message Failed {
        string error_message = 1;
    }
}
```

- [ ] **Step 5: Create `Proto/wendy/agent/services/v2/wifi_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyWiFiService {
    rpc ListWiFiNetworks(ListWiFiNetworksRequest) returns (ListWiFiNetworksResponse);
    rpc ConnectToWiFi(ConnectToWiFiRequest) returns (ConnectToWiFiResponse);
    rpc GetWiFiStatus(GetWiFiStatusRequest) returns (GetWiFiStatusResponse);
    rpc DisconnectWiFi(DisconnectWiFiRequest) returns (DisconnectWiFiResponse);
    rpc ListKnownWiFiNetworks(ListKnownWiFiNetworksRequest) returns (ListKnownWiFiNetworksResponse);
    rpc SetWiFiNetworkPriority(SetWiFiNetworkPriorityRequest) returns (SetWiFiNetworkPriorityResponse);
    rpc ReorderKnownWiFiNetworks(ReorderKnownWiFiNetworksRequest) returns (ReorderKnownWiFiNetworksResponse);
    rpc ForgetWiFiNetwork(ForgetWiFiNetworkRequest) returns (ForgetWiFiNetworkResponse);
}

enum WiFiSecurityType {
    WIFI_SECURITY_TYPE_UNSPECIFIED = 0;
    WIFI_SECURITY_TYPE_OPEN = 1;
    WIFI_SECURITY_TYPE_WEP = 2;
    WIFI_SECURITY_TYPE_WPA_PSK = 3;
    WIFI_SECURITY_TYPE_WPA2_PSK = 4;
    WIFI_SECURITY_TYPE_WPA3_SAE = 5;
    WIFI_SECURITY_TYPE_WPA2_ENTERPRISE = 6;
}

message ListWiFiNetworksRequest {}

message ListWiFiNetworksResponse {
    repeated WiFiNetwork networks = 1;

    message WiFiNetwork {
        string ssid = 1;
        optional int32 signal_strength = 2;
        WiFiSecurityType security = 3;
        bool is_known = 4;
        bool is_connected = 5;
        optional int32 priority = 6;
        optional int32 rssi_dbm = 7;
    }
}

message ConnectToWiFiRequest {
    string ssid = 1;
    string password = 2;
    optional WiFiSecurityType security = 3;
    optional bool hidden = 4;
}

message ConnectToWiFiResponse {
    bool success = 1;
    optional string error_message = 2;
}

message GetWiFiStatusRequest {}

message GetWiFiStatusResponse {
    bool connected = 1;
    optional string ssid = 2;
    optional string error_message = 3;
}

message DisconnectWiFiRequest {}

message DisconnectWiFiResponse {
    bool success = 1;
    optional string error_message = 2;
}

message ListKnownWiFiNetworksRequest {}

message ListKnownWiFiNetworksResponse {
    repeated KnownWiFiNetwork networks = 1;

    message KnownWiFiNetwork {
        string ssid = 1;
        string uuid = 2;
        int32 priority = 3;
        WiFiSecurityType security = 4;
    }
}

message SetWiFiNetworkPriorityRequest {
    string ssid = 1;
    int32 priority = 2;
}

message SetWiFiNetworkPriorityResponse {
    bool success = 1;
    optional string error_message = 2;
}

message ReorderKnownWiFiNetworksRequest {
    repeated string order_ssids = 1;
}

message ReorderKnownWiFiNetworksResponse {
    bool success = 1;
    optional string error_message = 2;
}

message ForgetWiFiNetworkRequest {
    string ssid = 1;
}

message ForgetWiFiNetworkResponse {
    bool success = 1;
    optional string error_message = 2;
}
```

- [ ] **Step 6: Create `Proto/wendy/agent/services/v2/bluetooth_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyBluetoothService {
    rpc ScanBluetoothPeripherals(stream ScanBluetoothPeripheralsRequest) returns (stream ScanBluetoothPeripheralsResponse);
    rpc ConnectBluetoothPeripheral(ConnectBluetoothPeripheralRequest) returns (ConnectBluetoothPeripheralResponse);
    rpc DisconnectBluetoothPeripheral(DisconnectBluetoothPeripheralRequest) returns (DisconnectBluetoothPeripheralResponse);
    rpc ForgetBluetoothPeripheral(ForgetBluetoothPeripheralRequest) returns (ForgetBluetoothPeripheralResponse);
}

message DiscoveredBluetoothPeripheral {
    string name = 1;
    string address = 2;
    int32 rssi = 3;
    string device_type = 4;
    bool paired = 5;
    bool connected = 6;
    bool trusted = 7;
}

message ScanBluetoothPeripheralsRequest {}

message ScanBluetoothPeripheralsResponse {
    repeated DiscoveredBluetoothPeripheral discovered_devices = 1;
}

message ConnectBluetoothPeripheralRequest {
    string address = 1;
    bool pair = 2;
    bool trust = 3;
}

message ConnectBluetoothPeripheralResponse {}

message DisconnectBluetoothPeripheralRequest {
    string address = 1;
}

message DisconnectBluetoothPeripheralResponse {}

message ForgetBluetoothPeripheralRequest {
    string address = 1;
}

message ForgetBluetoothPeripheralResponse {}
```

- [ ] **Step 7: Create `Proto/wendy/agent/services/v2/container_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

import "wendy/agent/services/v2/shared.proto";

service WendyContainerService {
    rpc StartContainer(StartContainerRequest) returns (stream ContainerStreamResponse);
    rpc AttachContainer(stream AttachContainerRequest) returns (stream ContainerStreamResponse);
    rpc StopContainer(StopContainerRequest) returns (StopContainerResponse);
    rpc DeleteContainer(DeleteContainerRequest) returns (DeleteContainerResponse);
    rpc ListContainers(ListContainersRequest) returns (stream ListContainersResponse);
    rpc ListVolumes(ListVolumesRequest) returns (ListVolumesResponse);
    rpc RemoveVolume(RemoveVolumeRequest) returns (RemoveVolumeResponse);
    rpc ListContainerStats(ListContainerStatsRequest) returns (ListContainerStatsResponse);
}

message StartContainerRequest {
    string app_name = 1;
}

message AttachContainerRequest {
    oneof request_type {
        string app_name = 1;
        bytes stdin_data = 2;
    }
}

message ContainerStreamResponse {
    oneof response_type {
        Started started = 1;
        ConsoleOutput stdout_output = 2;
        ConsoleOutput stderr_output = 3;
    }

    message Started {}

    message ConsoleOutput {
        bytes data = 1;
    }
}

message StopContainerRequest {
    string app_name = 1;
}

message StopContainerResponse {}

message DeleteContainerRequest {
    string app_name = 1;
    bool delete_image = 2;
    bool delete_volumes = 3;
}

message DeleteContainerResponse {}

message ListContainersRequest {}

message ListContainersResponse {
    AppContainer container = 1;
}

message ListVolumesRequest {}

message VolumeInfo {
    string name = 1;
    string path = 2;
    int64 size_bytes = 3;
    string created_at = 4;
    repeated string used_by = 5;
}

message ListVolumesResponse {
    repeated VolumeInfo volumes = 1;
}

message RemoveVolumeRequest {
    string name = 1;
}

message RemoveVolumeResponse {}

message ContainerStats {
    string app_name = 1;
    int64 memory_bytes = 2;
    int64 storage_bytes = 3;
}

message ListContainerStatsRequest {}

message ListContainerStatsResponse {
    repeated ContainerStats stats = 1;
}
```

- [ ] **Step 8: Create `Proto/wendy/agent/services/v2/provisioning_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyProvisioningService {
    rpc StartProvisioning(StartProvisioningRequest) returns (StartProvisioningResponse);
    rpc IsProvisioned(IsProvisionedRequest) returns (IsProvisionedResponse);
}

message IsProvisionedRequest {}

message IsProvisionedResponse {
    oneof response {
        NotProvisionedResponse not_provisioned = 1;
        ProvisionedResponse provisioned = 2;
    }
}

message NotProvisionedResponse {}

message ProvisionedResponse {
    string cloud_host = 1;
    int32 organization_id = 2;
    int32 asset_id = 3;
}

message StartProvisioningRequest {
    int32 organization_id = 1;
    string enrollment_token = 2;
    string cloud_host = 3;
    int32 asset_id = 4;
}

message StartProvisioningResponse {}
```

- [ ] **Step 9: Create `Proto/wendy/agent/services/v2/audio_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyAudioService {
    rpc ListAudioDevices(ListAudioDevicesRequest) returns (ListAudioDevicesResponse);
    rpc SetDefaultAudioDevice(SetDefaultAudioDeviceRequest) returns (SetDefaultAudioDeviceResponse);
    rpc StreamAudioLevels(StreamAudioLevelsRequest) returns (stream AudioLevelUpdate);
    rpc StreamAudio(StreamAudioRequest) returns (stream AudioChunk);
}

enum AudioDeviceType {
    AUDIO_DEVICE_TYPE_UNSPECIFIED = 0;
    AUDIO_DEVICE_TYPE_INPUT = 1;
    AUDIO_DEVICE_TYPE_OUTPUT = 2;
}

message AudioDevice {
    uint32 id = 1;
    string name = 2;
    string description = 3;
    AudioDeviceType type = 4;
    bool is_default = 5;
}

message ListAudioDevicesRequest {
    optional AudioDeviceType type_filter = 1;
}

message ListAudioDevicesResponse {
    repeated AudioDevice devices = 1;
}

message SetDefaultAudioDeviceRequest {
    uint32 device_id = 1;
}

message SetDefaultAudioDeviceResponse {
    bool success = 1;
    optional string error_message = 2;
}

message StreamAudioLevelsRequest {
    uint32 device_id = 1;
    uint32 update_rate_hz = 2;
}

message AudioLevelUpdate {
    float peak_db = 1;
    float rms_db = 2;
    uint64 timestamp_ns = 3;
}

message StreamAudioRequest {
    uint32 device_id = 1;
    uint32 sample_rate = 2;
    uint32 channels = 3;
}

message AudioChunk {
    bytes pcm_data = 1;
    uint64 timestamp_ns = 2;
    uint32 sample_rate = 3;
    uint32 channels = 4;
}
```

- [ ] **Step 10: Create `Proto/wendy/agent/services/v2/telemetry_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

import "opentelemetry/proto/collector/logs/v1/logs_service.proto";
import "opentelemetry/proto/collector/metrics/v1/metrics_service.proto";
import "opentelemetry/proto/collector/trace/v1/trace_service.proto";

service WendyTelemetryService {
    rpc StreamLogs(StreamLogsRequest) returns (stream StreamLogsResponse);
    rpc StreamMetrics(StreamMetricsRequest) returns (stream StreamMetricsResponse);
    rpc StreamTraces(StreamTracesRequest) returns (stream StreamTracesResponse);
}

message StreamLogsRequest {
    optional string service_name = 1;
    optional int32 min_severity = 2;
    optional string app_name = 3;
}

message StreamLogsResponse {
    opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest logs = 1;
}

message StreamMetricsRequest {
    optional string service_name = 1;
    optional string metric_name_prefix = 2;
    optional string app_name = 3;
}

message StreamMetricsResponse {
    opentelemetry.proto.collector.metrics.v1.ExportMetricsServiceRequest metrics = 1;
}

message StreamTracesRequest {
    optional string service_name = 1;
    optional string app_name = 2;
    optional string span_name_prefix = 3;
}

message StreamTracesResponse {
    opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest traces = 1;
}
```

- [ ] **Step 11: Create `Proto/wendy/agent/services/v2/file_sync_service.proto`**

```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2;agentpbv2";

service WendyFileSyncService {
    rpc SyncFiles(stream FileSyncRequest) returns (stream FileSyncResponse);
}

message FileSyncEntry {
    string path   = 1;
    int64  size   = 2;
    bytes  sha256 = 3;
    uint32 mode   = 4;
}

message FileSyncRequest {
    oneof request_type {
        FileSyncStart  start  = 1;
        FileSyncChunk  chunk  = 2;
        FileSyncCommit commit = 3;
        FileSyncChmod  chmod  = 4;
        FileSyncDelete delete = 5;
    }
}

message FileSyncStart {
    string           app_id   = 1;
    FileSyncManifest manifest = 2;
}

message FileSyncChunk {
    string path            = 1;
    bytes  data            = 2;
    uint64 sequence        = 3;
    int64  cumulative_size = 4;
    bytes  sha256          = 5;
}

message FileSyncCommit {
    string path   = 1;
    bytes  sha256 = 2;
    int64  size   = 3;
}

message FileSyncChmod {
    string path   = 1;
    uint32 mode   = 2;
    int64  size   = 3;
    bytes  sha256 = 4;
}

message FileSyncDelete {
    repeated string paths = 1;
}

message FileSyncResponse {
    oneof response_type {
        FileSyncManifest manifest = 1;
        FileSyncAck      ack      = 2;
        FileSyncComplete complete = 3;
    }
}

message FileSyncManifest {
    repeated FileSyncEntry files = 1;
}

message FileSyncAck {
    string path = 1;
}

message FileSyncComplete {}
```

- [ ] **Step 12: Commit all proto files**

```bash
cd /path/to/wendy-agent
git add Proto/wendy/agent/services/v2/
git commit -m "feat: add wendy.agent.services.v2 proto definitions"
```

---

## Task 2: Update generate-proto.sh and generate v2 Go code

**Files:**
- Modify: `go/scripts/generate-proto.sh`

- [ ] **Step 1: Add the v2 agent proto block to `go/scripts/generate-proto.sh`**

After the `AGENT_M_OPTS` block (around line 36) and before `OTEL_PROTOS`, insert:

```bash
# ---- Wendy Agent v2 protos ----
V2_AGENT_PKG="$MODULE/proto/gen/agentpb/v2"

V2_AGENT_PROTOS=(
    "wendy/agent/services/v2/shared.proto"
    "wendy/agent/services/v2/device_info_service.proto"
    "wendy/agent/services/v2/agent_update_service.proto"
    "wendy/agent/services/v2/os_update_service.proto"
    "wendy/agent/services/v2/wifi_service.proto"
    "wendy/agent/services/v2/bluetooth_service.proto"
    "wendy/agent/services/v2/container_service.proto"
    "wendy/agent/services/v2/provisioning_service.proto"
    "wendy/agent/services/v2/audio_service.proto"
    "wendy/agent/services/v2/telemetry_service.proto"
    "wendy/agent/services/v2/file_sync_service.proto"
)

V2_AGENT_M_OPTS=""
for p in "${V2_AGENT_PROTOS[@]}"; do
    V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go_opt=M${p}=${V2_AGENT_PKG}"
    V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go-grpc_opt=M${p}=${V2_AGENT_PKG}"
done
```

Then update the `ALL_M_OPTS` line to include `$V2_AGENT_M_OPTS`:

```bash
ALL_M_OPTS="$AGENT_M_OPTS $V2_AGENT_M_OPTS $OTEL_M_OPTS $CLOUD_M_OPTS"
```

Then add the v2 generation command **after** the existing "Generating Wendy Agent protos..." block:

```bash
echo "Generating Wendy Agent v2 protos..."
mkdir -p "$GEN_DIR/agentpb/v2"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/agentpb/v2" \
    --go_opt=module="$V2_AGENT_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/agentpb/v2" \
    --go-grpc_opt=module="$V2_AGENT_PKG" \
    ${V2_AGENT_PROTOS[@]}
```

- [ ] **Step 2: Run generation**

```bash
cd go
make proto
```

Expected: output ends with "Proto generation complete!" and no errors.

- [ ] **Step 3: Verify generated files exist**

```bash
ls go/proto/gen/agentpb/v2/
```

Expected: files like `device_info_service.pb.go`, `device_info_service_grpc.pb.go`, `wifi_service.pb.go`, etc.

- [ ] **Step 4: Verify the project still builds**

```bash
cd go && go build ./...
```

Expected: no errors (the new generated package is not yet imported anywhere so there are no compile errors).

- [ ] **Step 5: Commit**

```bash
git add go/scripts/generate-proto.sh go/proto/gen/agentpb/v2/
git commit -m "feat: generate v2 protobuf Go code"
```

---

## Task 3: WendyDeviceInfoService

**Files:**
- Create: `go/internal/agent/services/device_info_service.go`
- Create: `go/internal/agent/services/device_info_service_test.go`

`DeviceInfoService` exposes `GetDeviceInfo` (renamed from `GetAgentVersion`) and `ListHardwareCapabilities`. It reuses the unexported helpers `detectGPUInfo()`, `detectFeatureset()`, and `detectCUDAVersion()` that already live in `agent_service.go` within the same package.

- [ ] **Step 1: Write the failing test**

Create `go/internal/agent/services/device_info_service_test.go`:

```go
package services

import (
	"context"
	"net"
	"runtime"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

func startDeviceInfoServer(t *testing.T, hd HardwareDiscoverer) (agentpbv2.WendyDeviceInfoServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize) // bufSize = 1024*1024, defined in agent_service_test.go
	srv := grpc.NewServer()
	svc := NewDeviceInfoService(zap.NewNop(), hd)
	agentpbv2.RegisterWendyDeviceInfoServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyDeviceInfoServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestDeviceInfoService_GetDeviceInfo(t *testing.T) {
	client, cleanup := startDeviceInfoServer(t, &mockHardwareDiscoverer{})
	defer cleanup()

	resp, err := client.GetDeviceInfo(context.Background(), &agentpbv2.GetDeviceInfoRequest{})
	if err != nil {
		t.Fatalf("GetDeviceInfo: %v", err)
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

func TestDeviceInfoService_ListHardwareCapabilities(t *testing.T) {
	caps := []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
		{Category: "gpu", DevicePath: "/dev/nvidia0", Description: "NVIDIA GPU"},
	}
	client, cleanup := startDeviceInfoServer(t, &mockHardwareDiscoverer{caps: caps})
	defer cleanup()

	resp, err := client.ListHardwareCapabilities(context.Background(), &agentpbv2.ListHardwareCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ListHardwareCapabilities: %v", err)
	}
	if len(resp.Capabilities) != 1 {
		t.Fatalf("len(capabilities) = %d; want 1", len(resp.Capabilities))
	}
	if resp.Capabilities[0].Category != "gpu" {
		t.Errorf("category = %q; want gpu", resp.Capabilities[0].Category)
	}
	if resp.Capabilities[0].DevicePath != "/dev/nvidia0" {
		t.Errorf("device_path = %q; want /dev/nvidia0", resp.Capabilities[0].DevicePath)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd go && go test ./internal/agent/services/ -run TestDeviceInfoService -v
```

Expected: FAIL — `NewDeviceInfoService` is not defined.

- [ ] **Step 3: Implement `go/internal/agent/services/device_info_service.go`**

```go
package services

import (
	"context"
	"os"
	"runtime"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// DeviceInfoService implements agentpbv2.WendyDeviceInfoServiceServer.
type DeviceInfoService struct {
	agentpbv2.UnimplementedWendyDeviceInfoServiceServer
	logger             *zap.Logger
	hardwareDiscoverer HardwareDiscoverer
}

// NewDeviceInfoService creates a new DeviceInfoService.
func NewDeviceInfoService(logger *zap.Logger, hd HardwareDiscoverer) *DeviceInfoService {
	return &DeviceInfoService{logger: logger, hardwareDiscoverer: hd}
}

// GetDeviceInfo returns agent version, OS, architecture, GPU info, and featureset.
func (s *DeviceInfoService) GetDeviceInfo(_ context.Context, _ *agentpbv2.GetDeviceInfoRequest) (*agentpbv2.GetDeviceInfoResponse, error) {
	resp := &agentpbv2.GetDeviceInfoResponse{
		Version:         version.Version,
		Os:              runtime.GOOS,
		CpuArchitecture: runtime.GOARCH,
		Featureset:      detectFeatureset(),
	}

	if data, err := os.ReadFile("/etc/wendy/version.txt"); err == nil {
		v := strings.TrimSpace(string(data))
		resp.OsVersion = &v
	}

	if data, err := os.ReadFile("/etc/wendyos/device-type"); err == nil {
		v := strings.TrimSpace(string(data))
		resp.DeviceType = &v
	}

	gpuInfo := detectGPUInfo()
	resp.HasGpu = &gpuInfo.hasGPU
	if gpuInfo.vendor != "" {
		resp.GpuVendor = &gpuInfo.vendor
	}
	if gpuInfo.jetpackVersion != "" {
		resp.JetpackVersion = &gpuInfo.jetpackVersion
	}
	if gpuInfo.cudaVersion != "" {
		resp.CudaVersion = &gpuInfo.cudaVersion
	}

	return resp, nil
}

// ListHardwareCapabilities discovers hardware on the device.
func (s *DeviceInfoService) ListHardwareCapabilities(ctx context.Context, req *agentpbv2.ListHardwareCapabilitiesRequest) (*agentpbv2.ListHardwareCapabilitiesResponse, error) {
	caps, err := s.hardwareDiscoverer.Discover(ctx, req.GetCategoryFilter())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hardware discovery failed: %v", err)
	}
	v2caps := make([]*agentpbv2.ListHardwareCapabilitiesResponse_HardwareCapability, len(caps))
	for i, c := range caps {
		v2caps[i] = &agentpbv2.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    c.Category,
			DevicePath:  c.DevicePath,
			Description: c.Description,
			Properties:  c.Properties,
		}
	}
	return &agentpbv2.ListHardwareCapabilitiesResponse{Capabilities: v2caps}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd go && go test ./internal/agent/services/ -run TestDeviceInfoService -v
```

Expected: PASS for both `TestDeviceInfoService_GetDeviceInfo` and `TestDeviceInfoService_ListHardwareCapabilities`.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/device_info_service.go go/internal/agent/services/device_info_service_test.go
git commit -m "feat: add WendyDeviceInfoService v2 implementation"
```

---

## Task 4: WendyWiFiService

**Files:**
- Create: `go/internal/agent/services/wifi_service.go`
- Create: `go/internal/agent/services/wifi_service_test.go`

`WiFiService` takes the existing `NetworkManager` interface (defined in `interfaces.go`, uses v1 proto types) and converts results to v2 types at the gRPC boundary.

- [ ] **Step 1: Write the failing test**

Create `go/internal/agent/services/wifi_service_test.go`:

```go
package services

import (
	"context"
	"net"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

func startWiFiServer(t *testing.T, nm NetworkManager) (agentpbv2.WendyWiFiServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	svc := NewWiFiService(zap.NewNop(), nm)
	agentpbv2.RegisterWendyWiFiServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyWiFiServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestWiFiService_ListWiFiNetworks(t *testing.T) {
	nets := []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "HomeWiFi", IsConnected: true},
		{Ssid: "OfficeWiFi"},
	}
	client, cleanup := startWiFiServer(t, &mockNetworkManager{networks: nets})
	defer cleanup()

	resp, err := client.ListWiFiNetworks(context.Background(), &agentpbv2.ListWiFiNetworksRequest{})
	if err != nil {
		t.Fatalf("ListWiFiNetworks: %v", err)
	}
	if len(resp.Networks) != 2 {
		t.Fatalf("len(networks) = %d; want 2", len(resp.Networks))
	}
	if resp.Networks[0].Ssid != "HomeWiFi" {
		t.Errorf("networks[0].ssid = %q; want HomeWiFi", resp.Networks[0].Ssid)
	}
	if !resp.Networks[0].IsConnected {
		t.Errorf("networks[0].is_connected = false; want true")
	}
}

func TestWiFiService_ListWiFiNetworks_Unavailable(t *testing.T) {
	client, cleanup := startWiFiServer(t, nil)
	defer cleanup()

	_, err := client.ListWiFiNetworks(context.Background(), &agentpbv2.ListWiFiNetworksRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("error code = %v; want Unavailable", status.Code(err))
	}
}

func TestWiFiService_ConnectToWiFi(t *testing.T) {
	client, cleanup := startWiFiServer(t, &mockNetworkManager{})
	defer cleanup()

	resp, err := client.ConnectToWiFi(context.Background(), &agentpbv2.ConnectToWiFiRequest{
		Ssid:     "TestNet",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("ConnectToWiFi: %v", err)
	}
	if !resp.Success {
		t.Errorf("success = false; want true")
	}
}

func TestWiFiService_GetWiFiStatus(t *testing.T) {
	ssid := "HomeWiFi"
	client, cleanup := startWiFiServer(t, &mockNetworkManager{connected: true, ssid: ssid})
	defer cleanup()

	resp, err := client.GetWiFiStatus(context.Background(), &agentpbv2.GetWiFiStatusRequest{})
	if err != nil {
		t.Fatalf("GetWiFiStatus: %v", err)
	}
	if !resp.Connected {
		t.Errorf("connected = false; want true")
	}
	if resp.Ssid == nil || *resp.Ssid != ssid {
		t.Errorf("ssid = %v; want %q", resp.Ssid, ssid)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd go && go test ./internal/agent/services/ -run TestWiFiService -v
```

Expected: FAIL — `NewWiFiService` is not defined.

- [ ] **Step 3: Implement `go/internal/agent/services/wifi_service.go`**

```go
package services

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// WiFiService implements agentpbv2.WendyWiFiServiceServer.
type WiFiService struct {
	agentpbv2.UnimplementedWendyWiFiServiceServer
	logger         *zap.Logger
	networkManager NetworkManager
}

// NewWiFiService creates a new WiFiService.
func NewWiFiService(logger *zap.Logger, nm NetworkManager) *WiFiService {
	return &WiFiService{logger: logger, networkManager: nm}
}

func (s *WiFiService) ListWiFiNetworks(ctx context.Context, _ *agentpbv2.ListWiFiNetworksRequest) (*agentpbv2.ListWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	networks, err := s.networkManager.ListWiFiNetworks(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list WiFi networks: %v", err)
	}
	v2nets := make([]*agentpbv2.ListWiFiNetworksResponse_WiFiNetwork, len(networks))
	for i, n := range networks {
		v2nets[i] = mapWiFiNetworkToV2(n)
	}
	return &agentpbv2.ListWiFiNetworksResponse{Networks: v2nets}, nil
}

func (s *WiFiService) ConnectToWiFi(ctx context.Context, req *agentpbv2.ConnectToWiFiRequest) (*agentpbv2.ConnectToWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	v1req := &agentpb.ConnectToWiFiRequest{Ssid: req.Ssid, Password: req.Password}
	if req.Security != nil {
		sec := agentpb.WiFiSecurityType(*req.Security)
		v1req.Security = &sec
	}
	if req.Hidden != nil {
		v1req.Hidden = req.Hidden
	}
	if err := s.networkManager.ConnectToWiFi(ctx, v1req); err != nil {
		msg := err.Error()
		return &agentpbv2.ConnectToWiFiResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpbv2.ConnectToWiFiResponse{Success: true}, nil
}

func (s *WiFiService) GetWiFiStatus(ctx context.Context, _ *agentpbv2.GetWiFiStatusRequest) (*agentpbv2.GetWiFiStatusResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	connected, ssid, err := s.networkManager.GetWiFiStatus(ctx)
	if err != nil {
		msg := err.Error()
		return &agentpbv2.GetWiFiStatusResponse{ErrorMessage: &msg}, nil
	}
	return &agentpbv2.GetWiFiStatusResponse{Connected: connected, Ssid: &ssid}, nil
}

func (s *WiFiService) DisconnectWiFi(ctx context.Context, _ *agentpbv2.DisconnectWiFiRequest) (*agentpbv2.DisconnectWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.DisconnectWiFi(ctx); err != nil {
		msg := err.Error()
		return &agentpbv2.DisconnectWiFiResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpbv2.DisconnectWiFiResponse{Success: true}, nil
}

func (s *WiFiService) ListKnownWiFiNetworks(ctx context.Context, _ *agentpbv2.ListKnownWiFiNetworksRequest) (*agentpbv2.ListKnownWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	known, err := s.networkManager.ListKnownWiFiNetworks(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list known WiFi networks: %v", err)
	}
	v2known := make([]*agentpbv2.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, len(known))
	for i, k := range known {
		v2known[i] = &agentpbv2.ListKnownWiFiNetworksResponse_KnownWiFiNetwork{
			Ssid:     k.Ssid,
			Uuid:     k.Uuid,
			Priority: k.Priority,
			Security: agentpbv2.WiFiSecurityType(k.Security),
		}
	}
	return &agentpbv2.ListKnownWiFiNetworksResponse{Networks: v2known}, nil
}

func (s *WiFiService) SetWiFiNetworkPriority(ctx context.Context, req *agentpbv2.SetWiFiNetworkPriorityRequest) (*agentpbv2.SetWiFiNetworkPriorityResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.SetWiFiNetworkPriority(ctx, req.GetSsid(), req.GetPriority()); err != nil {
		msg := err.Error()
		return &agentpbv2.SetWiFiNetworkPriorityResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpbv2.SetWiFiNetworkPriorityResponse{Success: true}, nil
}

func (s *WiFiService) ReorderKnownWiFiNetworks(ctx context.Context, req *agentpbv2.ReorderKnownWiFiNetworksRequest) (*agentpbv2.ReorderKnownWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ReorderKnownWiFiNetworks(ctx, req.GetOrderSsids()); err != nil {
		msg := err.Error()
		return &agentpbv2.ReorderKnownWiFiNetworksResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpbv2.ReorderKnownWiFiNetworksResponse{Success: true}, nil
}

func (s *WiFiService) ForgetWiFiNetwork(ctx context.Context, req *agentpbv2.ForgetWiFiNetworkRequest) (*agentpbv2.ForgetWiFiNetworkResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ForgetWiFiNetwork(ctx, req.GetSsid()); err != nil {
		msg := err.Error()
		return &agentpbv2.ForgetWiFiNetworkResponse{Success: false, ErrorMessage: &msg}, nil
	}
	return &agentpbv2.ForgetWiFiNetworkResponse{Success: true}, nil
}

func mapWiFiNetworkToV2(n *agentpb.ListWiFiNetworksResponse_WiFiNetwork) *agentpbv2.ListWiFiNetworksResponse_WiFiNetwork {
	return &agentpbv2.ListWiFiNetworksResponse_WiFiNetwork{
		Ssid:           n.Ssid,
		SignalStrength: n.SignalStrength,
		Security:       agentpbv2.WiFiSecurityType(n.Security),
		IsKnown:        n.IsKnown,
		IsConnected:    n.IsConnected,
		Priority:       n.Priority,
		RssiDbm:        n.RssiDbm,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd go && go test ./internal/agent/services/ -run TestWiFiService -v
```

Expected: PASS for all four `TestWiFiService_*` tests.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/wifi_service.go go/internal/agent/services/wifi_service_test.go
git commit -m "feat: add WendyWiFiService v2 implementation"
```

---

## Task 5: WendyBluetoothService

**Files:**
- Create: `go/internal/agent/services/bluetooth_service.go`
- Create: `go/internal/agent/services/bluetooth_service_test.go`

`BluetoothService` takes the existing `BluetoothManager` interface and converts to v2 types. The `BluetoothManager.Scan` method returns `<-chan []*agentpb.DiscoveredBluetoothPeripheral` (v1 types); the v2 service converts each batch before sending.

- [ ] **Step 1: Write the failing test**

Create `go/internal/agent/services/bluetooth_service_test.go`:

```go
package services

import (
	"context"
	"net"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

func startBluetoothServer(t *testing.T, bm BluetoothManager) (agentpbv2.WendyBluetoothServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	svc := NewBluetoothService(zap.NewNop(), bm)
	agentpbv2.RegisterWendyBluetoothServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyBluetoothServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestBluetoothService_ScanReturnsEmpty(t *testing.T) {
	client, cleanup := startBluetoothServer(t, &mockBluetoothManager{})
	defer cleanup()

	stream, err := client.ScanBluetoothPeripherals(context.Background())
	if err != nil {
		t.Fatalf("ScanBluetoothPeripherals: %v", err)
	}
	if err := stream.Send(&agentpbv2.ScanBluetoothPeripheralsRequest{}); err != nil {
		t.Fatalf("stream.Send: %v", err)
	}
	stream.CloseSend()

	_, err = stream.Recv()
	// mockBluetoothManager closes the channel immediately, server returns nil → EOF
	if err == nil {
		// received one response — also fine
	}
}

func TestBluetoothService_ConnectDisconnectForget(t *testing.T) {
	client, cleanup := startBluetoothServer(t, &mockBluetoothManager{})
	defer cleanup()

	if _, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpbv2.ConnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); err != nil {
		t.Fatalf("ConnectBluetoothPeripheral: %v", err)
	}
	if _, err := client.DisconnectBluetoothPeripheral(context.Background(), &agentpbv2.DisconnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); err != nil {
		t.Fatalf("DisconnectBluetoothPeripheral: %v", err)
	}
	if _, err := client.ForgetBluetoothPeripheral(context.Background(), &agentpbv2.ForgetBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); err != nil {
		t.Fatalf("ForgetBluetoothPeripheral: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd go && go test ./internal/agent/services/ -run TestBluetoothService -v
```

Expected: FAIL — `NewBluetoothService` is not defined.

- [ ] **Step 3: Implement `go/internal/agent/services/bluetooth_service.go`**

```go
package services

import (
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// BluetoothService implements agentpbv2.WendyBluetoothServiceServer.
type BluetoothService struct {
	agentpbv2.UnimplementedWendyBluetoothServiceServer
	logger           *zap.Logger
	bluetoothManager BluetoothManager
}

// NewBluetoothService creates a new BluetoothService.
func NewBluetoothService(logger *zap.Logger, bm BluetoothManager) *BluetoothService {
	return &BluetoothService{logger: logger, bluetoothManager: bm}
}

// ScanBluetoothPeripherals streams discovered Bluetooth peripherals.
func (s *BluetoothService) ScanBluetoothPeripherals(stream grpc.BidiStreamingServer[agentpbv2.ScanBluetoothPeripheralsRequest, agentpbv2.ScanBluetoothPeripheralsResponse]) error {
	ctx := stream.Context()
	ch, err := s.bluetoothManager.Scan(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start bluetooth scan: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case peripherals, ok := <-ch:
			if !ok {
				return nil
			}
			v2devs := make([]*agentpbv2.DiscoveredBluetoothPeripheral, len(peripherals))
			for i, p := range peripherals {
				v2devs[i] = mapBluetoothPeripheralToV2(p)
			}
			if err := stream.Send(&agentpbv2.ScanBluetoothPeripheralsResponse{DiscoveredDevices: v2devs}); err != nil {
				return err
			}
		}
	}
}

// ConnectBluetoothPeripheral connects to a Bluetooth peripheral.
func (s *BluetoothService) ConnectBluetoothPeripheral(ctx interface{ Done() <-chan struct{} }, req *agentpbv2.ConnectBluetoothPeripheralRequest) (*agentpbv2.ConnectBluetoothPeripheralResponse, error) {
	return nil, nil
}

func (s *BluetoothService) connectBluetoothPeripheral(ctx interface{}, req *agentpbv2.ConnectBluetoothPeripheralRequest) (*agentpbv2.ConnectBluetoothPeripheralResponse, error) {
	return nil, nil
}
```

Wait — the unary RPCs need the standard `context.Context`. Let me rewrite this properly:

```go
package services

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// BluetoothService implements agentpbv2.WendyBluetoothServiceServer.
type BluetoothService struct {
	agentpbv2.UnimplementedWendyBluetoothServiceServer
	logger           *zap.Logger
	bluetoothManager BluetoothManager
}

// NewBluetoothService creates a new BluetoothService.
func NewBluetoothService(logger *zap.Logger, bm BluetoothManager) *BluetoothService {
	return &BluetoothService{logger: logger, bluetoothManager: bm}
}

func (s *BluetoothService) ScanBluetoothPeripherals(stream grpc.BidiStreamingServer[agentpbv2.ScanBluetoothPeripheralsRequest, agentpbv2.ScanBluetoothPeripheralsResponse]) error {
	ctx := stream.Context()
	ch, err := s.bluetoothManager.Scan(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start bluetooth scan: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case peripherals, ok := <-ch:
			if !ok {
				return nil
			}
			v2devs := make([]*agentpbv2.DiscoveredBluetoothPeripheral, len(peripherals))
			for i, p := range peripherals {
				v2devs[i] = mapBluetoothPeripheralToV2(p)
			}
			if err := stream.Send(&agentpbv2.ScanBluetoothPeripheralsResponse{DiscoveredDevices: v2devs}); err != nil {
				return err
			}
		}
	}
}

func (s *BluetoothService) ConnectBluetoothPeripheral(ctx context.Context, req *agentpbv2.ConnectBluetoothPeripheralRequest) (*agentpbv2.ConnectBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Connect(ctx, req.GetAddress(), req.GetPair(), req.GetTrust()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to connect bluetooth peripheral: %v", err)
	}
	return &agentpbv2.ConnectBluetoothPeripheralResponse{}, nil
}

func (s *BluetoothService) DisconnectBluetoothPeripheral(ctx context.Context, req *agentpbv2.DisconnectBluetoothPeripheralRequest) (*agentpbv2.DisconnectBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Disconnect(ctx, req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to disconnect bluetooth peripheral: %v", err)
	}
	return &agentpbv2.DisconnectBluetoothPeripheralResponse{}, nil
}

func (s *BluetoothService) ForgetBluetoothPeripheral(ctx context.Context, req *agentpbv2.ForgetBluetoothPeripheralRequest) (*agentpbv2.ForgetBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Forget(ctx, req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to forget bluetooth peripheral: %v", err)
	}
	return &agentpbv2.ForgetBluetoothPeripheralResponse{}, nil
}

func mapBluetoothPeripheralToV2(p *agentpb.DiscoveredBluetoothPeripheral) *agentpbv2.DiscoveredBluetoothPeripheral {
	return &agentpbv2.DiscoveredBluetoothPeripheral{
		Name:       p.Name,
		Address:    p.Address,
		Rssi:       p.Rssi,
		DeviceType: p.DeviceType,
		Paired:     p.Paired,
		Connected:  p.Connected,
		Trusted:    p.Trusted,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd go && go test ./internal/agent/services/ -run TestBluetoothService -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/bluetooth_service.go go/internal/agent/services/bluetooth_service_test.go
git commit -m "feat: add WendyBluetoothService v2 implementation"
```

---

## Task 6: WendyAgentUpdateService

**Files:**
- Create: `go/internal/agent/services/agent_update_service.go`

`AgentUpdateService` duplicates the `UpdateAgent` streaming logic from `agent_service.go` with v2 types. All helper functions used (`s.logger`, sha256 verification, binary replacement) stay in `agent_service.go` — this file just re-expresses the same logic with `agentpbv2` types instead of `agentpb`.

- [ ] **Step 1: Create `go/internal/agent/services/agent_update_service.go`**

```go
package services

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// AgentUpdateService implements agentpbv2.WendyAgentUpdateServiceServer.
type AgentUpdateService struct {
	agentpbv2.UnimplementedWendyAgentUpdateServiceServer
	logger     *zap.Logger
	updateMu   sync.Mutex
	isUpdating bool
}

// NewAgentUpdateService creates a new AgentUpdateService.
func NewAgentUpdateService(logger *zap.Logger) *AgentUpdateService {
	return &AgentUpdateService{logger: logger}
}

// UpdateAgent receives a binary upload and replaces the running agent binary.
func (s *AgentUpdateService) UpdateAgent(stream grpc.BidiStreamingServer[agentpbv2.UpdateAgentRequest, agentpbv2.UpdateAgentResponse]) error {
	s.updateMu.Lock()
	if s.isUpdating {
		s.updateMu.Unlock()
		return status.Error(codes.FailedPrecondition, "an update is already in progress")
	}
	s.isUpdating = true
	s.updateMu.Unlock()

	defer func() {
		s.updateMu.Lock()
		s.isUpdating = false
		s.updateMu.Unlock()
	}()

	s.logger.Info("UpdateAgent stream started")

	hasher := sha256.New()
	var binaryData []byte

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "error receiving update data: %v", err)
		}

		if chunk := msg.GetChunk(); chunk != nil {
			binaryData = append(binaryData, chunk.GetData()...)
			hasher.Write(chunk.GetData())
			continue
		}

		if ctrl := msg.GetControl(); ctrl != nil {
			if ctrl.GetUpdate() != nil {
				computedHash := hex.EncodeToString(hasher.Sum(nil))
				expectedHash := ctrl.GetUpdate().GetSha256()
				if expectedHash != "" && computedHash != expectedHash {
					return status.Errorf(codes.DataLoss,
						"SHA256 mismatch: expected %s, got %s", expectedHash, computedHash)
				}

				execPath, err := os.Executable()
				if err != nil {
					return status.Errorf(codes.Internal, "failed to get executable path: %v", err)
				}
				execPath, err = filepath.EvalSymlinks(execPath)
				if err != nil {
					return status.Errorf(codes.Internal, "failed to resolve executable symlinks: %v", err)
				}

				info, err := os.Stat(execPath)
				if err != nil {
					return status.Errorf(codes.Internal, "failed to stat executable: %v", err)
				}
				originalPerm := info.Mode()

				tmpPath := execPath + ".update"
				if err := os.WriteFile(tmpPath, binaryData, originalPerm); err != nil {
					return status.Errorf(codes.Internal, "failed to write update file: %v", err)
				}

				backupPath := execPath + ".backup"
				if err := os.Rename(execPath, backupPath); err != nil {
					os.Remove(tmpPath)
					return status.Errorf(codes.Internal, "failed to create backup: %v", err)
				}

				if err := os.Rename(tmpPath, execPath); err != nil {
					if rbErr := os.Rename(backupPath, execPath); rbErr != nil {
						s.logger.Error("Failed to rollback from backup",
							zap.Error(rbErr),
							zap.String("backup_path", backupPath),
						)
					}
					os.Remove(tmpPath)
					return status.Errorf(codes.Internal, "failed to replace binary: %v", err)
				}

				s.logger.Info("Agent binary updated successfully",
					zap.String("sha256", computedHash),
					zap.Int("size", len(binaryData)),
				)

				if err := stream.Send(&agentpbv2.UpdateAgentResponse{
					ResponseType: &agentpbv2.UpdateAgentResponse_Updated_{
						Updated: &agentpbv2.UpdateAgentResponse_Updated{},
					},
				}); err != nil {
					return err
				}

				go func() {
					time.Sleep(500 * time.Millisecond)
					os.Exit(0)
				}()

				return nil
			}
		}
	}

	return status.Error(codes.InvalidArgument, "update stream ended without update control command")
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd go && go build ./internal/agent/services/
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/agent/services/agent_update_service.go
git commit -m "feat: add WendyAgentUpdateService v2 implementation"
```

---

## Task 7: WendyOSUpdateService

**Files:**
- Create: `go/internal/agent/services/os_update_service.go`

`OSUpdateService` duplicates the `UpdateOS` logic from `agent_service.go` with v2 types. It can call the unexported package-level helpers `enableJetsonRootfsAB`, `resolveMenderBinary`, and `envWithPath` that are already defined in `agent_service.go` (same package).

- [ ] **Step 1: Create `go/internal/agent/services/os_update_service.go`**

```go
package services

import (
	"bufio"
	"fmt"
	"os/exec"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// OSUpdateService implements agentpbv2.WendyOSUpdateServiceServer.
type OSUpdateService struct {
	agentpbv2.UnimplementedWendyOSUpdateServiceServer
	logger *zap.Logger
}

// NewOSUpdateService creates a new OSUpdateService.
func NewOSUpdateService(logger *zap.Logger) *OSUpdateService {
	return &OSUpdateService{logger: logger}
}

// UpdateOS streams OS update progress using mender.
func (s *OSUpdateService) UpdateOS(req *agentpbv2.UpdateOSRequest, stream grpc.ServerStreamingServer[agentpbv2.UpdateOSResponse]) error {
	s.logger.Info("UpdateOS started", zap.String("artifact_url", req.GetArtifactUrl()))

	sendProgress := func(phase string, percent int32) {
		_ = stream.Send(&agentpbv2.UpdateOSResponse{
			ResponseType: &agentpbv2.UpdateOSResponse_Progress_{
				Progress: &agentpbv2.UpdateOSResponse_Progress{Phase: phase, Percent: percent},
			},
		})
	}

	if err := enableJetsonRootfsAB(s.logger); err != nil {
		return stream.Send(&agentpbv2.UpdateOSResponse{
			ResponseType: &agentpbv2.UpdateOSResponse_Failed_{
				Failed: &agentpbv2.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("Jetson A/B setup failed: %v", err),
				},
			},
		})
	}

	sendProgress("downloading", 0)
	cmdName, found := resolveMenderBinary()
	if !found {
		return stream.Send(&agentpbv2.UpdateOSResponse{
			ResponseType: &agentpbv2.UpdateOSResponse_Failed_{
				Failed: &agentpbv2.UpdateOSResponse_Failed{ErrorMessage: "mender-update binary not found"},
			},
		})
	}

	cmd := exec.CommandContext(stream.Context(), cmdName, "install", req.GetArtifactUrl())
	cmd.Env = envWithPath("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return stream.Send(&agentpbv2.UpdateOSResponse{
			ResponseType: &agentpbv2.UpdateOSResponse_Failed_{
				Failed: &agentpbv2.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("failed to create stderr pipe: %v", err),
				},
			},
		})
	}

	if err := cmd.Start(); err != nil {
		return stream.Send(&agentpbv2.UpdateOSResponse{
			ResponseType: &agentpbv2.UpdateOSResponse_Failed_{
				Failed: &agentpbv2.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("failed to start mender: %v", err),
				},
			},
		})
	}

	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if m := menderProgressRe.FindStringSubmatch(line); len(m) > 1 {
			if pct := parseInt32(m[1]); pct >= 0 {
				sendProgress("installing", pct)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return stream.Send(&agentpbv2.UpdateOSResponse{
			ResponseType: &agentpbv2.UpdateOSResponse_Failed_{
				Failed: &agentpbv2.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("mender failed: %v", err),
				},
			},
		})
	}

	rebootRequired := true
	return stream.Send(&agentpbv2.UpdateOSResponse{
		ResponseType: &agentpbv2.UpdateOSResponse_Completed_{
			Completed: &agentpbv2.UpdateOSResponse_Completed{RebootRequired: rebootRequired},
		},
	})
}

func parseInt32(s string) int32 {
	var n int32
	fmt.Sscanf(s, "%d", &n)
	return n
}
```

**Note:** `enableJetsonRootfsAB`, `resolveMenderBinary`, `envWithPath`, and `menderProgressRe` are all package-level identifiers in `agent_service.go` and are accessible from this file since they share package `services`.

- [ ] **Step 2: Verify it compiles**

```bash
cd go && go build ./internal/agent/services/
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/agent/services/os_update_service.go
git commit -m "feat: add WendyOSUpdateService v2 implementation"
```

---

## Task 8: WendyContainerServiceV2

**Files:**
- Create: `go/internal/agent/services/container_service_v2.go`
- Create: `go/internal/agent/services/container_service_v2_test.go`

`ContainerServiceV2` delegates to the v1 `ContainerService` struct via stream adapters. This reuses the log manager fan-out logic and the `streamContainerOutput` method without duplication.

- [ ] **Step 1: Write the failing test**

Create `go/internal/agent/services/container_service_v2_test.go`:

```go
package services

import (
	"context"
	"io"
	"net"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

func startContainerV2Server(t *testing.T, client ContainerdClient) (agentpbv2.WendyContainerServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	v1svc := NewContainerService(zap.NewNop(), client)
	svc := NewContainerServiceV2(v1svc)
	agentpbv2.RegisterWendyContainerServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyContainerServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestContainerServiceV2_StopContainer_NoClient(t *testing.T) {
	client, cleanup := startContainerV2Server(t, nil)
	defer cleanup()

	_, err := client.StopContainer(context.Background(), &agentpbv2.StopContainerRequest{AppName: "myapp"})
	if status.Code(err) != codes.Internal {
		t.Errorf("error code = %v; want Internal", status.Code(err))
	}
}

func TestContainerServiceV2_ListContainers_Empty(t *testing.T) {
	mc := &mockContainerdClient{}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	stream, err := client.ListContainers(context.Background(), &agentpbv2.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	_, err = stream.Recv()
	if err != io.EOF {
		t.Errorf("expected EOF for empty list, got %v", err)
	}
}
```

The test uses `mockContainerdClient` which is defined in `container_service_test.go` in the same package.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd go && go test ./internal/agent/services/ -run TestContainerServiceV2 -v
```

Expected: FAIL — `NewContainerServiceV2` is not defined.

- [ ] **Step 3: Implement `go/internal/agent/services/container_service_v2.go`**

```go
package services

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// ContainerServiceV2 implements agentpbv2.WendyContainerServiceServer.
// It delegates to the v1 ContainerService, using stream adapters to convert types.
type ContainerServiceV2 struct {
	agentpbv2.UnimplementedWendyContainerServiceServer
	v1 *ContainerService
}

// NewContainerServiceV2 creates a new ContainerServiceV2 wrapping the given v1 service.
func NewContainerServiceV2(v1 *ContainerService) *ContainerServiceV2 {
	return &ContainerServiceV2{v1: v1}
}

func (s *ContainerServiceV2) StartContainer(req *agentpbv2.StartContainerRequest, stream grpc.ServerStreamingServer[agentpbv2.ContainerStreamResponse]) error {
	return s.v1.streamContainerOutput(stream.Context(), req.GetAppName(), &containerStreamV1Adapter{v2stream: stream})
}

func (s *ContainerServiceV2) AttachContainer(stream grpc.BidiStreamingServer[agentpbv2.AttachContainerRequest, agentpbv2.ContainerStreamResponse]) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "missing first attach message")
	}
	if err != nil {
		return err
	}
	appName := first.GetAppName()
	if appName == "" {
		return status.Error(codes.InvalidArgument, "app_name required as first message")
	}

	ctx := stream.Context()
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()

	go func() {
		defer stdinW.Close()
		for {
			msg, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			if data := msg.GetStdinData(); len(data) > 0 {
				if _, writeErr := stdinW.Write(data); writeErr != nil {
					return
				}
			}
		}
	}()

	outputCh, err := s.v1.containerd.StartContainerWithStdin(ctx, appName, stdinR)
	if err != nil {
		stdinR.Close()
		return status.Errorf(codes.Internal, "failed to start container: %v", err)
	}

	if err := stream.Send(&agentpbv2.ContainerStreamResponse{
		ResponseType: &agentpbv2.ContainerStreamResponse_Started_{
			Started: &agentpbv2.ContainerStreamResponse_Started{},
		},
	}); err != nil {
		return err
	}

	var readCh <-chan ContainerOutput
	if s.v1.logManager != nil {
		subID, subCh := s.v1.logManager.Subscribe(appName)
		defer s.v1.logManager.Unsubscribe(appName, subID)
		readCh = subCh
		go func() {
			for output := range outputCh {
				s.v1.logManager.Publish(appName, output)
			}
			s.v1.logManager.Publish(appName, ContainerOutput{Done: true})
		}()
	} else {
		readCh = outputCh
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case output, ok := <-readCh:
			if !ok || output.Done {
				return nil
			}
			if len(output.Stdout) > 0 {
				if err := stream.Send(&agentpbv2.ContainerStreamResponse{
					ResponseType: &agentpbv2.ContainerStreamResponse_StdoutOutput{
						StdoutOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: output.Stdout},
					},
				}); err != nil {
					return err
				}
			}
			if len(output.Stderr) > 0 {
				if err := stream.Send(&agentpbv2.ContainerStreamResponse{
					ResponseType: &agentpbv2.ContainerStreamResponse_StderrOutput{
						StderrOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: output.Stderr},
					},
				}); err != nil {
					return err
				}
			}
		}
	}
}

func (s *ContainerServiceV2) StopContainer(ctx context.Context, req *agentpbv2.StopContainerRequest) (*agentpbv2.StopContainerResponse, error) {
	if s.v1.containerd == nil {
		return nil, status.Error(codes.Internal, "containerd is not available")
	}
	if err := s.v1.containerd.StopContainer(ctx, req.GetAppName()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to stop container: %v", err)
	}
	s.v1.logger.Info("Container stopped", zap.String("app_name", req.GetAppName()))
	return &agentpbv2.StopContainerResponse{}, nil
}

func (s *ContainerServiceV2) DeleteContainer(ctx context.Context, req *agentpbv2.DeleteContainerRequest) (*agentpbv2.DeleteContainerResponse, error) {
	if s.v1.containerd == nil {
		return nil, status.Error(codes.Internal, "containerd is not available")
	}
	if err := s.v1.containerd.DeleteContainer(ctx, req.GetAppName(), req.GetDeleteImage()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete container: %v", err)
	}
	if req.GetDeleteVolumes() {
		deleteVolumesByAppName(s.v1.logger, req.GetAppName())
	}
	return &agentpbv2.DeleteContainerResponse{}, nil
}

func (s *ContainerServiceV2) ListContainers(_ *agentpbv2.ListContainersRequest, stream grpc.ServerStreamingServer[agentpbv2.ListContainersResponse]) error {
	if s.v1.containerd == nil {
		return nil
	}
	containers, err := s.v1.containerd.ListContainers(stream.Context())
	if err != nil {
		return status.Errorf(codes.Internal, "failed to list containers: %v", err)
	}
	for _, c := range containers {
		if err := stream.Send(&agentpbv2.ListContainersResponse{
			Container: mapAppContainerToV2(c),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *ContainerServiceV2) ListVolumes(ctx context.Context, _ *agentpbv2.ListVolumesRequest) (*agentpbv2.ListVolumesResponse, error) {
	v1resp, err := s.v1.ListVolumes(ctx, &agentpb.ListVolumesRequest{})
	if err != nil {
		return nil, err
	}
	v2vols := make([]*agentpbv2.VolumeInfo, len(v1resp.Volumes))
	for i, v := range v1resp.Volumes {
		v2vols[i] = &agentpbv2.VolumeInfo{
			Name:      v.Name,
			Path:      v.Path,
			SizeBytes: v.SizeBytes,
			CreatedAt: v.CreatedAt,
			UsedBy:    v.UsedBy,
		}
	}
	return &agentpbv2.ListVolumesResponse{Volumes: v2vols}, nil
}

func (s *ContainerServiceV2) RemoveVolume(ctx context.Context, req *agentpbv2.RemoveVolumeRequest) (*agentpbv2.RemoveVolumeResponse, error) {
	if _, err := s.v1.RemoveVolume(ctx, &agentpb.RemoveVolumeRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return &agentpbv2.RemoveVolumeResponse{}, nil
}

func (s *ContainerServiceV2) ListContainerStats(ctx context.Context, _ *agentpbv2.ListContainerStatsRequest) (*agentpbv2.ListContainerStatsResponse, error) {
	if s.v1.containerd == nil {
		return &agentpbv2.ListContainerStatsResponse{}, nil
	}
	stats, err := s.v1.containerd.GetContainerStats(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get container stats: %v", err)
	}
	v2stats := make([]*agentpbv2.ContainerStats, len(stats))
	for i, st := range stats {
		v2stats[i] = &agentpbv2.ContainerStats{
			AppName:      st.AppName,
			MemoryBytes:  st.MemoryBytes,
			StorageBytes: st.StorageBytes,
		}
	}
	return &agentpbv2.ListContainerStatsResponse{Stats: v2stats}, nil
}

// deleteVolumesByAppName removes persistent volume directories for an app.
func deleteVolumesByAppName(logger *zap.Logger, appName string) {
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		logger.Warn("Failed to read volumes directory",
			zap.String("base", volumesDir),
			zap.String("app_name", appName),
			zap.Error(err),
		)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == appName || strings.HasPrefix(name, appName+"-") {
			path := filepath.Join(volumesDir, name)
			if err := os.RemoveAll(path); err != nil {
				logger.Warn("Failed to remove volume", zap.String("path", path), zap.Error(err))
			} else {
				logger.Info("Volume removed", zap.String("path", path))
			}
		}
	}
}

// mapAppContainerToV2 converts a v1 AppContainer to v2.
func mapAppContainerToV2(c *agentpb.AppContainer) *agentpbv2.AppContainer {
	return &agentpbv2.AppContainer{
		AppName:      c.AppName,
		AppVersion:   c.AppVersion,
		RunningState: agentpbv2.AppRunningState(c.RunningState),
		FailureCount: c.FailureCount,
	}
}

// containerStreamV1Adapter bridges v1 RunContainerLayersResponse to v2 ContainerStreamResponse.
// It satisfies grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse].
type containerStreamV1Adapter struct {
	v2stream grpc.ServerStreamingServer[agentpbv2.ContainerStreamResponse]
}

func (a *containerStreamV1Adapter) Send(resp *agentpb.RunContainerLayersResponse) error {
	switch t := resp.ResponseType.(type) {
	case *agentpb.RunContainerLayersResponse_Started_:
		return a.v2stream.Send(&agentpbv2.ContainerStreamResponse{
			ResponseType: &agentpbv2.ContainerStreamResponse_Started_{
				Started: &agentpbv2.ContainerStreamResponse_Started{},
			},
		})
	case *agentpb.RunContainerLayersResponse_StdoutOutput:
		return a.v2stream.Send(&agentpbv2.ContainerStreamResponse{
			ResponseType: &agentpbv2.ContainerStreamResponse_StdoutOutput{
				StdoutOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: t.StdoutOutput.Data},
			},
		})
	case *agentpb.RunContainerLayersResponse_StderrOutput:
		return a.v2stream.Send(&agentpbv2.ContainerStreamResponse{
			ResponseType: &agentpbv2.ContainerStreamResponse_StderrOutput{
				StderrOutput: &agentpbv2.ContainerStreamResponse_ConsoleOutput{Data: t.StderrOutput.Data},
			},
		})
	}
	return nil
}

func (a *containerStreamV1Adapter) SetHeader(md metadata.MD) error  { return a.v2stream.SetHeader(md) }
func (a *containerStreamV1Adapter) SendHeader(md metadata.MD) error { return a.v2stream.SendHeader(md) }
func (a *containerStreamV1Adapter) SetTrailer(md metadata.MD)       { a.v2stream.SetTrailer(md) }
func (a *containerStreamV1Adapter) Context() context.Context         { return a.v2stream.Context() }
func (a *containerStreamV1Adapter) SendMsg(m any) error              { return a.v2stream.SendMsg(m) }
func (a *containerStreamV1Adapter) RecvMsg(m any) error              { return a.v2stream.RecvMsg(m) }
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd go && go test ./internal/agent/services/ -run TestContainerServiceV2 -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/container_service_v2.go go/internal/agent/services/container_service_v2_test.go
git commit -m "feat: add WendyContainerServiceV2 implementation"
```

---

## Task 9: WendyProvisioningServiceV2

**Files:**
- Create: `go/internal/agent/services/provisioning_service_v2.go`

`ProvisioningServiceV2` wraps the v1 `ProvisioningService`. Both RPCs are unary, so no stream adapters are needed — just delegate and convert types.

- [ ] **Step 1: Create `go/internal/agent/services/provisioning_service_v2.go`**

```go
package services

import (
	"context"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// ProvisioningServiceV2 implements agentpbv2.WendyProvisioningServiceServer.
type ProvisioningServiceV2 struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	v1 *ProvisioningService
}

// NewProvisioningServiceV2 creates a new ProvisioningServiceV2 wrapping the given v1 service.
func NewProvisioningServiceV2(v1 *ProvisioningService) *ProvisioningServiceV2 {
	return &ProvisioningServiceV2{v1: v1}
}

func (s *ProvisioningServiceV2) IsProvisioned(ctx context.Context, _ *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	resp, err := s.v1.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetNotProvisioned() != nil {
		return &agentpbv2.IsProvisionedResponse{
			Response: &agentpbv2.IsProvisionedResponse_NotProvisioned{
				NotProvisioned: &agentpbv2.NotProvisionedResponse{},
			},
		}, nil
	}
	p := resp.GetProvisioned()
	return &agentpbv2.IsProvisionedResponse{
		Response: &agentpbv2.IsProvisionedResponse_Provisioned{
			Provisioned: &agentpbv2.ProvisionedResponse{
				CloudHost:      p.CloudHost,
				OrganizationId: p.OrganizationId,
				AssetId:        p.AssetId,
			},
		},
	}, nil
}

func (s *ProvisioningServiceV2) StartProvisioning(ctx context.Context, req *agentpbv2.StartProvisioningRequest) (*agentpbv2.StartProvisioningResponse, error) {
	if _, err := s.v1.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		OrganizationId:  req.OrganizationId,
		EnrollmentToken: req.EnrollmentToken,
		CloudHost:       req.CloudHost,
		AssetId:         req.AssetId,
	}); err != nil {
		return nil, err
	}
	return &agentpbv2.StartProvisioningResponse{}, nil
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd go && go build ./internal/agent/services/
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/agent/services/provisioning_service_v2.go
git commit -m "feat: add WendyProvisioningServiceV2 implementation"
```

---

## Task 10: WendyAudioServiceV2

**Files:**
- Create: `go/internal/agent/services/audio_service_v2.go`

`AudioServiceV2` wraps the v1 `AudioService`. Unary RPCs delegate with type conversion. Streaming RPCs use a `grpc.ServerStreamingServer` adapter that converts v1 message types to v2 on `Send`.

- [ ] **Step 1: Create `go/internal/agent/services/audio_service_v2.go`**

```go
package services

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// AudioServiceV2 implements agentpbv2.WendyAudioServiceServer.
type AudioServiceV2 struct {
	agentpbv2.UnimplementedWendyAudioServiceServer
	v1 *AudioService
}

// NewAudioServiceV2 creates a new AudioServiceV2 wrapping the given v1 service.
func NewAudioServiceV2(v1 *AudioService) *AudioServiceV2 {
	return &AudioServiceV2{v1: v1}
}

func (s *AudioServiceV2) ListAudioDevices(ctx context.Context, req *agentpbv2.ListAudioDevicesRequest) (*agentpbv2.ListAudioDevicesResponse, error) {
	v1req := &agentpb.ListAudioDevicesRequest{}
	if req.TypeFilter != nil {
		t := agentpb.AudioDeviceType(*req.TypeFilter)
		v1req.TypeFilter = &t
	}
	v1resp, err := s.v1.ListAudioDevices(ctx, v1req)
	if err != nil {
		return nil, err
	}
	devices := make([]*agentpbv2.AudioDevice, len(v1resp.Devices))
	for i, d := range v1resp.Devices {
		devices[i] = &agentpbv2.AudioDevice{
			Id:          d.Id,
			Name:        d.Name,
			Description: d.Description,
			Type:        agentpbv2.AudioDeviceType(d.Type),
			IsDefault:   d.IsDefault,
		}
	}
	return &agentpbv2.ListAudioDevicesResponse{Devices: devices}, nil
}

func (s *AudioServiceV2) SetDefaultAudioDevice(ctx context.Context, req *agentpbv2.SetDefaultAudioDeviceRequest) (*agentpbv2.SetDefaultAudioDeviceResponse, error) {
	v1resp, err := s.v1.SetDefaultAudioDevice(ctx, &agentpb.SetDefaultAudioDeviceRequest{DeviceId: req.DeviceId})
	if err != nil {
		return nil, err
	}
	return &agentpbv2.SetDefaultAudioDeviceResponse{
		Success:      v1resp.Success,
		ErrorMessage: v1resp.ErrorMessage,
	}, nil
}

func (s *AudioServiceV2) StreamAudioLevels(req *agentpbv2.StreamAudioLevelsRequest, stream grpc.ServerStreamingServer[agentpbv2.AudioLevelUpdate]) error {
	return s.v1.StreamAudioLevels(
		&agentpb.StreamAudioLevelsRequest{DeviceId: req.DeviceId, UpdateRateHz: req.UpdateRateHz},
		&audioLevelStreamAdapter{v2stream: stream},
	)
}

func (s *AudioServiceV2) StreamAudio(req *agentpbv2.StreamAudioRequest, stream grpc.ServerStreamingServer[agentpbv2.AudioChunk]) error {
	return s.v1.StreamAudio(
		&agentpb.StreamAudioRequest{DeviceId: req.DeviceId, SampleRate: req.SampleRate, Channels: req.Channels},
		&audioChunkStreamAdapter{v2stream: stream},
	)
}

// audioLevelStreamAdapter converts v1 AudioLevelUpdate sends to v2.
type audioLevelStreamAdapter struct {
	v2stream grpc.ServerStreamingServer[agentpbv2.AudioLevelUpdate]
}

func (a *audioLevelStreamAdapter) Send(u *agentpb.AudioLevelUpdate) error {
	return a.v2stream.Send(&agentpbv2.AudioLevelUpdate{
		PeakDb:      u.PeakDb,
		RmsDb:       u.RmsDb,
		TimestampNs: u.TimestampNs,
	})
}
func (a *audioLevelStreamAdapter) SetHeader(md metadata.MD) error  { return a.v2stream.SetHeader(md) }
func (a *audioLevelStreamAdapter) SendHeader(md metadata.MD) error { return a.v2stream.SendHeader(md) }
func (a *audioLevelStreamAdapter) SetTrailer(md metadata.MD)       { a.v2stream.SetTrailer(md) }
func (a *audioLevelStreamAdapter) Context() context.Context         { return a.v2stream.Context() }
func (a *audioLevelStreamAdapter) SendMsg(m any) error              { return a.v2stream.SendMsg(m) }
func (a *audioLevelStreamAdapter) RecvMsg(m any) error              { return a.v2stream.RecvMsg(m) }

// audioChunkStreamAdapter converts v1 AudioChunk sends to v2.
type audioChunkStreamAdapter struct {
	v2stream grpc.ServerStreamingServer[agentpbv2.AudioChunk]
}

func (a *audioChunkStreamAdapter) Send(c *agentpb.AudioChunk) error {
	return a.v2stream.Send(&agentpbv2.AudioChunk{
		PcmData:     c.PcmData,
		TimestampNs: c.TimestampNs,
		SampleRate:  c.SampleRate,
		Channels:    c.Channels,
	})
}
func (a *audioChunkStreamAdapter) SetHeader(md metadata.MD) error  { return a.v2stream.SetHeader(md) }
func (a *audioChunkStreamAdapter) SendHeader(md metadata.MD) error { return a.v2stream.SendHeader(md) }
func (a *audioChunkStreamAdapter) SetTrailer(md metadata.MD)       { a.v2stream.SetTrailer(md) }
func (a *audioChunkStreamAdapter) Context() context.Context         { return a.v2stream.Context() }
func (a *audioChunkStreamAdapter) SendMsg(m any) error              { return a.v2stream.SendMsg(m) }
func (a *audioChunkStreamAdapter) RecvMsg(m any) error              { return a.v2stream.RecvMsg(m) }
```

- [ ] **Step 2: Verify it compiles**

```bash
cd go && go build ./internal/agent/services/
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/agent/services/audio_service_v2.go
git commit -m "feat: add WendyAudioServiceV2 implementation"
```

---

## Task 11: WendyTelemetryServiceV2

**Files:**
- Create: `go/internal/agent/services/telemetry_service_v2.go`

`TelemetryServiceV2` wraps `TelemetryBroadcaster` directly (not the v1 `TelemetryService`). Since OTEL types (`*otelpb.*`) are shared between v1 and v2 — both protos import the same OTEL proto files — the response field `Logs`/`Metrics`/`Traces` uses the same Go type. Only the outer wrapper message type differs.

- [ ] **Step 1: Create `go/internal/agent/services/telemetry_service_v2.go`**

```go
package services

import (
	"go.uber.org/zap"
	"google.golang.org/grpc"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// TelemetryServiceV2 implements agentpbv2.WendyTelemetryServiceServer.
type TelemetryServiceV2 struct {
	agentpbv2.UnimplementedWendyTelemetryServiceServer
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
}

// NewTelemetryServiceV2 creates a new TelemetryServiceV2.
func NewTelemetryServiceV2(logger *zap.Logger, broadcaster *TelemetryBroadcaster) *TelemetryServiceV2 {
	return &TelemetryServiceV2{logger: logger, broadcaster: broadcaster}
}

func (s *TelemetryServiceV2) StreamLogs(req *agentpbv2.StreamLogsRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamLogsResponse]) error {
	subID, ch := s.broadcaster.SubscribeLogs()
	defer s.broadcaster.UnsubscribeLogs(subID)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case batch, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpbv2.StreamLogsResponse{Logs: batch}); err != nil {
				return err
			}
		}
	}
}

func (s *TelemetryServiceV2) StreamMetrics(req *agentpbv2.StreamMetricsRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamMetricsResponse]) error {
	subID, ch := s.broadcaster.SubscribeMetrics()
	defer s.broadcaster.UnsubscribeMetrics(subID)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case batch, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpbv2.StreamMetricsResponse{Metrics: batch}); err != nil {
				return err
			}
		}
	}
}

func (s *TelemetryServiceV2) StreamTraces(req *agentpbv2.StreamTracesRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamTracesResponse]) error {
	subID, ch := s.broadcaster.SubscribeTraces()
	defer s.broadcaster.UnsubscribeTraces(subID)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case batch, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpbv2.StreamTracesResponse{Traces: batch}); err != nil {
				return err
			}
		}
	}
}
```

**Note:** `SubscribeLogs`/`UnsubscribeLogs`, `SubscribeMetrics`/`UnsubscribeMetrics`, `SubscribeTraces`/`UnsubscribeTraces` are the exact method names on `TelemetryBroadcaster` as defined in `telemetry_service.go`.

- [ ] **Step 2: Verify it compiles**

```bash
cd go && go build ./internal/agent/services/
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add go/internal/agent/services/telemetry_service_v2.go
git commit -m "feat: add WendyTelemetryServiceV2 implementation"
```

---

## Task 12: WendyFileSyncServiceV2 (stub)

**Files:**
- Create: `go/internal/agent/services/file_sync_service_v2.go`

There is no server-side implementation of `FileSyncService` in the agent yet (neither v1 nor v2). This task creates a stub struct so that `ContainerServiceV2` can be registered on the server when a non-Linux implementation is eventually added. The stub is not registered anywhere in this PR.

- [ ] **Step 1: Create `go/internal/agent/services/file_sync_service_v2.go`**

```go
package services

import agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"

// FileSyncServiceV2 implements agentpbv2.WendyFileSyncServiceServer.
// The SyncFiles handler is not yet implemented — it returns Unimplemented.
type FileSyncServiceV2 struct {
	agentpbv2.UnimplementedWendyFileSyncServiceServer
}

// NewFileSyncServiceV2 creates a new FileSyncServiceV2 stub.
func NewFileSyncServiceV2() *FileSyncServiceV2 {
	return &FileSyncServiceV2{}
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd go && go build ./internal/agent/services/
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/agent/services/file_sync_service_v2.go
git commit -m "feat: add WendyFileSyncServiceV2 stub"
```

---

## Task 13: Register v2 services in main.go

**Files:**
- Modify: `go/cmd/wendy-agent/main.go`

Add v2 service construction and registration alongside v1. v1 registration is not changed.

- [ ] **Step 1: Add v2 imports to `main.go`**

Add to the import block:
```go
agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
```

- [ ] **Step 2: Construct v2 services after the existing service construction block**

After the line `telemetrySvc := services.NewTelemetryService(logger, broadcaster)`, add:

```go
// v2 services
deviceInfoSvc := services.NewDeviceInfoService(logger, hwDiscoverer)
wifiSvc := services.NewWiFiService(logger, networkMgr)
bluetoothSvc := services.NewBluetoothService(logger, btManager)
agentUpdateSvc := services.NewAgentUpdateService(logger)
osUpdateSvc := services.NewOSUpdateService(logger)
containerSvcV2 := services.NewContainerServiceV2(containerSvc)
provisioningSvcV2 := services.NewProvisioningServiceV2(provisioningSvc)
audioSvcV2 := services.NewAudioServiceV2(audioSvc)
telemetrySvcV2 := services.NewTelemetryServiceV2(logger, broadcaster)
```

- [ ] **Step 3: Add v2 registrations inside `registerAllServices`**

In the `registerAllServices` closure, after the existing v1 registrations, add:

```go
agentpbv2.RegisterWendyDeviceInfoServiceServer(srv, deviceInfoSvc)
agentpbv2.RegisterWendyWiFiServiceServer(srv, wifiSvc)
agentpbv2.RegisterWendyBluetoothServiceServer(srv, bluetoothSvc)
agentpbv2.RegisterWendyAgentUpdateServiceServer(srv, agentUpdateSvc)
agentpbv2.RegisterWendyOSUpdateServiceServer(srv, osUpdateSvc)
agentpbv2.RegisterWendyContainerServiceServer(srv, containerSvcV2)
agentpbv2.RegisterWendyProvisioningServiceServer(srv, provisioningSvcV2)
agentpbv2.RegisterWendyAudioServiceServer(srv, audioSvcV2)
agentpbv2.RegisterWendyTelemetryServiceServer(srv, telemetrySvcV2)
```

**Note:** `WendyFileSyncServiceV2` is intentionally not registered — it has no implementation yet.

**Note:** `agentpb.RegisterWendyProvisioningServiceServer` (v1) is already called separately on the plaintext server in two places when provisioning state differs. The new `agentpbv2.RegisterWendyProvisioningServiceServer` should only be added inside `registerAllServices`, not the extra single-service plaintext registration.

- [ ] **Step 4: Build the agent**

```bash
cd go && go build ./cmd/wendy-agent/
```

Expected: no errors.

- [ ] **Step 5: Run the full test suite**

```bash
cd go && go test ./... -count=1 -timeout 120s
```

Expected: all tests pass (no regressions introduced).

- [ ] **Step 6: Commit**

```bash
git add go/cmd/wendy-agent/main.go
git commit -m "feat: register v2 gRPC services on agent server"
```

---

## Self-Review Checklist

After completing all tasks, verify:

- [ ] `make proto` runs cleanly and generates files in `go/proto/gen/agentpb/v2/`
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes (all existing tests still pass)
- [ ] v1 `WendyAgentService` registration is unchanged in `main.go`
- [ ] `WendyFileSyncServiceV2` is NOT registered (stub only)
- [ ] v2 `WendyProvisioningService` is added only inside `registerAllServices`, not the individual plaintext-only registration
- [ ] `volumesDir` variable referenced in `container_service_v2.go` is the same package-level var in `container_service.go`
- [ ] Telemetry broadcaster method names match actual names in `telemetry_service.go`
