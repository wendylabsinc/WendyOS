# Model Watch, Plan 2: Agent Model Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `WendyModelService` to the Go agent and the `wendy device model` CLI, so a client can start a catalog model on a device camera, watch its events, and have it cleaned up when nobody watches. The whole path is verified on a device with a fake model host.

**Architecture:**
- **`go/internal/agent/models`** (new package) holds everything that can be tested without containerd: the catalog and variant selection, the host record contract, the event ring and filters, the model file cache, and the `Supervisor`. The `Supervisor` owns instances, watches, leases, restarts and engine-build slots.
- **Small interfaces.** The package reaches containerd, cameras and files only through `Runtime`, `Cameras`, `Files` and `Clock`.
- **Adapters in existing packages:**
  - the containerd client implements `Runtime` (`containerd/model_host.go`);
  - `VideoService` gains pinned two-plane demand;
  - the app data socket forwards accepted records to the supervisor;
  - `services/model_service.go` maps the supervisor to gRPC.
- **Wiring.** `main.go` registers the service on the mTLS server and the admin socket only.
- **Fake host.** A small Go fake host (`go/modelhost/fakehost`) exercises the path end to end before the Mojo host (milestone M1) exists.

**Tech Stack:** Go 1.27 (module `github.com/wendylabsinc/wendy`, `go.mod` at the repo root), gRPC and protobuf (local `protoc` 36.1, `protoc-gen-go` v1.36.11, `protoc-gen-go-grpc` 1.6.2), containerd v2.3.5 client, zap and cobra.

**Spec:** `specs/2026-09-25-model-watch-design.md`, milestone M2 in §13. This plan follows v2 proto conventions: times are `int64 *_unix_nanos` rather than `google.protobuf.Timestamp`, which no v2 agent proto uses. It adds two fields the spec's sketch lacked: `CatalogVariant.image_cached` and `WatchStarted.last_sequence`. Task 2 updates the spec to match.

## Global Constraints

- **Paths:** run every Go command from the repo root (`/media/work/WendyAgent`); `go.mod` lives there.
- **Reserved app-id prefix:** model hosts use `sh.wendy.model.` (`appconfig.ReservedModelAppIDPrefix`, re-exported as `models.AppIDPrefix`). User apps may not use it.
- **Leases:**
  - a watched instance lives while at least one watch is attached;
  - after the last watch is lost it stays **60 s**;
  - a started instance that no watch attaches to stops after **60 s**;
  - stopping the last watch explicitly stops the instance at once.
- **Admission limits:** at most **2** instances per device (`ErrCapacity` beyond that) and **one** engine build at a time.
- **Host health:**
  - the host sends `model.status` every 5 s; **15 s** of silence counts as a stall;
  - an engine build times out after **15 min**;
  - crashes and stalls restart with backoff **2 s, 8 s, 30 s**, and the fourth failure is final;
  - a host-reported `failed` status is final, with no restart.
- **Buffers:** the event ring keeps the **last 100** events per instance; each watch buffers **128** messages.
- **Host environment:** `WENDY_MODEL_MAX_FPS=10` and `WENDY_MODEL_CONFIDENCE_FLOOR=0.30`.
- **Model host containers:**
  - user 65534:65534 plus the video group 44; the data-socket group 2000 comes from `applyDataSocket`;
  - read-only root, no capabilities;
  - a private network namespace with no CNI, so no network;
  - exactly one camera node;
  - labels `sh.wendy/model.*` and never `sh.wendy/app.*`.
- **Registration:** `WendyModelService` is registered on the mTLS server and the admin socket, and never inside `registerAllServices`, which also serves the plaintext pre-provisioning port.
- **v2 proto style:**
  - `option go_package = "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2;agentpbv2";`
  - 2-space indent;
  - enum values prefixed with the enum name, with `_UNSPECIFIED = 0`.
- **Generated code:** commit only the new generated files. `make proto` rewrites every header, so restore the others.
- **Commit identity:** every commit uses `git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit`. Its message ends with the attribution lines your own session specifies. The commit steps below show the planning session's lines; replace them with yours, especially the `Claude-Session` link:
  ```
  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
  ```

## Review Focus

Five inputs the spec implies but its happy paths don't exercise, most likely first. Each line names the task that pins it with a test.

1. **A stop during a slow first start** (image pull, model download, engine build) must cancel the work promptly and never start a host afterwards. Task 6: `TestStopDuringImagePullCancelsIt`.
2. **A start refused for its camera** (missing, or unable to carry frame identity) must not keep a slot or a camera pin. Task 6: `TestRefusedCameraDoesNotTakeASlot`.
3. **A watch whose client stops reading** must never block record intake or other watches. It receives the missed range as a gap. Task 8: `TestSlowWatchNeverBlocksIntake`.
4. **Two clients starting the same model on the same camera at once** must share one instance, not race two containers onto one camera. Task 6: `TestConcurrentStartsShareOneInstance`.
5. **A status record from a host being restarted or removed** must not move the instance back to ready. Task 9: `TestStaleStatusDuringRestartIsIgnored`.

---

## File Structure

**New package `go/internal/agent/models/`** (nothing in it imports containerd or gRPC):

| File | Responsibility |
|---|---|
| `models.go` | Package doc, `AppIDPrefix`, `State`, `Stats`, `InstanceInfo`, sentinel errors |
| `catalog.go`, `catalog.json` | Catalog types, strict parsing and validation, the embedded (empty for now) catalog |
| `select.go` | `DeviceProfile`, `SelectVariant` |
| `records.go` | The host record contract (`model.entered`, `model.left`, `model.status`) and its parsers |
| `events.go` | `Event`, `Box`, `Gap`, `Filter`, and the per-instance event `ring` |
| `files.go` | `FileCache` (download, verify, content-addressed store) and `engineCached` |
| `runtime.go` | `HostSpec` with its `Env` contract, and the `Runtime`, `Cameras` and `Files` interfaces |
| `clock.go` | `Clock` and `Timer`, with the real clock |
| `watch.go` | `Watch`, `WatchRequest`, `WatchMessage`, non-blocking delivery, filter checks |
| `instance.go` | One instance's state, guarded by its mutex |
| `supervisor.go` | `Supervisor`: `Catalog`, `Start`, `List`, `Stop`, `Watch`, `Detach`, `Shutdown`, record intake |
| `lifecycle.go` | The run loop: prepare, start or restart the host, finish, and orphan cleanup |
| `modelstest/fakes.go` | Fakes for tests: `Clock`, `Runtime`, `Cameras`, `Files`, record builders, `Catalog`, `Eventually`, `AdvanceUntil` |

**Elsewhere:**

| File | Change |
|---|---|
| `Proto/wendy/agent/services/v2/model_service.proto` | New service contract |
| `go/proto/gen/agentpb/v2/model_service{,_grpc}.pb.go` | Generated |
| `go/scripts/generate-proto.sh` | Adds the proto |
| `go/internal/shared/appconfig/appconfig.go` | Reserves `sh.wendy.model.` for the agent |
| `go/internal/agent/services/container_service.go` | `parseAppConfig` rejects the reserved prefix |
| `go/internal/agent/services/video_service_two_plane.go`, new `video_service_two_plane_pin.go` | Pinned two-plane demand |
| `go/internal/agent/services/app_data_socket.go` | `SetRecordSink` |
| `go/internal/agent/containerd/model_host.go` (new) | Implements `models.Runtime` |
| `go/internal/agent/services/model_service.go`, `model_adapters.go` (new) | gRPC handlers, `ModelCameras`, `ModelDeviceProfile` |
| `go/cmd/wendy-agent/models.go` (new), `main.go` | Wiring, dev catalog override, boot cleanup |
| `go/internal/cli/grpcclient/client.go` | `ModelService` client |
| `go/internal/cli/commands/device_model.go` (new), `device.go` | `wendy device model` |
| `go/modelhost/fakehost/` (new) | Fake host, its Dockerfile and README |
| `.github/workflows/model-host.yml` (new) | Builds and tests the fake host; publishes on dispatch |

---

### Task 1: Reserve the model host app-id prefix

A user app with appId `sh.wendy.model.…` would share a model host's data socket and could forge its detections. Reserve the prefix wherever user app configs are validated.

**Files:**
- Modify: `go/internal/shared/appconfig/appconfig.go` (after `ValidateAppID`, ~line 628; in `(*AppConfig).Validate`, ~line 724)
- Modify: `go/internal/agent/services/container_service.go:1574-1578` (`parseAppConfig`)
- Test: `go/internal/shared/appconfig/reserved_app_id_test.go` (new)
- Test: `go/internal/agent/services/container_service_reserved_test.go` (new)

**Interfaces:**
- Produces: `appconfig.ReservedModelAppIDPrefix = "sh.wendy.model."` and `func appconfig.ValidateUserAppID(id string) error`.

- [ ] **Step 1: Write the failing tests**

`go/internal/shared/appconfig/reserved_app_id_test.go`:

```go
package appconfig

import (
	"strings"
	"testing"
)

func TestUserAppIDsCannotUseTheModelPrefix(t *testing.T) {
	if err := ValidateUserAppID("sh.wendy.model.m-1a2b3c4d"); err == nil {
		t.Fatal("a user app took a model host's identity")
	}
	if err := ValidateUserAppID("sh.wendy.models-demo"); err != nil {
		t.Fatalf("an unrelated id was refused: %v", err)
	}
	// The agent's own model hosts still pass the plain syntax check.
	if err := ValidateAppID("sh.wendy.model.m-1a2b3c4d"); err != nil {
		t.Fatalf("ValidateAppID rejected a model host id: %v", err)
	}
	cfg := &AppConfig{AppID: "sh.wendy.model.x"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Validate = %v, want the reservation", err)
	}
}
```

`go/internal/agent/services/container_service_reserved_test.go`:

```go
package services

import "testing"

func TestParseAppConfigRefusesReservedModelIDs(t *testing.T) {
	if _, err := parseAppConfig([]byte(`{"appId":"sh.wendy.model.m-1a2b3c4d"}`)); err == nil {
		t.Fatal("the agent accepted a user app with a model host's identity")
	}
	if _, err := parseAppConfig([]byte(`{"appId":"com.example.app"}`)); err != nil {
		t.Fatalf("an ordinary app was refused: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/shared/appconfig/ -run TestUserAppIDsCannotUseTheModelPrefix`
Expected: FAIL to compile with `undefined: ValidateUserAppID`.

- [ ] **Step 3: Implement the reservation**

In `go/internal/shared/appconfig/appconfig.go`, directly after the closing brace of `ValidateAppID`, add:

```go
// ReservedModelAppIDPrefix names the data-socket identity of the model hosts
// the agent runs itself (internal/agent/models). A user app with such an id
// would share a model host's data socket and could forge its detections.
const ReservedModelAppIDPrefix = "sh.wendy.model."

// ValidateUserAppID is ValidateAppID plus the reservations that apply only to
// apps a user deploys.
func ValidateUserAppID(id string) error {
	if err := ValidateAppID(id); err != nil {
		return err
	}
	if strings.HasPrefix(id, ReservedModelAppIDPrefix) {
		return fmt.Errorf("appId %q is invalid: the prefix %q is reserved for models the agent runs", id, ReservedModelAppIDPrefix)
	}
	return nil
}
```

In `(c *AppConfig) Validate()`, replace:

```go
	if err := ValidateAppID(c.AppID); err != nil {
		return err
	}
```

with:

```go
	if err := ValidateUserAppID(c.AppID); err != nil {
		return err
	}
```

In `go/internal/agent/services/container_service.go` `parseAppConfig`, replace `if err := appconfig.ValidateAppID(cfg.AppID); err != nil {` with `if err := appconfig.ValidateUserAppID(cfg.AppID); err != nil {`.

- [ ] **Step 4: Run the tests and the packages' suites**

Run: `go test ./go/internal/shared/appconfig/ ./go/internal/agent/services/ -run 'TestUserAppIDsCannotUseTheModelPrefix|TestParseAppConfigRefusesReservedModelIDs'`
Expected: PASS.
Run: `go test ./go/internal/shared/appconfig/`
Expected: PASS. No existing app id uses the prefix.

- [ ] **Step 5: Commit**

```bash
git add go/internal/shared/appconfig/appconfig.go go/internal/shared/appconfig/reserved_app_id_test.go go/internal/agent/services/container_service.go go/internal/agent/services/container_service_reserved_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
appconfig: reserve sh.wendy.model. for agent-run model hosts

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 2: The `WendyModelService` proto and CLI client

**Files:**
- Create: `Proto/wendy/agent/services/v2/model_service.proto`
- Modify: `go/scripts/generate-proto.sh` (the `V2_AGENT_PROTOS` array, after `"wendy/agent/services/v2/data_service.proto"`)
- Generated: `go/proto/gen/agentpb/v2/model_service.pb.go` and `go/proto/gen/agentpb/v2/model_service_grpc.pb.go`
- Modify: `go/internal/cli/grpcclient/client.go` (`AgentConnection` at ~line 92, `newAgentConnection` at ~line 582)
- Modify: `specs/2026-09-25-model-watch-design.md` §5 (the proto block)
- Test: `go/internal/cli/grpcclient/model_service_test.go` (new)

**Interfaces:**
- Produces (Go, package `agentpbv2`):
  - service: `WendyModelServiceClient`, `RegisterWendyModelServiceServer`, `UnimplementedWendyModelServiceServer`;
  - enum: `ModelState` with `ModelState_MODEL_STATE_{UNSPECIFIED,PREPARING,STARTING,READY,RESTARTING,FAILED,STOPPED}`;
  - watch messages: `ModelWatchMessage`, with oneof wrappers `ModelWatchMessage_Started`, `_Status`, `_Event` and `_Gap`;
  - every other message named in the proto below;
  - client field `grpcclient.AgentConnection.ModelService agentpbv2.WendyModelServiceClient`.

- [ ] **Step 1: Write the failing test**

`go/internal/cli/grpcclient/model_service_test.go`:

```go
package grpcclient

import (
	"slices"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestAgentConnectionHasModelService(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if NewFromConn(conn).ModelService == nil {
		t.Fatal("AgentConnection.ModelService is not wired")
	}
	var rpcs []string
	for _, m := range agentpbv2.WendyModelService_ServiceDesc.Methods {
		rpcs = append(rpcs, m.MethodName)
	}
	for _, s := range agentpbv2.WendyModelService_ServiceDesc.Streams {
		rpcs = append(rpcs, s.StreamName)
	}
	slices.Sort(rpcs)
	if want := []string{"ListCatalog", "ListModels", "StartModel", "StopModel", "WatchModel"}; !slices.Equal(rpcs, want) {
		t.Fatalf("rpcs = %v, want %v", rpcs, want)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./go/internal/cli/grpcclient/ -run TestAgentConnectionHasModelService`
Expected: FAIL to compile with `undefined: agentpbv2.WendyModelService_ServiceDesc`.

- [ ] **Step 3: Write the proto**

`Proto/wendy/agent/services/v2/model_service.proto`:

```proto
syntax = "proto3";

package wendy.agent.services.v2;

option go_package = "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2;agentpbv2";

// WendyModelService runs catalog models on the device's cameras for clients
// such as wendy chat (specs/2026-09-25-model-watch-design.md). An instance
// lives while a WatchModel stream holds it, plus 60 s for reconnects.
service WendyModelService {
  // ListCatalog lists the models this device can run and the cameras they can watch.
  rpc ListCatalog(ListModelCatalogRequest) returns (ListModelCatalogResponse);
  // StartModel starts a model on a camera, or returns the instance already
  // doing so. It returns at once; watch the instance for progress.
  rpc StartModel(StartModelRequest) returns (StartModelResponse);
  // WatchModel streams an instance's state and filtered events, and holds the
  // instance alive while the stream is open.
  rpc WatchModel(WatchModelRequest) returns (stream ModelWatchMessage);
  rpc ListModels(ListModelsRequest) returns (ListModelsResponse);
  // StopModel detaches one watch, stopping the instance if it was the last,
  // or with no watch_id stops the instance outright.
  rpc StopModel(StopModelRequest) returns (StopModelResponse);
}

message ListModelCatalogRequest {}

message ListModelCatalogResponse {
  string catalog_version = 1;
  repeated CatalogModel models = 2;
  repeated ModelCamera cameras = 3;   // local cameras a model can watch now
  uint32 max_running = 4;
  uint32 running = 5;
}

message CatalogModel {
  string id = 1;                      // "coco-detector"
  string description = 2;
  string kind = 3;                    // "detector"
  repeated string labels = 4;         // the classes it reports
  CatalogVariant variant = 5;         // chosen for this device; unset when none fits
  string unavailable_reason = 6;      // set when variant is unset
}

message CatalogVariant {
  string id = 1;
  string engine = 2;                  // "tensorrt" | "onnxruntime" | "qnn"
  uint64 download_bytes = 3;          // model file bytes a first start downloads; 0 when cached
  bool needs_engine_build = 4;        // no engine built for this device yet
  bool image_cached = 5;              // the host image is already on the device
}

message ModelCamera {
  string source_id = 1;               // "v4l2:/dev/video0"
  string name = 2;
}

message StartModelRequest {
  string model_id = 1;
  string camera_source_id = 2;
}

message StartModelResponse {
  ModelInstance instance = 1;
  bool reused = 2;                    // an instance already ran this model on this camera
}

enum ModelState {
  MODEL_STATE_UNSPECIFIED = 0;
  MODEL_STATE_PREPARING = 1;          // pulling, downloading, or waiting for or building an engine
  MODEL_STATE_STARTING = 2;
  MODEL_STATE_READY = 3;
  MODEL_STATE_RESTARTING = 4;
  MODEL_STATE_FAILED = 5;
  MODEL_STATE_STOPPED = 6;
}

message ModelStats {
  float processed_fps = 1;
  float latency_p50_ms = 2;
  uint64 frames_skipped = 3;
}

message ModelInstance {
  string instance_id = 1;
  string model_id = 2;
  string variant_id = 3;
  string engine = 4;
  string camera_source_id = 5;
  ModelState state = 6;
  string state_detail = 7;            // what it is doing, or why it failed
  uint32 watchers = 8;
  repeated string watch_labels = 9;
  int64 started_unix_nanos = 10;
  ModelStats stats = 11;              // from the host's latest heartbeat
  string file_sha256 = 12;            // the model file this instance runs
}

message WatchModelRequest {
  string instance_id = 1;
  string label = 2;                   // what the watcher calls this watch, e.g. "front door"
  repeated string classes = 3;        // empty: every class
  float min_confidence = 4;           // 0: everything the host reports
  repeated string event_types = 5;    // "entered", "left"; empty: both
  uint64 after_sequence = 6;          // replay retained events after this; 0 replays nothing
}

message ModelWatchMessage {
  oneof message {
    WatchStarted started = 1;         // always first
    ModelInstance status = 2;         // state changes and heartbeats; the last is final
    ModelEvent event = 3;
    ModelGap gap = 4;                 // events this watch did not receive
  }
}

message WatchStarted {
  string watch_id = 1;
  ModelInstance instance = 2;
  uint64 last_sequence = 3;           // newest event number; pass it as after_sequence to resume
}

message ModelEvent {
  uint64 sequence = 1;                // per instance, assigned by the agent
  string type = 2;                    // "entered" | "left"
  string class_name = 3;
  float confidence = 4;
  uint64 track_id = 5;
  BoundingBox box = 6;                // normalized to the frame, 0..1
  string source_id = 7;
  uint64 sample_id = 8;               // two-plane identity of the frame that triggered it
  int64 time_unix_nanos = 9;          // when the agent received it
}

message BoundingBox {
  float x = 1;
  float y = 2;
  float width = 3;
  float height = 4;
}

message ModelGap {
  uint64 first_missing = 1;
  uint64 last_missing = 2;
}

message ListModelsRequest {}

message ListModelsResponse {
  repeated ModelInstance instances = 1;
}

message StopModelRequest {
  string instance_id = 1;
  string watch_id = 2;                // detach this watch; empty stops the instance outright
}

message StopModelResponse {
  ModelInstance instance = 1;
}
```

- [ ] **Step 4: Register the proto and generate**

In `go/scripts/generate-proto.sh`, add `"wendy/agent/services/v2/model_service.proto"` on the line after `"wendy/agent/services/v2/data_service.proto"` in `V2_AGENT_PROTOS`.

Run:
```bash
cd go && make proto && cd ..
git status --short go/proto/gen
```
Expected: two untracked files (`?? go/proto/gen/agentpb/v2/model_service.pb.go` and `?? go/proto/gen/agentpb/v2/model_service_grpc.pb.go`), plus modified tracked files whose only change is the version header.

Throw away the header churn. This touches tracked files only, so the two new files stay:
```bash
git restore go/proto/gen
git status --short go/proto/gen
```
Expected: only the two `??` lines.

- [ ] **Step 5: Wire the client**

In `go/internal/cli/grpcclient/client.go`, add a field after `DataService          agentpbv2.DataServiceClient` in `AgentConnection`:

```go
	ModelService         agentpbv2.WendyModelServiceClient
```

In `newAgentConnection`, add this after `DataService:          agentpbv2.NewDataServiceClient(conn),`:

```go
		ModelService:         agentpbv2.NewWendyModelServiceClient(conn),
```

Run: `gofmt -l go/internal/cli/grpcclient/`
Expected: no output.

- [ ] **Step 6: Run the test**

Run: `go test ./go/internal/cli/grpcclient/ -run TestAgentConnectionHasModelService`
Expected: PASS.

- [ ] **Step 7: Bring the spec's proto sketch in line**

In `specs/2026-09-25-model-watch-design.md` §5, replace the fenced `proto` block with the service, enum and message definitions from Step 3. Then add one sentence after the block: "Times are `int64` Unix nanoseconds, as in every v2 agent proto."

- [ ] **Step 8: Commit**

```bash
git add Proto/wendy/agent/services/v2/model_service.proto go/scripts/generate-proto.sh go/proto/gen/agentpb/v2/model_service.pb.go go/proto/gen/agentpb/v2/model_service_grpc.pb.go go/internal/cli/grpcclient/client.go go/internal/cli/grpcclient/model_service_test.go specs/2026-09-25-model-watch-design.md
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
proto: add WendyModelService

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 3: models: core types, catalog and variant selection

**Files:**
- Create: `go/internal/agent/models/models.go`
- Create: `go/internal/agent/models/catalog.go`
- Create: `go/internal/agent/models/catalog.json`
- Create: `go/internal/agent/models/select.go`
- Test: `go/internal/agent/models/catalog_test.go`
- Test: `go/internal/agent/models/select_test.go`

**Interfaces:**
- Consumes: `appconfig.ReservedModelAppIDPrefix` (Task 1).
- Produces:
  - `models.AppIDPrefix`;
  - `State` with `StatePreparing … StateStopped` and a `String()` method;
  - `Stats{ProcessedFPS, LatencyP50Ms float32; FramesSkipped uint64}`;
  - `InstanceInfo{ID, ModelID, VariantID, Engine, CameraSourceID string; State State; StateDetail string; Watchers int; WatchLabels []string; StartedAt time.Time; Stats Stats; FileSHA256 string}`;
  - errors `ErrUnknownModel`, `ErrUnknownCamera`, `ErrUnknownInstance`, `ErrUnknownWatch`, `ErrNoVariant`, `ErrCameraNotStreamable`, `ErrCapacity`, `ErrInvalidFilter`;
  - catalog types `Catalog{Version; Models []Model}` with `(Catalog) Model(id string) (Model, bool)`, `Model{ID, Description, Kind string; Labels []string; Variants []Variant}`, `Variant{ID, Engine string; Requires Requires; HostImage string; File File; InputSize int}`, `Requires{Arch, GPUVendor, ComputeBackend, NPUBackend string}` and `File{URL, SHA256 string; Bytes int64}`;
  - constants `EngineTensorRT`, `EngineONNXRuntime`, `EngineQNN` and `KindDetector`;
  - catalog functions `ParseCatalog([]byte) (Catalog, error)` and `DefaultCatalog() (Catalog, error)`;
  - selection: `DeviceProfile{Arch, GPUVendor, GPUArch string; ComputeBackends, NPUBackends []string}` and `SelectVariant(Model, DeviceProfile) (Variant, string, bool)`.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/models/catalog_test.go`:

```go
package models

import (
	"strings"
	"testing"
)

const testSHA = "0000000000000000000000000000000000000000000000000000000000000001"

func testFile() string {
	return `{"url": "https://storage.googleapis.com/wendy-models-public/sha256/` + testSHA + `", "sha256": "` + testSHA + `", "bytes": 1024}`
}

// validCatalogJSON has one detector with a QNN, a TensorRT and a CPU variant,
// in that order.
func validCatalogJSON() string {
	return `{
  "version": "test-1",
  "models": [{
    "id": "coco-detector",
    "description": "Detects the 80 COCO classes",
    "kind": "detector",
    "labels": ["person", "car", "traffic light"],
    "variants": [
      {"id": "d-qnn", "engine": "qnn", "requires": {"npu_backend": "qnn"},
       "host_image": "ghcr.io/wendylabsinc/wendy-model-host-qualcomm@sha256:` + strings.Repeat("a", 64) + `",
       "file": ` + testFile() + `, "input_size": 640},
      {"id": "d-trt", "engine": "tensorrt", "requires": {"gpu_vendor": "nvidia", "compute_backend": "cuda"},
       "host_image": "ghcr.io/wendylabsinc/wendy-model-host-jetson@sha256:` + strings.Repeat("b", 64) + `",
       "file": ` + testFile() + `, "input_size": 640},
      {"id": "d-cpu", "engine": "onnxruntime", "requires": {"arch": "arm64"},
       "host_image": "ghcr.io/wendylabsinc/wendy-model-host-cpu@sha256:` + strings.Repeat("c", 64) + `",
       "file": ` + testFile() + `, "input_size": 320}
    ]
  }]
}`
}

func TestParseCatalogAcceptsValidCatalog(t *testing.T) {
	c, err := ParseCatalog([]byte(validCatalogJSON()))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := c.Model("coco-detector")
	if !ok || len(m.Variants) != 3 || m.Variants[2].InputSize != 320 {
		t.Fatalf("model = %+v, %v", m, ok)
	}
	if _, ok := c.Model("missing"); ok {
		t.Fatal("found a model that is not in the catalog")
	}
}

func TestDefaultCatalogParses(t *testing.T) {
	c, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("the embedded catalog is invalid: %v", err)
	}
	if c.Version == "" {
		t.Fatal("the embedded catalog has no version")
	}
}

func TestParseCatalogRejects(t *testing.T) {
	cases := map[string]func(string) string{
		"tag-only image": func(s string) string {
			return strings.Replace(s, "@sha256:"+strings.Repeat("c", 64), ":latest", 1)
		},
		"http file url":  func(s string) string { return strings.Replace(s, "https://", "http://", 1) },
		"unknown field":  func(s string) string { return strings.Replace(s, `"input_size": 320`, `"input_size": 320, "extra": 1`, 1) },
		"unknown engine": func(s string) string { return strings.Replace(s, `"engine": "onnxruntime"`, `"engine": "tflite"`, 1) },
		"repeated label": func(s string) string { return strings.Replace(s, `"car"`, `"person"`, 1) },
		"short digest":   func(s string) string { return strings.Replace(s, `"sha256": "`+testSHA+`"`, `"sha256": "abc"`, 1) },
		"no version":     func(s string) string { return strings.Replace(s, `"version": "test-1"`, `"version": ""`, 1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCatalog([]byte(mutate(validCatalogJSON()))); err == nil {
				t.Fatal("accepted an invalid catalog")
			}
		})
	}
}
```

`go/internal/agent/models/select_test.go`:

```go
package models

import (
	"strings"
	"testing"
)

func TestSelectVariantFollowsCatalogOrder(t *testing.T) {
	c, err := ParseCatalog([]byte(validCatalogJSON()))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := c.Model("coco-detector")
	cases := []struct {
		name string
		dev  DeviceProfile
		want string
	}{
		{"qualcomm", DeviceProfile{Arch: "arm64", GPUVendor: "qualcomm", NPUBackends: []string{"qnn"}}, "d-qnn"},
		{"jetson", DeviceProfile{Arch: "arm64", GPUVendor: "nvidia", ComputeBackends: []string{"cuda"}}, "d-trt"},
		{"raspberry pi 5", DeviceProfile{Arch: "arm64", GPUVendor: "broadcom"}, "d-cpu"},
	}
	for _, tc := range cases {
		v, reason, ok := SelectVariant(m, tc.dev)
		if !ok || v.ID != tc.want {
			t.Errorf("%s: got %q (%v, %q), want %q", tc.name, v.ID, ok, reason, tc.want)
		}
	}
}

func TestSelectVariantExplainsMismatch(t *testing.T) {
	c, _ := ParseCatalog([]byte(validCatalogJSON()))
	m, _ := c.Model("coco-detector")
	_, reason, ok := SelectVariant(m, DeviceProfile{Arch: "amd64"})
	if ok {
		t.Fatal("an amd64 CPU-only device matched")
	}
	for _, want := range []string{"d-qnn needs NPU backend qnn", "d-trt needs a nvidia GPU", "d-cpu needs arch arm64"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/`
Expected: FAIL. The package does not exist yet (`no Go files` or `undefined: ParseCatalog`).

- [ ] **Step 3: Write the implementation**

`go/internal/agent/models/models.go`:

```go
// Package models runs catalog models on this device for clients such as
// wendy chat. For each instance it picks the variant that fits the hardware,
// fetches and verifies the model file, runs an agent-owned host container, and
// streams the host's detections to watchers. An instance lives while a watch
// holds it, plus a grace period for reconnects
// (specs/2026-09-25-model-watch-design.md).
package models

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// AppIDPrefix is the data-socket and cgroup identity prefix of model hosts.
// The agent refuses user apps whose appId starts with it.
const AppIDPrefix = appconfig.ReservedModelAppIDPrefix

// State is an instance's lifecycle state.
type State int

const (
	StatePreparing State = iota + 1
	StateStarting
	StateReady
	StateRestarting
	StateFailed
	StateStopped
)

func (s State) String() string {
	switch s {
	case StatePreparing:
		return "preparing"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateRestarting:
		return "restarting"
	case StateFailed:
		return "failed"
	case StateStopped:
		return "stopped"
	}
	return "unspecified"
}

// Stats are a host's latest self-reported numbers.
type Stats struct {
	ProcessedFPS  float32
	LatencyP50Ms  float32
	FramesSkipped uint64
}

// InstanceInfo is a snapshot of one model instance.
type InstanceInfo struct {
	ID             string
	ModelID        string
	VariantID      string
	Engine         string
	CameraSourceID string
	State          State
	StateDetail    string
	Watchers       int
	WatchLabels    []string
	StartedAt      time.Time
	Stats          Stats
	FileSHA256     string
}

// Errors callers map to gRPC codes.
var (
	ErrUnknownModel        = errors.New("unknown model")
	ErrUnknownCamera       = errors.New("unknown camera")
	ErrUnknownInstance     = errors.New("unknown model instance")
	ErrUnknownWatch        = errors.New("unknown watch")
	ErrNoVariant           = errors.New("no variant of this model runs on this device")
	ErrCameraNotStreamable = errors.New("camera cannot stream to a model")
	ErrCapacity            = errors.New("this device is already running its maximum number of models")
	ErrInvalidFilter       = errors.New("invalid watch filter")
)
```

`go/internal/agent/models/catalog.go`:

```go
package models

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

//go:embed catalog.json
var defaultCatalogJSON []byte

// Engines a variant can run on.
const (
	EngineTensorRT    = "tensorrt"
	EngineONNXRuntime = "onnxruntime"
	EngineQNN         = "qnn"
)

// KindDetector is the only model kind in this slice.
const KindDetector = "detector"

// Catalog is the set of models this agent can run.
type Catalog struct {
	Version string  `json:"version"`
	Models  []Model `json:"models"`
}

// Model is one catalog entry.
type Model struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Kind        string    `json:"kind"`
	Labels      []string  `json:"labels"`
	Variants    []Variant `json:"variants"`
}

// Variant is one way to run a model on a class of hardware.
type Variant struct {
	ID        string   `json:"id"`
	Engine    string   `json:"engine"`
	Requires  Requires `json:"requires"`
	HostImage string   `json:"host_image"`
	File      File     `json:"file"`
	InputSize int      `json:"input_size"`
}

// Requires holds predicates over a DeviceProfile; empty fields match anything.
type Requires struct {
	Arch           string `json:"arch,omitempty"`
	GPUVendor      string `json:"gpu_vendor,omitempty"`
	ComputeBackend string `json:"compute_backend,omitempty"`
	NPUBackend     string `json:"npu_backend,omitempty"`
}

// File is a content-addressed model file.
type File struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

var (
	catalogIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	labelPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9 _-]{0,63}$`)
	digestRefPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.:/_-]*@sha256:[0-9a-f]{64}$`)
	sha256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// DefaultCatalog returns the catalog compiled into the agent.
func DefaultCatalog() (Catalog, error) { return ParseCatalog(defaultCatalogJSON) }

// ParseCatalog decodes and validates a catalog document. Unknown fields are
// errors, so a typo cannot silently drop a requirement.
func ParseCatalog(raw []byte) (Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("model catalog: %w", err)
	}
	if err := c.validate(); err != nil {
		return Catalog{}, fmt.Errorf("model catalog: %w", err)
	}
	return c, nil
}

// Model returns the entry with this id.
func (c Catalog) Model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func (c Catalog) validate() error {
	if c.Version == "" {
		return errors.New("version is required")
	}
	seen := map[string]bool{}
	for _, m := range c.Models {
		if !catalogIDPattern.MatchString(m.ID) || seen[m.ID] {
			return fmt.Errorf("model id %q is invalid or repeated", m.ID)
		}
		seen[m.ID] = true
		if err := m.validate(); err != nil {
			return fmt.Errorf("model %s: %w", m.ID, err)
		}
	}
	return nil
}

func (m Model) validate() error {
	if m.Kind != KindDetector {
		return fmt.Errorf("kind %q is not supported", m.Kind)
	}
	if len(m.Labels) == 0 {
		return errors.New("labels are required")
	}
	labels := map[string]bool{}
	for _, l := range m.Labels {
		if !labelPattern.MatchString(l) || labels[l] {
			return fmt.Errorf("label %q is invalid or repeated", l)
		}
		labels[l] = true
	}
	if len(m.Variants) == 0 {
		return errors.New("at least one variant is required")
	}
	variants := map[string]bool{}
	for _, v := range m.Variants {
		if !catalogIDPattern.MatchString(v.ID) || variants[v.ID] {
			return fmt.Errorf("variant id %q is invalid or repeated", v.ID)
		}
		variants[v.ID] = true
		if err := v.validate(); err != nil {
			return fmt.Errorf("variant %s: %w", v.ID, err)
		}
	}
	return nil
}

func (v Variant) validate() error {
	switch v.Engine {
	case EngineTensorRT, EngineONNXRuntime, EngineQNN:
	default:
		return fmt.Errorf("engine %q is not supported", v.Engine)
	}
	// A digest is the only trust anchor for code the agent runs with camera
	// and accelerator access, so a tag is never enough.
	if !digestRefPattern.MatchString(v.HostImage) {
		return fmt.Errorf("host_image %q must be pinned by digest (name@sha256:…)", v.HostImage)
	}
	u, err := url.Parse(v.File.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("file url %q must be an https URL", v.File.URL)
	}
	if !sha256Pattern.MatchString(v.File.SHA256) {
		return fmt.Errorf("file sha256 %q is not 64 lowercase hex digits", v.File.SHA256)
	}
	if v.File.Bytes <= 0 {
		return errors.New("file bytes must be positive")
	}
	if v.InputSize <= 0 {
		return errors.New("input_size must be positive")
	}
	return nil
}
```

`go/internal/agent/models/catalog.json`. It stays empty until milestone M1 publishes a real host:

```json
{
  "version": "2026.09.25",
  "models": []
}
```

`go/internal/agent/models/select.go`:

```go
package models

import (
	"slices"
	"strings"
)

// DeviceProfile is what variant selection knows about this device.
type DeviceProfile struct {
	Arch            string   // runtime.GOARCH, e.g. "arm64"
	GPUVendor       string   // e.g. "nvidia"; empty when there is no GPU
	GPUArch         string   // e.g. "sm_87"; empty when unknown
	ComputeBackends []string // GPU compute backends, e.g. ["cuda"]
	NPUBackends     []string // e.g. ["qnn"]
}

// SelectVariant returns the first variant, in catalog order, whose
// requirements the device meets. When none fits, the string says what each
// variant needed.
func SelectVariant(m Model, d DeviceProfile) (Variant, string, bool) {
	var needs []string
	for _, v := range m.Variants {
		unmet := v.Requires.unmet(d)
		if unmet == "" {
			return v, "", true
		}
		needs = append(needs, v.ID+" needs "+unmet)
	}
	if len(needs) == 0 {
		return Variant{}, "the model has no variants", false
	}
	return Variant{}, strings.Join(needs, "; "), false
}

func (r Requires) unmet(d DeviceProfile) string {
	switch {
	case r.Arch != "" && r.Arch != d.Arch:
		return "arch " + r.Arch
	case r.GPUVendor != "" && r.GPUVendor != d.GPUVendor:
		return "a " + r.GPUVendor + " GPU"
	case r.ComputeBackend != "" && !slices.Contains(d.ComputeBackends, r.ComputeBackend):
		return "GPU compute backend " + r.ComputeBackend
	case r.NPUBackend != "" && !slices.Contains(d.NPUBackends, r.NPUBackend):
		return "NPU backend " + r.NPUBackend
	}
	return ""
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./go/internal/agent/models/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/models/
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: add the catalog and variant selection

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 4: models: host records, events, filters and the event ring

**Files:**
- Create: `go/internal/agent/models/records.go`
- Create: `go/internal/agent/models/events.go`
- Test: `go/internal/agent/models/records_test.go`
- Test: `go/internal/agent/models/events_test.go`

**Interfaces:**
- Consumes: `data.ApplicationRecord` and `data.SampleRef` from `internal/agent/data`.
- Produces:
  - record names `RecordEntered = "model.entered"`, `RecordLeft = "model.left"`, `RecordStatus = "model.status"`;
  - host states `HostBuildingEngine`, `HostReady`, `HostFailed`;
  - `HostStatus{State, Reason string; Stats Stats}`;
  - parsers (unexported) `parseStatus(data.ApplicationRecord) (HostStatus, error)` and `parseDetection(data.ApplicationRecord, time.Time) (Event, error)`;
  - event types `EventEntered = "entered"` and `EventLeft = "left"`;
  - `Event{Sequence uint64; Type, Class string; Confidence float32; TrackID uint64; Box Box; SourceID string; SampleID uint64; Time time.Time}`, `Box{X, Y, Width, Height float32}` and `Gap{FirstMissing, LastMissing uint64}`;
  - `Filter{Classes []string; MinConfidence float32; Types []string}` with `(Filter) Match(Event) bool`;
  - the ring (unexported): `newRing(int) *ring`, `(*ring) append(Event) Event`, `(*ring) since(uint64) ([]Event, *Gap)` and field `last uint64`.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/models/records_test.go`:

```go
package models

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

func TestParseDetection(t *testing.T) {
	rec := data.ApplicationRecord{Version: 1, Type: "event", Name: RecordEntered, Model: "d-cpu",
		Attributes: map[string]any{"class": "person", "confidence": 0.91, "track_id": float64(12),
			"box": map[string]any{"x": 0.1, "y": 0.2, "width": 0.3, "height": 1.4}},
		Inputs: []data.SampleRef{{SourceID: "v4l2:/dev/video0", SampleID: 77}}}
	at := time.Unix(100, 0)
	e, err := parseDetection(rec, at)
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != EventEntered || e.Class != "person" || e.Confidence != float32(0.91) || e.TrackID != 12 {
		t.Fatalf("event = %+v", e)
	}
	if e.Box.Height != 1 {
		t.Fatalf("box height %v was not clamped to the frame", e.Box.Height)
	}
	if e.SourceID != "v4l2:/dev/video0" || e.SampleID != 77 || !e.Time.Equal(at) {
		t.Fatalf("event provenance = %+v", e)
	}
}

func TestParseDetectionRejectsBadRecords(t *testing.T) {
	cases := map[string]data.ApplicationRecord{
		"no class":           {Name: RecordEntered, Attributes: map[string]any{"confidence": 0.5}},
		"confidence above 1": {Name: RecordEntered, Attributes: map[string]any{"class": "person", "confidence": 1.5}},
		"not a detection":    {Name: RecordStatus, Attributes: map[string]any{"class": "person", "confidence": 0.5}},
	}
	for name, rec := range cases {
		if _, err := parseDetection(rec, time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseStatus(t *testing.T) {
	st, err := parseStatus(data.ApplicationRecord{Name: RecordStatus, Attributes: map[string]any{
		"state": "ready", "processed_fps": 9.5, "latency_p50_ms": 31.0, "frames_skipped": float64(4)}})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != HostReady || st.Stats.ProcessedFPS != 9.5 || st.Stats.LatencyP50Ms != 31 || st.Stats.FramesSkipped != 4 {
		t.Fatalf("status = %+v", st)
	}
	if _, err := parseStatus(data.ApplicationRecord{Name: RecordStatus, Attributes: map[string]any{"state": "napping"}}); err == nil {
		t.Fatal("accepted an unknown host state")
	}
}
```

`go/internal/agent/models/events_test.go`:

```go
package models

import (
	"slices"
	"testing"
)

func TestFilterMatch(t *testing.T) {
	person := Event{Type: EventEntered, Class: "person", Confidence: 0.8}
	cases := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"empty filter", Filter{}, true},
		{"class listed", Filter{Classes: []string{"car", "person"}}, true},
		{"class not listed", Filter{Classes: []string{"car"}}, false},
		{"confident enough", Filter{MinConfidence: 0.8}, true},
		{"not confident enough", Filter{MinConfidence: 0.9}, false},
		{"type listed", Filter{Types: []string{EventEntered}}, true},
		{"type not listed", Filter{Types: []string{EventLeft}}, false},
	}
	for _, tc := range cases {
		if got := tc.filter.Match(person); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRingNumbersEvictsAndReportsGaps(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		if e := r.append(Event{Class: "person"}); e.Sequence != uint64(i+1) {
			t.Fatalf("append %d numbered %d", i, e.Sequence)
		}
	}
	seqs := func(events []Event) []uint64 {
		var out []uint64
		for _, e := range events {
			out = append(out, e.Sequence)
		}
		return out
	}
	events, gap := r.since(1)
	if !slices.Equal(seqs(events), []uint64{3, 4, 5}) || gap == nil || *gap != (Gap{FirstMissing: 2, LastMissing: 2}) {
		t.Fatalf("since(1) = %v, %+v", seqs(events), gap)
	}
	events, gap = r.since(4)
	if !slices.Equal(seqs(events), []uint64{5}) || gap != nil {
		t.Fatalf("since(4) = %v, %+v", seqs(events), gap)
	}
	if events, gap = r.since(5); events != nil || gap != nil {
		t.Fatalf("since(last) = %v, %+v", events, gap)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/ -run 'TestParse|TestFilterMatch|TestRing'`
Expected: FAIL to compile with `undefined: parseDetection`.

- [ ] **Step 3: Write the implementation**

`go/internal/agent/models/records.go`:

```go
package models

import (
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// Record names a model host sends as "event" records on its data socket. The
// host side of this contract is go/modelhost; keep the two in step.
const (
	RecordEntered = "model.entered"
	RecordLeft    = "model.left"
	RecordStatus  = "model.status"
)

// States a host reports in model.status records.
const (
	HostBuildingEngine = "building_engine"
	HostReady          = "ready"
	HostFailed         = "failed"
)

// HostStatus is a parsed model.status record.
type HostStatus struct {
	State  string
	Reason string
	Stats  Stats
}

func parseStatus(rec data.ApplicationRecord) (HostStatus, error) {
	st := HostStatus{State: attrString(rec.Attributes, "state"), Reason: attrString(rec.Attributes, "reason")}
	switch st.State {
	case HostBuildingEngine, HostReady, HostFailed:
	default:
		return HostStatus{}, fmt.Errorf("model.status has unknown state %q", st.State)
	}
	st.Stats.ProcessedFPS = float32(attrFloat(rec.Attributes, "processed_fps"))
	st.Stats.LatencyP50Ms = float32(attrFloat(rec.Attributes, "latency_p50_ms"))
	if skipped := attrFloat(rec.Attributes, "frames_skipped"); skipped > 0 {
		st.Stats.FramesSkipped = uint64(skipped)
	}
	return st, nil
}

// parseDetection turns a model.entered or model.left record into an Event
// received at `at`. Sequence stays zero; the instance's ring assigns it.
func parseDetection(rec data.ApplicationRecord, at time.Time) (Event, error) {
	e := Event{Time: at}
	switch rec.Name {
	case RecordEntered:
		e.Type = EventEntered
	case RecordLeft:
		e.Type = EventLeft
	default:
		return Event{}, fmt.Errorf("record %q is not a detection", rec.Name)
	}
	if e.Class = attrString(rec.Attributes, "class"); e.Class == "" {
		return Event{}, fmt.Errorf("%s has no class", rec.Name)
	}
	confidence := attrFloat(rec.Attributes, "confidence")
	if math.IsNaN(confidence) || confidence < 0 || confidence > 1 {
		return Event{}, fmt.Errorf("%s confidence %v is outside 0..1", rec.Name, confidence)
	}
	e.Confidence = float32(confidence)
	if track := attrFloat(rec.Attributes, "track_id"); track > 0 {
		e.TrackID = uint64(track)
	}
	if box, ok := rec.Attributes["box"].(map[string]any); ok {
		e.Box = Box{X: unit(box, "x"), Y: unit(box, "y"), Width: unit(box, "width"), Height: unit(box, "height")}
	}
	if len(rec.Inputs) > 0 {
		e.SourceID, e.SampleID = rec.Inputs[0].SourceID, rec.Inputs[0].SampleID
	}
	return e, nil
}

func attrString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// attrFloat reads a number: float64 from JSON, int from records built in Go.
// Absent or non-numeric values read as 0.
func attrFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// unit reads a box coordinate clamped to the frame.
func unit(m map[string]any, key string) float32 {
	return float32(math.Min(1, math.Max(0, attrFloat(m, key))))
}
```

`go/internal/agent/models/events.go`:

```go
package models

import (
	"slices"
	"time"
)

// Detection event types.
const (
	EventEntered = "entered"
	EventLeft    = "left"
)

// Event is one detection event, numbered per instance from 1.
type Event struct {
	Sequence   uint64
	Type       string
	Class      string
	Confidence float32
	TrackID    uint64
	Box        Box
	SourceID   string
	SampleID   uint64
	Time       time.Time
}

// Box is normalized to the frame: 0..1 on both axes.
type Box struct{ X, Y, Width, Height float32 }

// Gap reports events a watch did not receive.
type Gap struct{ FirstMissing, LastMissing uint64 }

// Filter selects the events one watch receives.
type Filter struct {
	Classes       []string // empty: every class
	MinConfidence float32  // 0: everything the host reports
	Types         []string // empty: entered and left
}

// Match reports whether e passes the filter.
func (f Filter) Match(e Event) bool {
	if len(f.Classes) > 0 && !slices.Contains(f.Classes, e.Class) {
		return false
	}
	if e.Confidence < f.MinConfidence {
		return false
	}
	return len(f.Types) == 0 || slices.Contains(f.Types, e.Type)
}

// ring keeps an instance's most recent events and numbers them.
type ring struct {
	buf   []Event // oldest first
	limit int
	last  uint64 // sequence of the newest event; 0 before the first
}

func newRing(limit int) *ring { return &ring{limit: limit} }

// append numbers e and keeps it, evicting the oldest event when full.
func (r *ring) append(e Event) Event {
	r.last++
	e.Sequence = r.last
	if len(r.buf) == r.limit {
		r.buf = append(r.buf[:0], r.buf[1:]...)
	}
	r.buf = append(r.buf, e)
	return e
}

// since returns the retained events numbered after `after`, oldest first, and
// the requested range that was already evicted.
func (r *ring) since(after uint64) ([]Event, *Gap) {
	if after >= r.last {
		return nil, nil
	}
	first := r.last + 1 - uint64(len(r.buf)) // sequence of buf[0]
	var gap *Gap
	if after+1 < first {
		gap = &Gap{FirstMissing: after + 1, LastMissing: first - 1}
	}
	var out []Event
	for _, e := range r.buf {
		if e.Sequence > after {
			out = append(out, e)
		}
	}
	return out, gap
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./go/internal/agent/models/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/models/records.go go/internal/agent/models/events.go go/internal/agent/models/records_test.go go/internal/agent/models/events_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: parse host records and keep an event ring

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 5: models: the model file cache

**Files:**
- Create: `go/internal/agent/models/files.go`
- Test: `go/internal/agent/models/files_test.go`

**Interfaces:**
- Consumes: `File` (Task 3).
- Produces:
  - `ErrDigestMismatch`;
  - `NewFileCache(root string, client *http.Client) *FileCache`;
  - methods `(*FileCache) Path(sha string) string`, `(*FileCache) Has(sha string) bool` and `(*FileCache) Fetch(ctx, File) (string, error)`;
  - `engineCached(enginesRoot, fileSHA, gpuArch string) bool` (unexported).

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/models/files_test.go`:

```go
package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func serve(t *testing.T, body []byte, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFileCacheFetchesVerifiesAndReuses(t *testing.T) {
	body := []byte("model weights")
	var hits atomic.Int32
	srv := serve(t, body, &hits)
	cache := NewFileCache(t.TempDir(), srv.Client())
	f := File{URL: srv.URL + "/m", SHA256: digest(body), Bytes: int64(len(body))}

	path, err := cache.Fetch(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) || !cache.Has(f.SHA256) || path != cache.Path(f.SHA256) {
		t.Fatalf("cached %q at %s", got, path)
	}
	if _, err := cache.Fetch(context.Background(), f); err != nil || hits.Load() != 1 {
		t.Fatalf("second fetch: err=%v downloads=%d, want one download", err, hits.Load())
	}
}

func TestFileCacheRejectsTamperedAndOversizedBodies(t *testing.T) {
	good := []byte("model weights")
	cases := map[string][]byte{
		"tampered":  []byte("tampered data"), // same length, different digest
		"oversized": append(append([]byte{}, good...), '!'),
	}
	for name, served := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			srv := serve(t, served, nil)
			cache := NewFileCache(root, srv.Client())
			f := File{URL: srv.URL + "/m", SHA256: digest(good), Bytes: int64(len(good))}
			if _, err := cache.Fetch(context.Background(), f); !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("Fetch = %v, want ErrDigestMismatch", err)
			}
			if cache.Has(f.SHA256) {
				t.Fatal("an unverified file was cached")
			}
			if left, _ := os.ReadDir(filepath.Join(root, "sha256")); len(left) != 0 {
				t.Fatalf("partial downloads left behind: %v", left)
			}
		})
	}
}

func TestFileCacheReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	cache := NewFileCache(t.TempDir(), srv.Client())
	_, err := cache.Fetch(context.Background(), File{URL: srv.URL + "/m", SHA256: digest([]byte("x")), Bytes: 1})
	if err == nil || errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Fetch = %v, want an HTTP error", err)
	}
}

func TestEngineCached(t *testing.T) {
	root := t.TempDir()
	sha := digest([]byte("m"))
	if engineCached(root, sha, "sm_87") {
		t.Fatal("found an engine in an empty cache")
	}
	if err := os.MkdirAll(filepath.Join(root, sha), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sha, "sm_87-trt10.7.plan"), []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !engineCached(root, sha, "sm_87") || engineCached(root, sha, "sm_110") || engineCached(root, sha, "") {
		t.Fatal("engine lookup ignores the GPU architecture")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/ -run 'TestFileCache|TestEngineCached'`
Expected: FAIL to compile with `undefined: NewFileCache`.

- [ ] **Step 3: Write the implementation**

`go/internal/agent/models/files.go`:

```go
package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// ErrDigestMismatch means a downloaded model file did not match the catalog.
var ErrDigestMismatch = errors.New("model file failed verification")

// FileCache keeps verified model files under root/sha256/<digest>. The
// content-addressed layout is the one WDY-3139 proposes for a device-side
// artifact store.
type FileCache struct {
	root   string
	client *http.Client

	mu    sync.Mutex
	locks map[string]*sync.Mutex // one per digest, so one download serves concurrent starts
}

// NewFileCache stores files under root. A nil client uses http.DefaultClient.
func NewFileCache(root string, client *http.Client) *FileCache {
	if client == nil {
		client = http.DefaultClient
	}
	return &FileCache{root: root, client: client, locks: map[string]*sync.Mutex{}}
}

// Path is where the file with this digest lives once fetched.
func (c *FileCache) Path(sha string) string { return filepath.Join(c.root, "sha256", sha) }

// Has reports whether a verified copy is already cached.
func (c *FileCache) Has(sha string) bool {
	info, err := os.Stat(c.Path(sha))
	return err == nil && info.Mode().IsRegular()
}

// Fetch returns the path of a verified copy of f, downloading it first when
// needed. A download is renamed into place only after its size and digest
// match, so every cached path is verified.
func (c *FileCache) Fetch(ctx context.Context, f File) (string, error) {
	lock := c.lock(f.SHA256)
	lock.Lock()
	defer lock.Unlock()
	path := c.Path(f.SHA256)
	if c.Has(f.SHA256) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // a no-op after the rename
	defer tmp.Close()
	if err := c.download(ctx, f, tmp); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func (c *FileCache) download(ctx context.Context, f File, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading model file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading model file: %s", resp.Status)
	}
	h := sha256.New()
	// Read one byte past the declared size so an oversized body is caught.
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, f.Bytes+1))
	if err != nil {
		return fmt.Errorf("downloading model file: %w", err)
	}
	if n != f.Bytes {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrDigestMismatch, n, f.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%w: sha256 %s, want %s", ErrDigestMismatch, got, f.SHA256)
	}
	return nil
}

func (c *FileCache) lock(sha string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[sha]
	if !ok {
		l = &sync.Mutex{}
		c.locks[sha] = l
	}
	return l
}

// engineCached reports whether a TensorRT engine built from fileSHA for this
// GPU architecture exists. Hosts name engines <gpu_arch>-trt<version>.plan and
// record their provenance beside them (design §6.3).
func engineCached(enginesRoot, fileSHA, gpuArch string) bool {
	if gpuArch == "" {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(enginesRoot, fileSHA, gpuArch+"-trt*.plan"))
	return len(matches) > 0
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./go/internal/agent/models/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/models/files.go go/internal/agent/models/files_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: add a verified, content-addressed model file cache

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 6: models: the Supervisor's start, prepare, run and stop

This task builds instances without watches: `Start` runs a host until `Stop` or `Shutdown`, and a host exit is final. Task 7 adds watches and leases, Task 8 detections, and Task 9 restarts and engine builds.

**Files:**
- Create: `go/internal/agent/models/runtime.go`
- Create: `go/internal/agent/models/clock.go`
- Create: `go/internal/agent/models/instance.go`
- Create: `go/internal/agent/models/supervisor.go`
- Create: `go/internal/agent/models/lifecycle.go`
- Create: `go/internal/agent/models/modelstest/fakes.go`
- Test: `go/internal/agent/models/supervisor_test.go` (package `models_test`)

**Interfaces:**
- Consumes: Tasks 3–5.
- Produces (package `models`):
  - host paths and limits: `HostModelDir`, `HostModelFile`, `HostLabelsFile`, `HostEngineCacheDir`, `HostMaxFPS`, `HostConfidenceFloor`, `HostUID`, `HostGID`;
  - `HostSpec{InstanceID, AppID, Image, Engine, ModelID, VariantID, FileSHA256, ModelFile, LabelsFile, EngineCache, CameraNode, CameraSource, LogPath string}` with `(HostSpec) Env() []string`, and `HostExit{Code uint32; Err error}`;
  - the `Runtime` interface: `HasImage(ctx, ref) bool`, `EnsureImage(ctx, ref) error`, `StartHost(ctx, HostSpec) (<-chan HostExit, error)`, `RemoveHost(ctx, instanceID) error` and `ListHosts(ctx) ([]string, error)`;
  - `Camera{SourceID, Name string}`, the `Cameras` interface (`List`, `Acquire(ctx, owner, sourceID) (string, error)`, `Release(ctx, owner)`) and the `Files` interface (`Has(sha) bool`, `Fetch(ctx, File) (string, error)`);
  - `Clock` (`Now()`, `AfterFunc(d, f) Timer`) and `Timer` (`Stop() bool`);
  - `Config{Catalog; Device; Runtime; Cameras; Files; Root string; Logger *zap.Logger; Clock; MaxRunning int}`;
  - `NewSupervisor(Config) *Supervisor`;
  - catalog view: `(*Supervisor) Catalog(ctx) CatalogView`, where `CatalogView{Version; Models []CatalogEntry; Cameras []Camera; MaxRunning, Running int}` and `CatalogEntry{Model; Variant *Variant; UnavailableReason string; ImageCached bool; DownloadBytes int64; NeedsEngineBuild bool}`;
  - instance control: `(*Supervisor) Start(ctx, modelID, cameraSourceID) (InstanceInfo, bool, error)`, `List() []InstanceInfo`, `Stop(ctx, instanceID, watchID string) (InstanceInfo, error)` (`watchID` is only supported from Task 7), `Shutdown(ctx)` and `PublishApplicationRecord(appID string, rec data.ApplicationRecord)`.
- Produces (package `modelstest`):
  - `NewClock() *Clock` with `Advance(d)`;
  - `NewRuntime() *Runtime`, with field `BlockEnsure chan struct{}` and methods `Exit(id, code)`, `AddLeftover(id)`, `Starts(id) int`, `Running(id) bool`, `LastSpec() models.HostSpec` and `Removed() []string`;
  - `NewCameras(...models.Camera) *Cameras` with `Refuse(sourceID)` and `Owners() []string`;
  - `NewFiles() *Files`;
  - record builders `Status(state)`, `Failed(reason)`, `Entered(class, confidence, track)` and `Left(class, confidence, track)`;
  - `Catalog(engine) models.Catalog`;
  - test helpers `Eventually(t, what, cond)` and `AdvanceUntil(t, clock, step, what, cond)`.

- [ ] **Step 1: Write the interfaces the tests need**

`go/internal/agent/models/runtime.go`:

```go
package models

import (
	"context"
	"sort"
	"strconv"
)

// Paths inside a model host container (design §6.2).
const (
	HostModelDir       = "/run/wendy/model"
	HostModelFile      = HostModelDir + "/model"
	HostLabelsFile     = HostModelDir + "/labels.txt"
	HostEngineCacheDir = HostModelDir + "/engines"
)

// Limits every model host receives.
const (
	HostMaxFPS          = 10
	HostConfidenceFloor = 0.30
)

// HostUID and HostGID are the unprivileged identity model hosts run as.
const (
	HostUID = 65534
	HostGID = 65534
)

// HostSpec is what a Runtime needs to create one model host container.
type HostSpec struct {
	InstanceID   string
	AppID        string // AppIDPrefix + InstanceID: the data socket and cgroup identity
	Image        string // pinned by digest
	Engine       string
	ModelID      string
	VariantID    string
	FileSHA256   string
	ModelFile    string // host path, mounted read-only at HostModelFile
	LabelsFile   string // host path, mounted read-only at HostLabelsFile
	EngineCache  string // host directory mounted read-write at HostEngineCacheDir; TensorRT only
	CameraNode   string // the camera's two-plane node, bound at the same path
	CameraSource string // canonical source id, e.g. "v4l2:/dev/video0"
	LogPath      string // host file that receives the container's stdout and stderr
}

// Env is the environment half of the host contract. The runtime adds
// WENDY_DATA_SOCKET when it mounts the socket.
func (h HostSpec) Env() []string {
	env := []string{
		"WENDY_MODEL_INSTANCE=" + h.InstanceID,
		"WENDY_MODEL_VARIANT=" + h.VariantID,
		"WENDY_MODEL_FILE=" + HostModelFile,
		"WENDY_MODEL_LABELS=" + HostLabelsFile,
		"WENDY_CAMERA_NODE=" + h.CameraNode,
		"WENDY_CAMERA_SOURCE=" + h.CameraSource,
		"WENDY_MODEL_MAX_FPS=" + strconv.Itoa(HostMaxFPS),
		"WENDY_MODEL_CONFIDENCE_FLOOR=" + strconv.FormatFloat(HostConfidenceFloor, 'f', 2, 64),
	}
	if h.EngineCache != "" {
		env = append(env, "WENDY_MODEL_ENGINE_CACHE="+HostEngineCacheDir)
	}
	sort.Strings(env)
	return env
}

// HostExit reports that a model host's process ended.
type HostExit struct {
	Code uint32
	Err  error
}

// Runtime creates and removes model host containers.
type Runtime interface {
	// HasImage reports whether ref is already on the device, so the catalog
	// can say whether a first start downloads it.
	HasImage(ctx context.Context, ref string) bool
	EnsureImage(ctx context.Context, ref string) error
	// StartHost creates and starts a host. The channel receives exactly one
	// value when the host's process ends, for any reason.
	StartHost(ctx context.Context, spec HostSpec) (<-chan HostExit, error)
	// RemoveHost stops and deletes a host; removing a missing host is not an error.
	RemoveHost(ctx context.Context, instanceID string) error
	// ListHosts returns the instance ids of every existing host container.
	ListHosts(ctx context.Context) ([]string, error)
}

// Camera is a local camera a model can watch.
type Camera struct {
	SourceID string // e.g. "v4l2:/dev/video0"
	Name     string
}

// Cameras lists local cameras and pins the nodes models read them from.
type Cameras interface {
	List(ctx context.Context) []Camera
	// Acquire keeps the two-plane path running for owner and returns the node
	// carrying sourceID. Failures wrap ErrCameraNotStreamable.
	Acquire(ctx context.Context, owner, sourceID string) (string, error)
	Release(ctx context.Context, owner string)
}

// Files provides verified model files. *FileCache implements it.
type Files interface {
	Has(sha256 string) bool
	Fetch(ctx context.Context, f File) (string, error)
}
```

`go/internal/agent/models/clock.go`:

```go
package models

import "time"

// Clock is the time source the supervisor schedules with; tests use a fake.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a scheduled callback.
type Timer interface{ Stop() bool }

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
```

`go/internal/agent/models/modelstest/fakes.go`:

```go
// Package modelstest provides fakes for testing code built on package models.
package modelstest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
)

// Clock is a manual clock. AfterFunc callbacks run, in deadline order, when
// Advance moves time past them.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
}

type timer struct {
	clock *Clock
	at    time.Time
	f     func()
	done  bool // fired or stopped
}

func (t *timer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) AfterFunc(d time.Duration, f func()) models.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{clock: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves time forward by d and runs every callback that falls due, on
// the calling goroutine and without holding the clock's lock.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*timer
	kept := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.done:
		case !t.at.After(c.now):
			t.done = true
			due = append(due, t)
		default:
			kept = append(kept, t)
		}
	}
	c.timers = kept
	c.mu.Unlock()
	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.f()
	}
}

// Runtime is an in-memory models.Runtime.
type Runtime struct {
	// BlockEnsure, when set before the supervisor uses the runtime, makes
	// EnsureImage wait for it to close or for the context to end.
	BlockEnsure chan struct{}

	mu        sync.Mutex
	images    map[string]bool
	hosts     map[string]chan models.HostExit
	starts    map[string]int
	specs     []models.HostSpec
	removed   []string
	leftovers []string
}

func NewRuntime() *Runtime {
	return &Runtime{images: map[string]bool{}, hosts: map[string]chan models.HostExit{}, starts: map[string]int{}}
}

func (r *Runtime) HasImage(_ context.Context, ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.images[ref]
}

func (r *Runtime) EnsureImage(ctx context.Context, ref string) error {
	if r.BlockEnsure != nil {
		select {
		case <-r.BlockEnsure:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[ref] = true
	return nil
}

func (r *Runtime) StartHost(_ context.Context, spec models.HostSpec) (<-chan models.HostExit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.hosts[spec.InstanceID]; running {
		return nil, fmt.Errorf("host %s already runs", spec.InstanceID)
	}
	ch := make(chan models.HostExit, 1)
	r.hosts[spec.InstanceID] = ch
	r.starts[spec.InstanceID]++
	r.specs = append(r.specs, spec)
	return ch, nil
}

// RemoveHost ends a running host the way killing its task does.
func (r *Runtime) RemoveHost(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.hosts[id]; ok {
		ch <- models.HostExit{Code: 137}
		delete(r.hosts, id)
	}
	r.leftovers = slices.DeleteFunc(r.leftovers, func(s string) bool { return s == id })
	r.removed = append(r.removed, id)
	return nil
}

func (r *Runtime) ListHosts(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := slices.Clone(r.leftovers)
	for id := range r.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Exit ends a running host's process with code, as a crash would.
func (r *Runtime) Exit(id string, code uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.hosts[id]; ok {
		ch <- models.HostExit{Code: code}
		delete(r.hosts, id)
	}
}

// AddLeftover records a host container a previous agent process left behind.
func (r *Runtime) AddLeftover(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leftovers = append(r.leftovers, id)
}

func (r *Runtime) Starts(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[id]
}

func (r *Runtime) Running(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.hosts[id]
	return ok
}

func (r *Runtime) LastSpec() models.HostSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		return models.HostSpec{}
	}
	return r.specs[len(r.specs)-1]
}

func (r *Runtime) Removed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.removed)
}

// Cameras is a fixed camera list whose nodes can be pinned.
type Cameras struct {
	mu     sync.Mutex
	cams   []models.Camera
	refuse map[string]bool
	owners map[string]string // owner -> source
}

func NewCameras(cams ...models.Camera) *Cameras {
	return &Cameras{cams: cams, refuse: map[string]bool{}, owners: map[string]string{}}
}

// Refuse makes Acquire fail for sourceID, like a stream that cannot carry
// frame identity.
func (c *Cameras) Refuse(sourceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refuse[sourceID] = true
}

func (c *Cameras) List(context.Context) []models.Camera {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.cams)
}

func (c *Cameras) Acquire(_ context.Context, owner, sourceID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse[sourceID] {
		return "", fmt.Errorf("%w: %s carries no frame identity", models.ErrCameraNotStreamable, sourceID)
	}
	c.owners[owner] = sourceID
	return "/dev/video255", nil
}

func (c *Cameras) Release(_ context.Context, owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.owners, owner)
}

// Owners lists the owners holding a camera pin.
func (c *Cameras) Owners() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for o := range c.owners {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// Files is an in-memory models.Files.
type Files struct {
	mu   sync.Mutex
	have map[string]bool
}

func NewFiles() *Files { return &Files{have: map[string]bool{}} }

func (f *Files) Has(sha string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.have[sha]
}

func (f *Files) Fetch(_ context.Context, file models.File) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.have[file.SHA256] = true
	return "/var/lib/wendy/models/files/sha256/" + file.SHA256, nil
}

// Status builds the model.status record a healthy host heartbeats.
func Status(state string) data.ApplicationRecord {
	return data.ApplicationRecord{Version: 1, Type: "event", Name: models.RecordStatus,
		Attributes: map[string]any{"state": state, "processed_fps": 9.5, "latency_p50_ms": 31.0}}
}

// Failed builds the final model.status record of a host that gives up.
func Failed(reason string) data.ApplicationRecord {
	rec := Status(models.HostFailed)
	rec.Attributes["reason"] = reason
	return rec
}

// Entered builds a model.entered record for a detection.
func Entered(class string, confidence float64, track int) data.ApplicationRecord {
	return detection(models.RecordEntered, class, confidence, track)
}

// Left builds a model.left record for a detection.
func Left(class string, confidence float64, track int) data.ApplicationRecord {
	return detection(models.RecordLeft, class, confidence, track)
}

func detection(name, class string, confidence float64, track int) data.ApplicationRecord {
	return data.ApplicationRecord{Version: 1, Type: "event", Name: name, Model: "test",
		Attributes: map[string]any{"class": class, "confidence": confidence, "track_id": float64(track),
			"box": map[string]any{"x": 0.1, "y": 0.1, "width": 0.2, "height": 0.4}},
		Inputs: []data.SampleRef{{SourceID: "v4l2:/dev/video0", SampleID: uint64(track)}}}
}

// Catalog is a one-model catalog whose only variant runs anywhere on engine.
func Catalog(engine string) models.Catalog {
	return models.Catalog{Version: "test", Models: []models.Model{{
		ID: "coco-detector", Description: "test detector", Kind: models.KindDetector,
		Labels: []string{"person", "car", "dog"},
		Variants: []models.Variant{{ID: "test-" + engine, Engine: engine,
			HostImage: "ghcr.io/wendylabsinc/wendy-model-host-test@sha256:" + strings.Repeat("a", 64),
			File:      models.File{URL: "https://example.invalid/model", SHA256: strings.Repeat("1", 64), Bytes: 10},
			InputSize: 320}},
	}}}
}

// Eventually polls cond for up to two seconds.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// AdvanceUntil advances clock in steps until cond holds, pausing between
// steps so the supervisor's goroutines can arm their timers.
func AdvanceUntil(t testing.TB, clock *Clock, step time.Duration, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		clock.Advance(step)
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out advancing the clock until %s", what)
}
```

- [ ] **Step 2: Write the failing tests**

`go/internal/agent/models/supervisor_test.go`:

```go
package models_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

const (
	frontDoor = "v4l2:/dev/video0"
	garage    = "v4l2:/dev/video2"
)

type harness struct {
	sup     *models.Supervisor
	clock   *modelstest.Clock
	runtime *modelstest.Runtime
	cameras *modelstest.Cameras
	files   *modelstest.Files
	root    string
}

func newHarness(t *testing.T, engine string, configure ...func(*models.Config)) *harness {
	t.Helper()
	h := &harness{
		clock:   modelstest.NewClock(),
		runtime: modelstest.NewRuntime(),
		cameras: modelstest.NewCameras(models.Camera{SourceID: frontDoor, Name: "front door"}, models.Camera{SourceID: garage, Name: "garage"}),
		files:   modelstest.NewFiles(),
		root:    t.TempDir(),
	}
	cfg := models.Config{
		Catalog: modelstest.Catalog(engine),
		Device:  models.DeviceProfile{Arch: "arm64", GPUArch: "sm_87"},
		Runtime: h.runtime, Cameras: h.cameras, Files: h.files, Root: h.root, Clock: h.clock,
	}
	for _, c := range configure {
		c(&cfg)
	}
	h.sup = models.NewSupervisor(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.sup.Shutdown(ctx)
	})
	return h
}

// info returns the instance's snapshot; State is zero once it is gone.
func (h *harness) info(id string) models.InstanceInfo {
	for _, i := range h.sup.List() {
		if i.ID == id {
			return i
		}
	}
	return models.InstanceInfo{}
}

// startReady starts the detector on camera and has the host report ready.
func (h *harness) startReady(t *testing.T, camera string) models.InstanceInfo {
	t.Helper()
	info, _, err := h.sup.Start(context.Background(), "coco-detector", camera)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "the instance to be ready", func() bool { return h.info(info.ID).State == models.StateReady })
	return h.info(info.ID)
}

func TestStartRunsHostAndBecomesReady(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info, reused, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil || reused {
		t.Fatalf("Start = %+v, %v, %v", info, reused, err)
	}
	if info.State != models.StatePreparing {
		t.Fatalf("state = %v, want preparing", info.State)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })

	spec := h.runtime.LastSpec()
	if spec.AppID != models.AppIDPrefix+info.ID || spec.CameraNode != "/dev/video255" || spec.CameraSource != frontDoor {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.EngineCache != "" {
		t.Fatalf("an ONNX Runtime host got an engine cache: %q", spec.EngineCache)
	}
	if labels, err := os.ReadFile(spec.LabelsFile); err != nil || string(labels) != "person\ncar\ndog\n" {
		t.Fatalf("labels = %q, %v", labels, err)
	}
	if !slices.Contains(spec.Env(), "WENDY_MODEL_FILE="+models.HostModelFile) {
		t.Fatalf("env = %v", spec.Env())
	}

	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "ready", func() bool { return h.info(info.ID).State == models.StateReady })
	if got := h.info(info.ID).Stats.ProcessedFPS; got != 9.5 {
		t.Fatalf("processed fps = %v, want the host's 9.5", got)
	}
}

func TestStartReusesInstanceAndEnforcesCapacity(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime, func(c *models.Config) { c.MaxRunning = 1 })
	ctx := context.Background()
	first, _, err := h.sup.Start(ctx, "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	again, reused, err := h.sup.Start(ctx, "coco-detector", frontDoor)
	if err != nil || !reused || again.ID != first.ID {
		t.Fatalf("second start = %+v, reused=%v, %v; want the same instance", again, reused, err)
	}
	if _, _, err := h.sup.Start(ctx, "coco-detector", garage); !errors.Is(err, models.ErrCapacity) {
		t.Fatalf("start past capacity = %v, want ErrCapacity", err)
	}
}

func TestConcurrentStartsShareOneInstance(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = info.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent starts made more than one instance: %v", ids)
		}
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(ids[0]) })
	if n := len(h.sup.List()); n != 1 {
		t.Fatalf("%d instances, want 1", n)
	}
}

func TestStartRejectsWhatCannotRun(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	ctx := context.Background()
	if _, _, err := h.sup.Start(ctx, "no-such-model", frontDoor); !errors.Is(err, models.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
	if _, _, err := h.sup.Start(ctx, "coco-detector", "v4l2:/dev/video9"); !errors.Is(err, models.ErrUnknownCamera) {
		t.Fatalf("unknown camera: %v", err)
	}
	amd := newHarness(t, models.EngineONNXRuntime, func(c *models.Config) {
		c.Catalog.Models[0].Variants[0].Requires = models.Requires{Arch: "arm64"}
		c.Device.Arch = "amd64"
	})
	if _, _, err := amd.sup.Start(ctx, "coco-detector", frontDoor); !errors.Is(err, models.ErrNoVariant) {
		t.Fatalf("no variant: %v", err)
	}
}

func TestRefusedCameraDoesNotTakeASlot(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime, func(c *models.Config) { c.MaxRunning = 1 })
	h.cameras.Refuse(frontDoor)
	if _, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor); !errors.Is(err, models.ErrCameraNotStreamable) {
		t.Fatalf("refused camera: %v", err)
	}
	if n := len(h.sup.List()); n != 0 {
		t.Fatalf("a refused start left %d instances", n)
	}
	if owners := h.cameras.Owners(); len(owners) != 0 {
		t.Fatalf("a refused start left camera pins: %v", owners)
	}
	if _, _, err := h.sup.Start(context.Background(), "coco-detector", garage); err != nil {
		t.Fatalf("the only slot was lost to a refused start: %v", err)
	}
}

func TestStopRemovesHostCameraAndRunDir(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	final, err := h.sup.Stop(context.Background(), info.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if final.State != models.StateStopped || final.StateDetail != "stopped" {
		t.Fatalf("final = %v %q", final.State, final.StateDetail)
	}
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the host container was not removed")
	}
	if len(h.cameras.Owners()) != 0 {
		t.Fatal("the camera pin was not released")
	}
	if _, err := os.Stat(filepath.Join(h.root, "run", info.ID)); !os.IsNotExist(err) {
		t.Fatalf("the run directory remains: %v", err)
	}
	if len(h.sup.List()) != 0 {
		t.Fatal("a stopped instance is still listed")
	}
	if _, err := h.sup.Stop(context.Background(), info.ID, ""); !errors.Is(err, models.ErrUnknownInstance) {
		t.Fatalf("second stop: %v", err)
	}
}

func TestStopDuringImagePullCancelsIt(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	h.runtime.BlockEnsure = make(chan struct{}) // the pull never finishes on its own
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the pull to begin", func() bool { return h.info(info.ID).StateDetail == "pulling host image" })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	final, err := h.sup.Stop(ctx, info.ID, "")
	if err != nil {
		t.Fatalf("stop did not interrupt the pull: %v", err)
	}
	if final.State != models.StateStopped {
		t.Fatalf("final state = %v", final.State)
	}
	if h.runtime.Starts(info.ID) != 0 {
		t.Fatal("a host started after the instance was stopped")
	}
}

func TestHostReportedFailureRemovesInstance(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Failed("no frames from camera"))
	modelstest.Eventually(t, "the failed instance to go", func() bool { return h.info(info.ID).State == 0 })
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the failed host was not removed")
	}
}

func TestCatalogReportsFitAndFirstStartCost(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	view := h.sup.Catalog(context.Background())
	if len(view.Models) != 1 || view.Models[0].Variant == nil {
		t.Fatalf("view = %+v", view)
	}
	entry := view.Models[0]
	if entry.ImageCached || entry.DownloadBytes != 10 || entry.NeedsEngineBuild {
		t.Fatalf("first-start cost = %+v", entry)
	}
	if len(view.Cameras) != 2 || view.MaxRunning != 2 || view.Running != 0 {
		t.Fatalf("view = %+v", view)
	}
	h.startReady(t, frontDoor)
	entry = h.sup.Catalog(context.Background()).Models[0]
	if !entry.ImageCached || entry.DownloadBytes != 0 {
		t.Fatalf("after a start = %+v", entry)
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/...`
Expected: FAIL to compile with `undefined: models.NewSupervisor` (and `models.Config`).

- [ ] **Step 4: Write the instance and the supervisor**

`go/internal/agent/models/instance.go`:

```go
package models

import (
	"context"
	"sync"
	"time"
)

// instance is one model instance. Fields above mu are fixed at creation; the
// rest are guarded by mu.
type instance struct {
	id      string
	model   Model
	variant Variant
	camera  string
	started time.Time
	runDir  string

	ctx    context.Context // cancelled when the instance must stop
	cancel context.CancelFunc
	wake   chan struct{} // pokes the run loop; capacity 1
	done   chan struct{} // closed once the instance is fully removed

	mu          sync.Mutex
	state       State
	detail      string
	stats       Stats
	node        string // the camera's two-plane node
	modelFile   string // the verified model file on the device
	stopReason  string
	hostRunning bool      // a host from the current start speaks for the instance
	hostFailure string    // reason from a host-reported failure
	lastStatus  time.Time // latest model.status from the live host
}

func (inst *instance) poke() {
	select {
	case inst.wake <- struct{}{}:
	default:
	}
}

// infoLocked snapshots the instance. Caller holds mu.
func (inst *instance) infoLocked() InstanceInfo {
	return InstanceInfo{
		ID: inst.id, ModelID: inst.model.ID, VariantID: inst.variant.ID, Engine: inst.variant.Engine,
		CameraSourceID: inst.camera, State: inst.state, StateDetail: inst.detail,
		StartedAt: inst.started, Stats: inst.stats, FileSHA256: inst.variant.File.SHA256,
	}
}

func (inst *instance) info() InstanceInfo {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.infoLocked()
}

func (inst *instance) failure() string {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.hostFailure
}

// setStateLocked records a state change and reports whether anything
// changed. Caller holds mu.
func (inst *instance) setStateLocked(s State, detail string) bool {
	if inst.state == s && inst.detail == detail {
		return false
	}
	inst.state, inst.detail = s, detail
	return true
}

// requestStopLocked asks the run loop to stop the instance; the first reason
// is the one reported. Caller holds mu.
func (inst *instance) requestStopLocked(reason string) {
	if inst.stopReason == "" {
		inst.stopReason = reason
	}
	inst.cancel()
}
```

`go/internal/agent/models/supervisor.go`:

```go
package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"go.uber.org/zap"
)

// Limits and lifetimes from the design (§5, §7).
const (
	defaultMaxRunning = 2
	cameraTimeout     = 10 * time.Second
	removeTimeout     = 30 * time.Second
)

// Config holds a Supervisor's dependencies.
type Config struct {
	Catalog    Catalog
	Device     DeviceProfile
	Runtime    Runtime
	Cameras    Cameras
	Files      Files
	Root       string // state directory, e.g. /var/lib/wendy/models
	Logger     *zap.Logger
	Clock      Clock // nil: the real clock
	MaxRunning int   // 0: two
}

// Supervisor owns every model instance on the device.
type Supervisor struct {
	cfg   Config
	clock Clock
	log   *zap.Logger

	mu        sync.Mutex
	instances map[string]*instance
	byKey     map[string]*instance // model id + NUL + camera source id
}

// NewSupervisor returns a Supervisor with no instances.
func NewSupervisor(cfg Config) *Supervisor {
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.MaxRunning == 0 {
		cfg.MaxRunning = defaultMaxRunning
	}
	return &Supervisor{cfg: cfg, clock: cfg.Clock, log: cfg.Logger,
		instances: map[string]*instance{}, byKey: map[string]*instance{}}
}

// CatalogEntry is one catalog model as it applies to this device.
type CatalogEntry struct {
	Model             Model
	Variant           *Variant // nil when no variant fits the device
	UnavailableReason string
	ImageCached       bool
	DownloadBytes     int64 // model file bytes a first start downloads; 0 when cached
	NeedsEngineBuild  bool
}

// CatalogView is the catalog, the cameras, and the free slots on this device.
type CatalogView struct {
	Version    string
	Models     []CatalogEntry
	Cameras    []Camera
	MaxRunning int
	Running    int
}

// Catalog describes what this device can run right now.
func (s *Supervisor) Catalog(ctx context.Context) CatalogView {
	view := CatalogView{Version: s.cfg.Catalog.Version, Cameras: s.cfg.Cameras.List(ctx), MaxRunning: s.cfg.MaxRunning}
	for _, m := range s.cfg.Catalog.Models {
		entry := CatalogEntry{Model: m}
		v, reason, ok := SelectVariant(m, s.cfg.Device)
		if !ok {
			entry.UnavailableReason = reason
			view.Models = append(view.Models, entry)
			continue
		}
		entry.Variant = &v
		entry.ImageCached = s.cfg.Runtime.HasImage(ctx, v.HostImage)
		if !s.cfg.Files.Has(v.File.SHA256) {
			entry.DownloadBytes = v.File.Bytes
		}
		entry.NeedsEngineBuild = s.needsEngineBuild(v)
		view.Models = append(view.Models, entry)
	}
	s.mu.Lock()
	view.Running = len(s.instances)
	s.mu.Unlock()
	return view
}

func (s *Supervisor) needsEngineBuild(v Variant) bool {
	return v.Engine == EngineTensorRT && !engineCached(s.enginesDir(), v.File.SHA256, s.cfg.Device.GPUArch)
}

func (s *Supervisor) enginesDir() string { return filepath.Join(s.cfg.Root, "engines") }

// Start runs modelID on the camera, or returns the instance already doing so
// (reused). The instance prepares in the background; watch it for progress.
func (s *Supervisor) Start(ctx context.Context, modelID, cameraSourceID string) (InstanceInfo, bool, error) {
	model, ok := s.cfg.Catalog.Model(modelID)
	if !ok {
		return InstanceInfo{}, false, fmt.Errorf("%w %q", ErrUnknownModel, modelID)
	}
	variant, reason, ok := SelectVariant(model, s.cfg.Device)
	if !ok {
		return InstanceInfo{}, false, fmt.Errorf("%w: %s", ErrNoVariant, reason)
	}
	if !s.hasCamera(ctx, cameraSourceID) {
		return InstanceInfo{}, false, fmt.Errorf("%w %q", ErrUnknownCamera, cameraSourceID)
	}

	key := modelID + "\x00" + cameraSourceID
	s.mu.Lock()
	if existing := s.byKey[key]; existing != nil {
		s.mu.Unlock()
		return existing.info(), true, nil
	}
	if len(s.instances) >= s.cfg.MaxRunning {
		running := s.idsLocked()
		s.mu.Unlock()
		return InstanceInfo{}, false, fmt.Errorf("%w (%d): %s", ErrCapacity, s.cfg.MaxRunning, strings.Join(running, ", "))
	}
	inst := s.newInstance(model, variant, cameraSourceID)
	s.instances[inst.id] = inst
	s.byKey[key] = inst
	s.mu.Unlock()

	// A camera that cannot stream is the caller's error, not a failed
	// instance, so it is checked before the start counts.
	acquireCtx, cancel := context.WithTimeout(ctx, cameraTimeout)
	node, err := s.cfg.Cameras.Acquire(acquireCtx, inst.id, cameraSourceID)
	cancel()
	if err != nil {
		s.forget(inst)
		inst.cancel()
		close(inst.done)
		if !errors.Is(err, ErrCameraNotStreamable) {
			err = fmt.Errorf("%w: %v", ErrCameraNotStreamable, err)
		}
		return InstanceInfo{}, false, err
	}

	inst.mu.Lock()
	inst.node = node
	info := inst.infoLocked()
	inst.mu.Unlock()
	go s.run(inst)
	return info, false, nil
}

func (s *Supervisor) hasCamera(ctx context.Context, sourceID string) bool {
	for _, c := range s.cfg.Cameras.List(ctx) {
		if c.SourceID == sourceID {
			return true
		}
	}
	return false
}

func (s *Supervisor) idsLocked() []string {
	ids := make([]string, 0, len(s.instances))
	for id := range s.instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Supervisor) newInstance(m Model, v Variant, camera string) *instance {
	id := newID("m-")
	ctx, cancel := context.WithCancel(context.Background())
	return &instance{
		id: id, model: m, variant: v, camera: camera, started: s.clock.Now(),
		runDir: filepath.Join(s.cfg.Root, "run", id),
		ctx:    ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
		state: StatePreparing, detail: "starting",
	}
}

// newID returns prefix followed by eight random hex digits.
func newID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// forget removes inst from the supervisor's maps, if it is still there.
func (s *Supervisor) forget(inst *instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instances[inst.id] == inst {
		delete(s.instances, inst.id)
	}
	if key := inst.model.ID + "\x00" + inst.camera; s.byKey[key] == inst {
		delete(s.byKey, key)
	}
}

func (s *Supervisor) lookup(id string) (*instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst := s.instances[id]; inst != nil {
		return inst, nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownInstance, id)
}

// List returns every instance, oldest first.
func (s *Supervisor) List() []InstanceInfo {
	s.mu.Lock()
	insts := make([]*instance, 0, len(s.instances))
	for _, inst := range s.instances {
		insts = append(insts, inst)
	}
	s.mu.Unlock()
	out := make([]InstanceInfo, 0, len(insts))
	for _, inst := range insts {
		out = append(out, inst.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Stop with an empty watchID stops the instance outright. With a watchID it
// ends that watch, and stops the instance if it was the last. When the
// instance stops, Stop waits for its removal and returns the final state.
func (s *Supervisor) Stop(ctx context.Context, instanceID, watchID string) (InstanceInfo, error) {
	inst, err := s.lookup(instanceID)
	if err != nil {
		return InstanceInfo{}, err
	}
	if watchID != "" {
		return inst.info(), fmt.Errorf("%w %q", ErrUnknownWatch, watchID)
	}
	inst.mu.Lock()
	inst.requestStopLocked("stopped")
	inst.mu.Unlock()
	return s.awaitRemoval(ctx, inst)
}

func (s *Supervisor) awaitRemoval(ctx context.Context, inst *instance) (InstanceInfo, error) {
	select {
	case <-inst.done:
		return inst.info(), nil
	case <-ctx.Done():
		return inst.info(), ctx.Err()
	}
}

// Shutdown stops every instance and waits for their removal, or for ctx.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.Lock()
	insts := make([]*instance, 0, len(s.instances))
	for _, inst := range s.instances {
		insts = append(insts, inst)
	}
	s.mu.Unlock()
	for _, inst := range insts {
		inst.mu.Lock()
		inst.requestStopLocked("the agent is shutting down")
		inst.mu.Unlock()
	}
	for _, inst := range insts {
		select {
		case <-inst.done:
		case <-ctx.Done():
			return
		}
	}
}

// PublishApplicationRecord receives every record the app data socket
// accepts. Records from model hosts update their instance; everything else is
// ignored. It never blocks, so it is safe on the socket's read path.
func (s *Supervisor) PublishApplicationRecord(appID string, rec data.ApplicationRecord) {
	id, ok := strings.CutPrefix(appID, AppIDPrefix)
	if !ok || rec.Type != "event" {
		return
	}
	s.mu.Lock()
	inst := s.instances[id]
	s.mu.Unlock()
	if inst == nil {
		return
	}
	if rec.Name == RecordStatus {
		st, err := parseStatus(rec)
		if err != nil {
			s.log.Debug("ignoring a malformed model status", zap.String("instance", id), zap.Error(err))
			return
		}
		s.handleStatus(inst, st)
	}
}
```

`go/internal/agent/models/lifecycle.go`:

```go
package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// run takes an instance from preparation to removal.
func (s *Supervisor) run(inst *instance) {
	final, detail := StateStopped, ""
	defer func() { s.finish(inst, final, detail) }()
	if err := s.prepare(inst); err != nil {
		if inst.ctx.Err() == nil {
			final, detail = StateFailed, err.Error()
		}
		return
	}
	if failed, reason := s.runHost(inst); failed {
		final, detail = StateFailed, reason
	}
}

// prepare pulls the host image, fetches the model file and writes the labels.
func (s *Supervisor) prepare(inst *instance) error {
	s.setState(inst, StatePreparing, "pulling host image")
	if err := s.cfg.Runtime.EnsureImage(inst.ctx, inst.variant.HostImage); err != nil {
		return fmt.Errorf("pulling host image: %w", err)
	}
	s.setState(inst, StatePreparing, "downloading model file")
	file, err := s.cfg.Files.Fetch(inst.ctx, inst.variant.File)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(inst.runDir, 0o755); err != nil {
		return err
	}
	labels := strings.Join(inst.model.Labels, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(inst.runDir, "labels.txt"), []byte(labels), 0o444); err != nil {
		return err
	}
	if inst.variant.Engine == EngineTensorRT {
		dir := filepath.Join(s.enginesDir(), inst.variant.File.SHA256)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// The host runs unprivileged and writes the engine it builds here.
		if err := os.Chown(dir, HostUID, HostGID); err != nil && !errors.Is(err, os.ErrPermission) {
			return err
		}
	}
	inst.mu.Lock()
	inst.modelFile = file
	inst.mu.Unlock()
	return nil
}

func (s *Supervisor) setState(inst *instance, st State, detail string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.setStateLocked(st, detail)
}

// runHost starts the host and waits until the instance must stop or the host
// ends. It reports whether the instance failed, and why.
func (s *Supervisor) runHost(inst *instance) (bool, string) {
	// Mark the host live before it starts, so an immediate "ready" counts.
	inst.mu.Lock()
	inst.hostRunning, inst.hostFailure = true, ""
	inst.lastStatus = s.clock.Now()
	inst.setStateLocked(StateStarting, "")
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.hostRunning = false
		inst.mu.Unlock()
	}()

	exits, err := s.cfg.Runtime.StartHost(inst.ctx, s.hostSpec(inst))
	if err != nil {
		if inst.ctx.Err() != nil {
			return false, ""
		}
		return true, "starting host: " + err.Error()
	}
	for {
		select {
		case <-inst.ctx.Done():
			return false, ""
		case exit := <-exits:
			if failure := inst.failure(); failure != "" {
				return true, failure
			}
			return true, fmt.Sprintf("host exited with status %d", exit.Code) + s.logTail(inst)
		case <-inst.wake:
			if failure := inst.failure(); failure != "" {
				return true, failure
			}
		}
	}
}

func (s *Supervisor) hostSpec(inst *instance) HostSpec {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	spec := HostSpec{
		InstanceID: inst.id, AppID: AppIDPrefix + inst.id, Image: inst.variant.HostImage,
		Engine: inst.variant.Engine, ModelID: inst.model.ID, VariantID: inst.variant.ID,
		FileSHA256: inst.variant.File.SHA256, ModelFile: inst.modelFile,
		LabelsFile: filepath.Join(inst.runDir, "labels.txt"), CameraNode: inst.node,
		CameraSource: inst.camera, LogPath: filepath.Join(inst.runDir, "host.log"),
	}
	if inst.variant.Engine == EngineTensorRT {
		spec.EngineCache = filepath.Join(s.enginesDir(), inst.variant.File.SHA256)
	}
	return spec
}

// handleStatus applies a model.status record from the live host.
func (s *Supervisor) handleStatus(inst *instance, st HostStatus) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.hostRunning {
		return // a host being replaced or removed no longer speaks for the instance
	}
	inst.lastStatus = s.clock.Now()
	inst.stats = st.Stats
	switch st.State {
	case HostFailed:
		inst.hostFailure = st.Reason
		if inst.hostFailure == "" {
			inst.hostFailure = "the model host reported a failure"
		}
		inst.poke()
	case HostBuildingEngine:
		inst.setStateLocked(StatePreparing, "building TensorRT engine")
	case HostReady:
		inst.setStateLocked(StateReady, "")
	}
}

// logTail returns the end of the host's output, for failure details.
func (s *Supervisor) logTail(inst *instance) string {
	f, err := os.Open(filepath.Join(inst.runDir, "host.log"))
	if err != nil {
		return ""
	}
	defer f.Close()
	const limit = 4096
	if info, err := f.Stat(); err == nil && info.Size() > limit {
		if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
			return ""
		}
	}
	tail, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil || len(tail) == 0 {
		return ""
	}
	return "\n" + strings.TrimSpace(string(tail))
}

// finish removes the host and everything the instance held.
func (s *Supervisor) finish(inst *instance, final State, detail string) {
	s.removeHost(inst)
	s.cfg.Cameras.Release(context.Background(), inst.id)
	if err := os.RemoveAll(inst.runDir); err != nil {
		s.log.Warn("removing a model run directory failed", zap.String("instance", inst.id), zap.Error(err))
	}
	s.forget(inst)
	inst.mu.Lock()
	if final == StateStopped && detail == "" {
		detail = inst.stopReason
	}
	inst.setStateLocked(final, detail)
	inst.mu.Unlock()
	inst.cancel()
	close(inst.done)
	s.log.Info("model instance ended", zap.String("instance", inst.id), zap.Stringer("state", final), zap.String("detail", detail))
}

func (s *Supervisor) removeHost(inst *instance) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	if err := s.cfg.Runtime.RemoveHost(ctx, inst.id); err != nil {
		s.log.Warn("removing a model host failed", zap.String("instance", inst.id), zap.Error(err))
	}
}
```

- [ ] **Step 5: Run the tests with the race detector**

Run: `go test -race ./go/internal/agent/models/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go/internal/agent/models/
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: start, prepare, run and stop model instances

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 7: models: watches and leases

**Files:**
- Create: `go/internal/agent/models/watch.go`
- Modify: `go/internal/agent/models/instance.go` (new fields; `infoLocked` and `setStateLocked` replaced; `broadcastLocked` added)
- Modify: `go/internal/agent/models/supervisor.go` (`leaseGrace`; `newInstance`, `Start` and `Stop`; new `Watch`, `Detach`, `detach`, `startGraceLocked`, `cancelGraceLocked`)
- Modify: `go/internal/agent/models/lifecycle.go` (`finish` and `handleStatus` replaced)
- Test: `go/internal/agent/models/watch_test.go` (package `models_test`)

**Interfaces:**
- Consumes: Task 6.
- Produces:
  - `WatchRequest{InstanceID, Label string; Filter Filter; AfterSequence uint64}`;
  - `WatchMessage{Status *InstanceInfo; Event *Event; Gap *Gap}`, where exactly one field is set;
  - `Watch{ID, InstanceID, Label string; C <-chan WatchMessage}` with `(*Watch) Final() *InstanceInfo`;
  - `(*Supervisor) Watch(WatchRequest) (*Watch, InstanceInfo, uint64, error)`;
  - `(*Supervisor) Detach(instanceID, watchID string)`;
  - `Stop(ctx, instanceID, watchID)` now supports `watchID`.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/models/watch_test.go`:

```go
package models_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

// next returns the watch's next message, failing after a second.
func next(t *testing.T, w *models.Watch) models.WatchMessage {
	t.Helper()
	select {
	case msg, ok := <-w.C:
		if !ok {
			t.Fatal("the watch closed")
		}
		return msg
	case <-time.After(time.Second):
		t.Fatal("no message within a second")
	}
	return models.WatchMessage{}
}

// drainToEnd reads until the watch closes and returns the final state it reports.
func drainToEnd(t *testing.T, w *models.Watch) *models.InstanceInfo {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-w.C:
			if !ok {
				return w.Final()
			}
		case <-deadline:
			t.Fatal("the watch never closed")
		}
	}
}

// advanceAlive moves the clock by d in five-second steps, with the host
// reporting ready at every step the way a healthy host heartbeats.
func (h *harness) advanceAlive(id string, d time.Duration) {
	for elapsed := time.Duration(0); elapsed < d; elapsed += 5 * time.Second {
		h.sup.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Status(models.HostReady))
		h.clock.Advance(5 * time.Second)
	}
}

func TestNeverWatchedInstanceExpires(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	h.advanceAlive(info.ID, 55*time.Second)
	time.Sleep(10 * time.Millisecond)
	if h.info(info.ID).State != models.StateReady {
		t.Fatal("the instance stopped before its grace period ended")
	}
	h.advanceAlive(info.ID, 10*time.Second)
	modelstest.Eventually(t, "the unwatched instance to stop", func() bool { return h.info(info.ID).State == 0 })
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the host was not removed")
	}
}

func TestLostWatchKeepsInstanceForGrace(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.advanceAlive(info.ID, 5*time.Minute) // watched: never expires
	h.sup.Detach(info.ID, w.ID)
	h.advanceAlive(info.ID, 30*time.Second)
	w2, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatalf("re-attach within the grace period: %v", err)
	}
	h.advanceAlive(info.ID, 5*time.Minute)
	if h.info(info.ID).State != models.StateReady {
		t.Fatal("a re-attached instance expired")
	}
	h.sup.Detach(info.ID, w2.ID)
	h.advanceAlive(info.ID, 65*time.Second)
	modelstest.Eventually(t, "expiry after the last watch is lost", func() bool { return h.info(info.ID).State == 0 })
}

func TestStopWithLastWatchStopsAtOnce(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w1, _, _, _ := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, Label: "front door"})
	w2, _, _, _ := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, Label: "porch"})
	still, err := h.sup.Stop(context.Background(), info.ID, w1.ID)
	if err != nil || still.State != models.StateReady || still.Watchers != 1 || !slices.Equal(still.WatchLabels, []string{"porch"}) {
		t.Fatalf("after the first watch stops: %+v, %v", still, err)
	}
	final, err := h.sup.Stop(context.Background(), info.ID, w2.ID)
	if err != nil || final.State != models.StateStopped || final.StateDetail != "stopped by its last watcher" {
		t.Fatalf("after the last watch stops: %+v, %v", final, err)
	}
	if _, err := h.sup.Stop(context.Background(), info.ID, "w-unknown"); !errors.Is(err, models.ErrUnknownInstance) {
		t.Fatalf("stop after removal: %v", err)
	}
}

func TestStoppedInstanceEndsWatchesWithFinalState(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.sup.Stop(context.Background(), info.ID, ""); err != nil {
		t.Fatal(err)
	}
	if final := drainToEnd(t, w); final == nil || final.State != models.StateStopped || final.StateDetail != "stopped" {
		t.Fatalf("final = %+v", final)
	}
}

func TestWatchSeesStateChanges(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	for {
		if msg := next(t, w); msg.Status != nil && msg.Status.State == models.StateReady {
			return
		}
	}
}

func TestWatchRejectsBadFilters(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	for _, f := range []models.Filter{{Classes: []string{"unicorn"}}, {Types: []string{"appeared"}}, {MinConfidence: 1.5}} {
		if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, Filter: f}); !errors.Is(err, models.ErrInvalidFilter) {
			t.Fatalf("filter %+v: %v", f, err)
		}
	}
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: "m-missing"}); !errors.Is(err, models.ErrUnknownInstance) {
		t.Fatalf("unknown instance: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/...`
Expected: FAIL to compile with `h.sup.Watch undefined`.

- [ ] **Step 3: Add the watch type**

`go/internal/agent/models/watch.go`:

```go
package models

import (
	"fmt"
	"slices"
)

// watchBuffer holds a full ring replay plus room for statuses.
const watchBuffer = 128

// WatchRequest opens a watch on a live instance.
type WatchRequest struct {
	InstanceID    string
	Label         string // what the watcher calls this watch, e.g. "front door"
	Filter        Filter
	AfterSequence uint64 // replay retained events after this; 0 replays nothing
}

// WatchMessage is one item on a watch; exactly one field is set.
type WatchMessage struct {
	Status *InstanceInfo
	Event  *Event
	Gap    *Gap
}

// Watch is one client's subscription to an instance, and holds the instance
// alive. C closes when the watch ends. Once it has closed, Final returns the
// instance's end state if the instance itself ended.
type Watch struct {
	ID         string
	InstanceID string
	Label      string
	C          <-chan WatchMessage

	// Guarded by the instance's mutex.
	c      chan WatchMessage
	filter Filter
	gap    *Gap // events dropped because C was full, not yet reported
	final  *InstanceInfo
	closed bool
}

// Final returns the instance's end state after C has closed, or nil when only
// the watch ended. Read it only after observing C closed.
func (w *Watch) Final() *InstanceInfo { return w.final }

// offer delivers msg without blocking. A full buffer drops the message; for
// events, the dropped range is reported as a Gap ahead of the next event.
// Caller holds the instance's mutex.
func (w *Watch) offer(msg WatchMessage) {
	if w.closed {
		return
	}
	if msg.Event != nil && w.gap != nil {
		select {
		case w.c <- WatchMessage{Gap: w.gap}:
			w.gap = nil
		default:
			w.gap.LastMissing = msg.Event.Sequence
			return
		}
	}
	select {
	case w.c <- msg:
	default:
		if msg.Event != nil {
			if w.gap == nil {
				w.gap = &Gap{FirstMissing: msg.Event.Sequence}
			}
			w.gap.LastMissing = msg.Event.Sequence
		}
		// A dropped status is superseded by the next one.
	}
}

// end closes the watch. final is the instance's end state, or nil when only
// the watch ended. Caller holds the instance's mutex.
func (w *Watch) end(final *InstanceInfo) {
	if w.closed {
		return
	}
	w.closed = true
	w.final = final
	close(w.c)
}

// checkFilter rejects classes the model cannot report, unknown event types
// and confidences outside 0..1.
func (m Model) checkFilter(f Filter) error {
	for _, c := range f.Classes {
		if !slices.Contains(m.Labels, c) {
			return fmt.Errorf("%w: %s does not report %q", ErrInvalidFilter, m.ID, c)
		}
	}
	for _, t := range f.Types {
		if t != EventEntered && t != EventLeft {
			return fmt.Errorf("%w: unknown event type %q", ErrInvalidFilter, t)
		}
	}
	if f.MinConfidence < 0 || f.MinConfidence > 1 {
		return fmt.Errorf("%w: min confidence %v is outside 0..1", ErrInvalidFilter, f.MinConfidence)
	}
	return nil
}
```

- [ ] **Step 4: Give instances watches**

In `go/internal/agent/models/instance.go`:

1. Add `"sort"` to the imports.
2. Add these fields after `lastStatus time.Time` in `instance`:

```go
	watches  map[string]*Watch
	grace    Timer  // pending lease expiry; nil while watched
	graceGen uint64 // invalidates a grace callback that fires after being replaced
```

3. Replace `infoLocked` and `setStateLocked` with:

```go
// infoLocked snapshots the instance. Caller holds mu.
func (inst *instance) infoLocked() InstanceInfo {
	labels := make([]string, 0, len(inst.watches))
	for _, w := range inst.watches {
		if w.Label != "" {
			labels = append(labels, w.Label)
		}
	}
	sort.Strings(labels)
	return InstanceInfo{
		ID: inst.id, ModelID: inst.model.ID, VariantID: inst.variant.ID, Engine: inst.variant.Engine,
		CameraSourceID: inst.camera, State: inst.state, StateDetail: inst.detail,
		Watchers: len(inst.watches), WatchLabels: labels,
		StartedAt: inst.started, Stats: inst.stats, FileSHA256: inst.variant.File.SHA256,
	}
}

// setStateLocked records a state change, tells every watch, and reports
// whether anything changed. Caller holds mu.
func (inst *instance) setStateLocked(s State, detail string) bool {
	if inst.state == s && inst.detail == detail {
		return false
	}
	inst.state, inst.detail = s, detail
	inst.broadcastLocked()
	return true
}

// broadcastLocked sends the current snapshot to every watch. Caller holds mu.
func (inst *instance) broadcastLocked() {
	info := inst.infoLocked()
	for _, w := range inst.watches {
		snapshot := info
		w.offer(WatchMessage{Status: &snapshot})
	}
}
```

- [ ] **Step 5: Add watches, leases and detaching to the supervisor**

In `go/internal/agent/models/supervisor.go`:

1. Add `leaseGrace = 60 * time.Second` to the constant block.
2. In `newInstance`, add `watches: map[string]*Watch{},` to the returned `&instance{…}` literal.
3. In `Start`, replace:

```go
	inst.mu.Lock()
	inst.node = node
	info := inst.infoLocked()
```

with:

```go
	inst.mu.Lock()
	inst.node = node
	s.startGraceLocked(inst) // no watch yet: stop unless one attaches
	info := inst.infoLocked()
```

4. Replace `Stop` with:

```go
// Stop with an empty watchID stops the instance outright. With a watchID it
// ends that watch, and stops the instance if it was the last. When the
// instance stops, Stop waits for its removal and returns the final state.
func (s *Supervisor) Stop(ctx context.Context, instanceID, watchID string) (InstanceInfo, error) {
	if watchID != "" {
		inst, stopping, err := s.detach(instanceID, watchID, true)
		if err != nil {
			return InstanceInfo{}, err
		}
		if !stopping {
			return inst.info(), nil
		}
		return s.awaitRemoval(ctx, inst)
	}
	inst, err := s.lookup(instanceID)
	if err != nil {
		return InstanceInfo{}, err
	}
	inst.mu.Lock()
	inst.requestStopLocked("stopped")
	inst.mu.Unlock()
	return s.awaitRemoval(ctx, inst)
}
```

5. Append:

```go
// Watch attaches a watch to a live instance. It returns the watch, the
// instance snapshot, and the sequence of the instance's newest event, which a
// client passes back as AfterSequence when it re-attaches.
func (s *Supervisor) Watch(req WatchRequest) (*Watch, InstanceInfo, uint64, error) {
	inst, err := s.lookup(req.InstanceID)
	if err != nil {
		return nil, InstanceInfo{}, 0, err
	}
	if err := inst.model.checkFilter(req.Filter); err != nil {
		return nil, InstanceInfo{}, 0, err
	}
	ch := make(chan WatchMessage, watchBuffer)
	w := &Watch{ID: newID("w-"), InstanceID: inst.id, Label: req.Label, C: ch, c: ch, filter: req.Filter}

	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.ctx.Err() != nil {
		return nil, InstanceInfo{}, 0, fmt.Errorf("%w %q", ErrUnknownInstance, req.InstanceID)
	}
	inst.watches[w.ID] = w
	s.cancelGraceLocked(inst)
	return w, inst.infoLocked(), 0, nil
}

// Detach ends a watch whose client went away. The instance stays for
// leaseGrace so the client can re-attach. Unknown ids are ignored.
func (s *Supervisor) Detach(instanceID, watchID string) {
	_, _, _ = s.detach(instanceID, watchID, false)
}

// detach ends one watch. When it was the last, an explicit detach stops the
// instance at once and a lost client starts the grace period. It reports
// whether the instance is now stopping.
func (s *Supervisor) detach(instanceID, watchID string, explicit bool) (*instance, bool, error) {
	inst, err := s.lookup(instanceID)
	if err != nil {
		return nil, false, err
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	w := inst.watches[watchID]
	if w == nil {
		return inst, false, fmt.Errorf("%w %q", ErrUnknownWatch, watchID)
	}
	delete(inst.watches, watchID)
	w.end(nil)
	if len(inst.watches) > 0 {
		return inst, false, nil
	}
	if explicit {
		inst.requestStopLocked("stopped by its last watcher")
		return inst, true, nil
	}
	s.startGraceLocked(inst)
	return inst, false, nil
}

// startGraceLocked stops the instance unless a watch attaches within
// leaseGrace. Caller holds inst.mu.
func (s *Supervisor) startGraceLocked(inst *instance) {
	s.cancelGraceLocked(inst)
	gen := inst.graceGen
	inst.grace = s.clock.AfterFunc(leaseGrace, func() {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		if inst.graceGen == gen && len(inst.watches) == 0 {
			inst.requestStopLocked("no watchers for 60 s")
		}
	})
}

// cancelGraceLocked drops a pending lease expiry. Caller holds inst.mu.
func (s *Supervisor) cancelGraceLocked(inst *instance) {
	inst.graceGen++
	if inst.grace != nil {
		inst.grace.Stop()
		inst.grace = nil
	}
}
```

- [ ] **Step 6: End watches when the instance ends, and forward heartbeats**

In `go/internal/agent/models/lifecycle.go`, replace `finish` with:

```go
// finish removes the host and everything the instance held, then ends every
// watch with the final state.
func (s *Supervisor) finish(inst *instance, final State, detail string) {
	s.removeHost(inst)
	s.cfg.Cameras.Release(context.Background(), inst.id)
	if err := os.RemoveAll(inst.runDir); err != nil {
		s.log.Warn("removing a model run directory failed", zap.String("instance", inst.id), zap.Error(err))
	}
	s.forget(inst)
	inst.mu.Lock()
	s.cancelGraceLocked(inst)
	if final == StateStopped && detail == "" {
		detail = inst.stopReason
	}
	// Set directly instead of broadcasting: each watch gets the final state
	// exactly once, through Final after its channel closes.
	inst.state, inst.detail = final, detail
	end := inst.infoLocked()
	for id, w := range inst.watches {
		snapshot := end
		w.end(&snapshot)
		delete(inst.watches, id)
	}
	inst.mu.Unlock()
	inst.cancel()
	close(inst.done)
	s.log.Info("model instance ended", zap.String("instance", inst.id), zap.Stringer("state", final), zap.String("detail", detail))
}
```

Replace `handleStatus` with:

```go
// handleStatus applies a model.status record from the live host.
func (s *Supervisor) handleStatus(inst *instance, st HostStatus) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.hostRunning {
		return // a host being replaced or removed no longer speaks for the instance
	}
	inst.lastStatus = s.clock.Now()
	inst.stats = st.Stats
	changed := false
	switch st.State {
	case HostFailed:
		inst.hostFailure = st.Reason
		if inst.hostFailure == "" {
			inst.hostFailure = "the model host reported a failure"
		}
		inst.poke()
		return
	case HostBuildingEngine:
		changed = inst.setStateLocked(StatePreparing, "building TensorRT engine")
	case HostReady:
		changed = inst.setStateLocked(StateReady, "")
	}
	if !changed {
		inst.broadcastLocked() // a heartbeat carries fresh stats
	}
}
```

- [ ] **Step 7: Run the tests**

Run: `go test -race ./go/internal/agent/models/...`
Expected: PASS, including the Task 6 tests.

- [ ] **Step 8: Commit**

```bash
git add go/internal/agent/models/
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: hold instances with watches and a 60 s lease grace

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 8: models: detection events, replay and gaps

**Files:**
- Modify: `go/internal/agent/models/instance.go` (add `ring *ring`)
- Modify: `go/internal/agent/models/supervisor.go` (`ringSize`; `newInstance`, `Watch` and `PublishApplicationRecord`; new `handleDetection`)
- Test: `go/internal/agent/models/events_supervisor_test.go` (package `models_test`)

**Interfaces:**
- Consumes: Tasks 4 and 7.
- Produces: detections reach watches. `Watch` replays events after `AfterSequence` and returns the newest sequence.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/models/events_supervisor_test.go`:

```go
package models_test

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

// nextEvent skips status messages and returns the next event.
func nextEvent(t *testing.T, w *models.Watch) models.Event {
	t.Helper()
	for {
		if msg := next(t, w); msg.Event != nil {
			return *msg.Event
		}
	}
}

func TestWatchGetsFilteredNumberedEvents(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, last, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID,
		Filter: models.Filter{Classes: []string{"person"}, MinConfidence: 0.5}})
	if err != nil || last != 0 {
		t.Fatalf("watch: last=%d err=%v", last, err)
	}
	app := models.AppIDPrefix + info.ID
	h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, 1))
	h.sup.PublishApplicationRecord(app, modelstest.Entered("car", 0.9, 2))
	h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.3, 3))
	h.sup.PublishApplicationRecord(app, modelstest.Left("person", 0.9, 1))
	first, second := nextEvent(t, w), nextEvent(t, w)
	if first.Sequence != 1 || first.Type != models.EventEntered || first.Class != "person" || first.SampleID != 1 {
		t.Fatalf("first = %+v", first)
	}
	if second.Sequence != 4 || second.Type != models.EventLeft {
		t.Fatalf("second = %+v; the car and the unsure person must be filtered out", second)
	}
}

func TestReattachReplaysMissedEvents(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	app := models.AppIDPrefix + info.ID
	for i := 1; i <= 105; i++ {
		h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, i))
	}
	w, _, last, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, AfterSequence: 3})
	if err != nil || last != 105 {
		t.Fatalf("watch: last=%d err=%v", last, err)
	}
	if msg := next(t, w); msg.Gap == nil || *msg.Gap != (models.Gap{FirstMissing: 4, LastMissing: 5}) {
		t.Fatalf("first message = %+v, want the evicted range 4-5", msg)
	}
	for want := uint64(6); want <= 105; want++ {
		if e := nextEvent(t, w); e.Sequence != want {
			t.Fatalf("replayed %d, want %d", e.Sequence, want)
		}
	}
}

func TestSlowWatchNeverBlocksIntake(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	app := models.AppIDPrefix + info.ID
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 300; i++ {
			h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, i))
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("record intake blocked on a watch nobody reads")
	}
	var lastSeen uint64
	for i := 0; i < 128; i++ { // drain what fit in the buffer
		if msg := next(t, w); msg.Event != nil {
			lastSeen = msg.Event.Sequence
		}
	}
	h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, 301))
	msg := next(t, w)
	if msg.Gap == nil || msg.Gap.FirstMissing != lastSeen+1 || msg.Gap.LastMissing != 300 {
		t.Fatalf("after draining: %+v, want a gap from %d to 300", msg, lastSeen+1)
	}
	if e := nextEvent(t, w); e.Sequence != 301 {
		t.Fatalf("next event %d, want 301", e.Sequence)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/... -run 'TestWatchGetsFilteredNumberedEvents|TestReattachReplaysMissedEvents|TestSlowWatchNeverBlocksIntake'`
Expected: FAIL. No event ever arrives ("no message within a second").

- [ ] **Step 3: Implement event intake and replay**

In `go/internal/agent/models/instance.go`, add after the `graceGen` field:

```go
	ring *ring // the latest detections, numbered
```

In `go/internal/agent/models/supervisor.go`:

1. Add `ringSize = 100` to the constant block.
2. In `newInstance`, add `ring: newRing(ringSize),` to the `&instance{…}` literal.
3. In `Watch`, replace:

```go
	inst.watches[w.ID] = w
	s.cancelGraceLocked(inst)
	return w, inst.infoLocked(), 0, nil
```

with:

```go
	inst.watches[w.ID] = w
	s.cancelGraceLocked(inst)
	if req.AfterSequence > 0 {
		events, gap := inst.ring.since(req.AfterSequence)
		if gap != nil {
			w.offer(WatchMessage{Gap: gap})
		}
		for _, e := range events {
			if w.filter.Match(e) {
				event := e
				w.offer(WatchMessage{Event: &event})
			}
		}
	}
	return w, inst.infoLocked(), inst.ring.last, nil
```

4. In `PublishApplicationRecord`, replace the final `if rec.Name == RecordStatus { … }` block with:

```go
	switch rec.Name {
	case RecordStatus:
		st, err := parseStatus(rec)
		if err != nil {
			s.log.Debug("ignoring a malformed model status", zap.String("instance", id), zap.Error(err))
			return
		}
		s.handleStatus(inst, st)
	case RecordEntered, RecordLeft:
		s.handleDetection(inst, rec)
	}
```

5. Append:

```go
// handleDetection numbers a detection and offers it to every watch whose
// filter it passes.
func (s *Supervisor) handleDetection(inst *instance, rec data.ApplicationRecord) {
	e, err := parseDetection(rec, s.clock.Now())
	if err != nil {
		s.log.Debug("ignoring a malformed model detection", zap.String("instance", inst.id), zap.Error(err))
		return
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.ctx.Err() != nil {
		return
	}
	e = inst.ring.append(e)
	for _, w := range inst.watches {
		if w.filter.Match(e) {
			event := e
			w.offer(WatchMessage{Event: &event})
		}
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./go/internal/agent/models/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/models/
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: deliver numbered detections to watches, with replay and gaps

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 9: models: restarts, stalls, engine builds and orphan cleanup

**Files:**
- Modify: `go/internal/agent/models/instance.go` (add `building`, `holdsBuild`)
- Modify: `go/internal/agent/models/supervisor.go` (constants; `build` slot on `Supervisor`)
- Modify: `go/internal/agent/models/lifecycle.go` (replace `run`, `runHost`, `handleStatus`, `finish`; extend `prepare`; add `checkHost`, `sleep`, `armStallLocked`, `releaseBuildLocked`, `CleanupOrphans`)
- Test: `go/internal/agent/models/failures_test.go` (package `models_test`)

**Interfaces:**
- Consumes: Tasks 6–8.
- Produces: `(*Supervisor) CleanupOrphans(ctx) error`. Behaviour:
  - crashes and stalls restart with 2/8/30 s backoff, and the fourth failure is final;
  - a host-reported failure is final;
  - one engine build runs at a time, with a 15 min timeout;
  - statuses from a host being replaced are ignored.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/models/failures_test.go`:

```go
package models_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

func TestCrashedHostRestartsAfterBackoff(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	h.runtime.Exit(info.ID, 1)
	modelstest.Eventually(t, "restarting", func() bool { return h.info(info.ID).State == models.StateRestarting })
	if detail := h.info(info.ID).StateDetail; !strings.Contains(detail, "status 1") {
		t.Fatalf("detail = %q", detail)
	}
	h.clock.Advance(time.Second)
	time.Sleep(10 * time.Millisecond)
	if h.runtime.Starts(info.ID) != 1 {
		t.Fatal("restarted before the 2 s backoff")
	}
	modelstest.AdvanceUntil(t, h.clock, 500*time.Millisecond, "the restart", func() bool { return h.runtime.Starts(info.ID) == 2 })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "ready again", func() bool { return h.info(info.ID).State == models.StateReady })
}

func TestFourthFailureIsFinal(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	for start := 1; start <= 3; start++ {
		h.runtime.Exit(info.ID, 1)
		modelstest.AdvanceUntil(t, h.clock, time.Second, "a restart", func() bool { return h.runtime.Starts(info.ID) == start+1 })
	}
	h.runtime.Exit(info.ID, 1)
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || !strings.Contains(final.StateDetail, "host failed 4 times") {
		t.Fatalf("final = %+v", final)
	}
}

func TestReportedFailureIsFinal(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Failed("no frames from camera"))
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || final.StateDetail != "no frames from camera" {
		t.Fatalf("final = %+v", final)
	}
	if n := h.runtime.Starts(info.ID); n != 1 {
		t.Fatalf("a reported failure was retried: %d starts", n)
	}
}

func TestSilentHostIsRestarted(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })
	modelstest.AdvanceUntil(t, h.clock, time.Second, "the silent host to be replaced", func() bool { return h.runtime.Starts(info.ID) == 2 })
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the silent host was not removed before its restart")
	}
}

func TestStaleStatusDuringRestartIsIgnored(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	h.runtime.Exit(info.ID, 1)
	modelstest.Eventually(t, "restarting", func() bool { return h.info(info.ID).State == models.StateRestarting })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	time.Sleep(20 * time.Millisecond)
	if st := h.info(info.ID).State; st != models.StateRestarting {
		t.Fatalf("a status from the dead host moved the instance to %v", st)
	}
}

func TestEngineBuildsRunOneAtATime(t *testing.T) {
	h := newHarness(t, models.EngineTensorRT)
	ctx := context.Background()
	first, _, err := h.sup.Start(ctx, "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the first host", func() bool { return h.runtime.Running(first.ID) })
	if h.runtime.LastSpec().EngineCache == "" {
		t.Fatal("a TensorRT host got no engine cache")
	}
	second, _, err := h.sup.Start(ctx, "coco-detector", garage)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the second to wait", func() bool {
		return h.info(second.ID).StateDetail == "waiting for another engine build"
	})
	h.sup.PublishApplicationRecord(models.AppIDPrefix+first.ID, modelstest.Status(models.HostBuildingEngine))
	modelstest.Eventually(t, "building", func() bool { return h.info(first.ID).StateDetail == "building TensorRT engine" })
	if h.runtime.Running(second.ID) {
		t.Fatal("two engine builds ran at once")
	}
	h.sup.PublishApplicationRecord(models.AppIDPrefix+first.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "the second host to start", func() bool { return h.runtime.Running(second.ID) })
}

func TestEngineBuildTimesOut(t *testing.T) {
	h := newHarness(t, models.EngineTensorRT)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host", func() bool { return h.runtime.Running(info.ID) })
	app := models.AppIDPrefix + info.ID
	for i := 0; i < 200 && h.info(info.ID).State != 0; i++ {
		h.sup.PublishApplicationRecord(app, modelstest.Status(models.HostBuildingEngine))
		h.clock.Advance(5 * time.Second)
		time.Sleep(time.Millisecond)
	}
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || !strings.Contains(final.StateDetail, "engine build took longer than 15 min") {
		t.Fatalf("final = %+v", final)
	}
}

func TestCleanupOrphansRemovesLeftovers(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	h.runtime.AddLeftover("m-0ld00001")
	stale := filepath.Join(h.root, "run", "m-0ld00001")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.CleanupOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.runtime.Removed(), "m-0ld00001") {
		t.Fatal("the leftover host was not removed")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the leftover run directory remains: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/... -run 'TestCrashed|TestFourth|TestReportedFailure|TestSilent|TestStale|TestEngineBuild|TestCleanupOrphans'`
Expected: FAIL. `CleanupOrphans` is undefined, and crashes end the instance instead of restarting it.

- [ ] **Step 3: Add the new state**

In `go/internal/agent/models/instance.go`, add after the `ring` field:

```go
	building   time.Time // first building_engine report from the current host; zero otherwise
	holdsBuild bool      // this instance holds the device's engine build slot
```

In `go/internal/agent/models/supervisor.go`:

1. Extend the constant block with:

```go
	stallTimeout       = 15 * time.Second
	engineBuildTimeout = 15 * time.Minute
	maxRestarts        = 3
```

and add below it:

```go
// restartBackoff is how long to wait before the first, second and third restart.
var restartBackoff = [maxRestarts]time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}
```

2. Add a field to `Supervisor`, above `mu`:

```go
	build chan struct{} // the device's single engine build slot
```

3. In `NewSupervisor`, add `build: make(chan struct{}, 1),` to the returned literal.

- [ ] **Step 4: Replace the run loop**

In `go/internal/agent/models/lifecycle.go`:

1. Add `"time"` to the imports.
2. In `prepare`, add this directly before the final `inst.mu.Lock()` / `inst.modelFile = file` block:

```go
	if s.needsEngineBuild(inst.variant) {
		select {
		case s.build <- struct{}{}:
		default:
			s.setState(inst, StatePreparing, "waiting for another engine build")
			select {
			case s.build <- struct{}{}:
			case <-inst.ctx.Done():
				return inst.ctx.Err()
			}
		}
		inst.mu.Lock()
		inst.holdsBuild = true
		inst.mu.Unlock()
	}
```

3. Replace `run` and `runHost` with:

```go
type hostOutcome int

const (
	hostStopped     hostOutcome = iota // the instance was asked to stop
	hostFailedFinal                    // reported failure or expired engine build
	hostCrashed                        // exited or went silent; worth a restart
)

const engineBuildTimeoutReason = "engine build took longer than 15 min"

// run takes an instance from preparation to removal, restarting a crashed or
// silent host up to maxRestarts times.
func (s *Supervisor) run(inst *instance) {
	final, detail := StateStopped, ""
	defer func() { s.finish(inst, final, detail) }()
	if err := s.prepare(inst); err != nil {
		if inst.ctx.Err() == nil {
			final, detail = StateFailed, err.Error()
		}
		return
	}
	for attempt := 0; ; attempt++ {
		outcome, reason := s.runHost(inst)
		switch outcome {
		case hostStopped:
			return
		case hostFailedFinal:
			final, detail = StateFailed, reason
			return
		}
		if attempt == maxRestarts {
			final, detail = StateFailed, fmt.Sprintf("host failed %d times; last: %s", attempt+1, reason)
			return
		}
		s.setState(inst, StateRestarting, reason)
		s.removeHost(inst)
		if !s.sleep(inst, restartBackoff[attempt]) {
			return
		}
	}
}

// runHost starts one host and waits until the instance must stop, the host
// reports a failure, or it crashes or goes silent.
func (s *Supervisor) runHost(inst *instance) (hostOutcome, string) {
	// Mark the host live before it starts, so an immediate "ready" counts.
	inst.mu.Lock()
	inst.hostRunning, inst.hostFailure, inst.building = true, "", time.Time{}
	inst.lastStatus = s.clock.Now()
	inst.setStateLocked(StateStarting, "")
	s.armStallLocked(inst)
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.hostRunning = false
		inst.mu.Unlock()
	}()

	exits, err := s.cfg.Runtime.StartHost(inst.ctx, s.hostSpec(inst))
	if err != nil {
		if inst.ctx.Err() != nil {
			return hostStopped, ""
		}
		return hostCrashed, "starting host: " + err.Error()
	}
	for {
		select {
		case <-inst.ctx.Done():
			return hostStopped, ""
		case exit := <-exits:
			if failure := inst.failure(); failure != "" {
				return hostFailedFinal, failure
			}
			return hostCrashed, fmt.Sprintf("host exited with status %d", exit.Code) + s.logTail(inst)
		case <-inst.wake:
			outcome, reason, done := s.checkHost(inst)
			if !done {
				continue
			}
			if reason == engineBuildTimeoutReason {
				reason += s.logTail(inst)
			}
			return outcome, reason
		}
	}
}

// checkHost looks for a reported failure, an expired engine build, or a
// silent host.
func (s *Supervisor) checkHost(inst *instance) (hostOutcome, string, bool) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	now := s.clock.Now()
	switch {
	case inst.hostFailure != "":
		return hostFailedFinal, inst.hostFailure, true
	case !inst.building.IsZero() && inst.state != StateReady && now.Sub(inst.building) >= engineBuildTimeout:
		return hostFailedFinal, engineBuildTimeoutReason, true
	case now.Sub(inst.lastStatus) >= stallTimeout:
		return hostCrashed, "host sent no status for 15 s", true
	}
	return 0, "", false
}

// sleep waits d on the supervisor's clock; false means the instance was
// stopped first.
func (s *Supervisor) sleep(inst *instance, d time.Duration) bool {
	fired := make(chan struct{})
	t := s.clock.AfterFunc(d, func() { close(fired) })
	defer t.Stop()
	select {
	case <-fired:
		return true
	case <-inst.ctx.Done():
		return false
	}
}

// armStallLocked wakes the run loop once the host has been silent for
// stallTimeout; every status re-arms it. Caller holds inst.mu.
func (s *Supervisor) armStallLocked(inst *instance) {
	s.clock.AfterFunc(stallTimeout, inst.poke)
}

// releaseBuildLocked frees the engine build slot if this instance holds it.
// Caller holds inst.mu.
func (s *Supervisor) releaseBuildLocked(inst *instance) {
	if inst.holdsBuild {
		inst.holdsBuild = false
		<-s.build
	}
}
```

4. Replace `handleStatus` with:

```go
// handleStatus applies a model.status record from the live host.
func (s *Supervisor) handleStatus(inst *instance, st HostStatus) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.hostRunning {
		return // a host being replaced or removed no longer speaks for the instance
	}
	now := s.clock.Now()
	inst.lastStatus = now
	inst.stats = st.Stats
	s.armStallLocked(inst)
	changed := false
	switch st.State {
	case HostFailed:
		inst.hostFailure = st.Reason
		if inst.hostFailure == "" {
			inst.hostFailure = "the model host reported a failure"
		}
		inst.poke()
		return
	case HostBuildingEngine:
		if inst.building.IsZero() {
			inst.building = now
			s.clock.AfterFunc(engineBuildTimeout, inst.poke)
		}
		changed = inst.setStateLocked(StatePreparing, "building TensorRT engine")
	case HostReady:
		s.releaseBuildLocked(inst)
		changed = inst.setStateLocked(StateReady, "")
	}
	if !changed {
		inst.broadcastLocked() // a heartbeat carries fresh stats
	}
}
```

5. In `finish`, add `s.releaseBuildLocked(inst)` on the line after `s.cancelGraceLocked(inst)`.

6. Append:

```go
// CleanupOrphans removes model hosts and run directories that a previous
// agent process left behind; leases do not survive a restart. Call it before
// serving.
func (s *Supervisor) CleanupOrphans(ctx context.Context) error {
	ids, err := s.cfg.Runtime.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("listing model hosts: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if err := s.cfg.Runtime.RemoveHost(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("removing model host %s: %w", id, err))
		}
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.Root, "run")); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
```

- [ ] **Step 5: Run the whole package with the race detector, twice**

Run: `go test -race -count=2 ./go/internal/agent/models/...`
Expected: PASS on both runs.

- [ ] **Step 6: Commit**

```bash
git add go/internal/agent/models/
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: restart crashed hosts, serialize engine builds, clean up orphans

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 10: Pinned two-plane demand in `VideoService`

Two-plane nodes exist only while something demands them, and the minute-long container sync replaces that demand wholesale. Model hosts carry no app labels, so they need a pin that the sync cannot clear.

**Files:**
- Modify: `go/internal/agent/services/video_service_two_plane.go` (`twoPlaneState` at ~lines 364-380; `SetTwoPlaneContainerConsumers` at ~lines 78-98)
- Create: `go/internal/agent/services/video_service_two_plane_pin.go`
- Test: `go/internal/agent/services/video_service_two_plane_pin_test.go`

**Interfaces:**
- Produces: `(*VideoService) AcquireTwoPlaneNode(ctx, owner, sourceID string) (string, error)` and `(*VideoService) ReleaseTwoPlaneNode(ctx, owner string)`.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/services/video_service_two_plane_pin_test.go`:

```go
package services

import (
	"context"
	"testing"
)

func TestTwoPlanePinSurvivesContainerSync(t *testing.T) {
	video, loop, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	node, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0")
	if err != nil {
		t.Fatal(err)
	}
	if path, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok || path != node {
		t.Fatalf("node %q is not the published one (%q, %v)", node, path, ok)
	}
	// The minute sweep finds no camera-entitled app containers.
	video.SetTwoPlaneContainerConsumers(ctx, nil)
	if _, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok {
		t.Fatal("a container sync tore down the node a model host holds")
	}
	video.ReleaseTwoPlaneNode(ctx, "m-1")
	waitUntilTwoPlane(t, "the node to go once the pin is released", func() bool {
		_, ok := video.TwoPlaneNodePath("v4l2:/dev/video0")
		return !ok
	})
	if got := loop.auxCreatedCount(); got != 1 {
		t.Fatalf("created %d nodes, want 1", got)
	}
}

func TestTwoPlaneAcquireUnknownSourceLeavesNoPin(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	if _, err := video.AcquireTwoPlaneNode(context.Background(), "m-1", "v4l2:/dev/video7"); err == nil {
		t.Fatal("acquired a node for a camera that does not exist")
	}
	video.twoPlaneMu.Lock()
	demand, pins := video.twoPlaneDemand, len(video.twoPlanePinned)
	video.twoPlaneMu.Unlock()
	if demand || pins != 0 {
		t.Fatalf("demand=%v pins=%d after a failed acquire", demand, pins)
	}
}

func TestTwoPlaneContainerDemandSurvivesPinRelease(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	video.SetTwoPlaneContainerConsumers(ctx, []string{"app-a"})
	if _, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0"); err != nil {
		t.Fatal(err)
	}
	video.ReleaseTwoPlaneNode(ctx, "m-1")
	if _, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok {
		t.Fatal("releasing a pin stopped the node an app container still uses")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/services/ -run 'TestTwoPlanePin|TestTwoPlaneAcquire|TestTwoPlaneContainerDemand'`
Expected: FAIL to compile with `video.AcquireTwoPlaneNode undefined`.

- [ ] **Step 3: Split demand into containers and pins**

In `go/internal/agent/services/video_service_two_plane.go`, add these fields to `twoPlaneState` next to `twoPlaneDemand`:

```go
	// twoPlaneContainerDemand is the container sync's view: some running app
	// container is entitled to the two-plane path.
	twoPlaneContainerDemand bool
	// twoPlanePinned holds agent-managed owners (model hosts) that need the
	// path whatever the container sync says. twoPlaneDemand is
	// twoPlaneContainerDemand || len(twoPlanePinned) > 0.
	twoPlanePinned map[string]bool
```

In `SetTwoPlaneContainerConsumers`, replace the line `s.twoPlaneDemand = len(containerIDs) > 0` with:

```go
	s.twoPlaneContainerDemand = len(containerIDs) > 0
	s.twoPlaneDemand = s.twoPlaneContainerDemand || len(s.twoPlanePinned) > 0
```

Create `go/internal/agent/services/video_service_two_plane_pin.go`:

```go
package services

import (
	"context"
	"fmt"
)

// AcquireTwoPlaneNode keeps the two-plane data path running for owner, an
// agent-managed model host that carries no app labels and so never counts in
// the container sync, and returns the node carrying sourceID's frames. The
// pin outlasts every container sync until ReleaseTwoPlaneNode. A source with
// no node (unknown, or refused because its stream cannot carry frame
// identity) is an error and leaves no pin behind.
func (s *VideoService) AcquireTwoPlaneNode(ctx context.Context, owner, sourceID string) (string, error) {
	s.twoPlaneMu.Lock()
	if s.twoPlanePinned == nil {
		s.twoPlanePinned = map[string]bool{}
	}
	s.twoPlanePinned[owner] = true
	s.twoPlaneDemand = true
	s.twoPlaneMu.Unlock()

	s.ensureTwoPlaneForLocalCameras(ctx)
	if node, ok := s.TwoPlaneNodePath(sourceID); ok {
		return node, nil
	}
	s.ReleaseTwoPlaneNode(ctx, owner)
	return "", fmt.Errorf("no two-plane node for %s: the camera is missing, or its stream cannot carry frame identity", sourceID)
}

// ReleaseTwoPlaneNode drops owner's pin, and stops the data path when nothing
// else needs it.
func (s *VideoService) ReleaseTwoPlaneNode(_ context.Context, owner string) {
	s.twoPlaneMu.Lock()
	delete(s.twoPlanePinned, owner)
	s.twoPlaneDemand = s.twoPlaneContainerDemand || len(s.twoPlanePinned) > 0
	demand := s.twoPlaneDemand
	s.twoPlaneMu.Unlock()
	if !demand {
		s.stopAllTwoPlane()
	}
}
```

- [ ] **Step 4: Run the new tests and every two-plane test**

Run: `go test -race ./go/internal/agent/services/ -run 'TwoPlane'`
Expected: PASS, including the existing `TestTwoPlaneRefusedSourceIsNotRecreated`.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/video_service_two_plane.go go/internal/agent/services/video_service_two_plane_pin.go go/internal/agent/services/video_service_two_plane_pin_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
video: let agent-managed owners pin two-plane demand

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 11: A record sink on the app data socket

**Files:**
- Modify: `go/internal/agent/services/app_data_socket.go` (`AppDataSocketManager` struct ~line 89; `serveConn` ~line 502)
- Test: `go/internal/agent/services/app_data_socket_sink_test.go`

**Interfaces:**
- Produces: `(*AppDataSocketManager) SetRecordSink(func(appID string, rec data.ApplicationRecord))`. The sink sees every record that passes validation, after capture, whether or not capture succeeded.

- [ ] **Step 1: Write the failing test**

`go/internal/agent/services/app_data_socket_sink_test.go`:

```go
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func TestAppDataSocketForwardsAcceptedRecordsToSink(t *testing.T) {
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "wendy-sink-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	oldRoot := AppDataSocketRootPath
	AppDataSocketRootPath = socketRoot
	defer func() { AppDataSocketRootPath = oldRoot }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := NewAppDataSocketManager(ctx, nil, capture)
	manager.peerCred = func(net.Conn) (peerCredentials, error) { return peerCredentials{UID: 0, PID: 4242}, nil }
	manager.cgroupOfPID = func(int32) (string, error) {
		return fmt.Sprintf("0::/system.slice/%s-sh.wendy.model.m-1.scope\n", sharedenv.SystemdServiceName()), nil
	}
	type forwarded struct {
		appID string
		rec   data.ApplicationRecord
	}
	got := make(chan forwarded, 4)
	manager.SetRecordSink(func(appID string, rec data.ApplicationRecord) { got <- forwarded{appID, rec} })

	dir, err := manager.Ensure("sh.wendy.model.m-1", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	send := func(rec data.ApplicationRecord) dataAck {
		body, _ := json.Marshal(rec)
		if err := writeDataFrame(conn, json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
		ackBody, err := readDataFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		var ack dataAck
		if err := json.Unmarshal(ackBody, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}

	status := data.ApplicationRecord{Version: 1, Type: "event", Name: "model.status",
		Attributes: map[string]any{"state": "ready"}, ClientBootID: "unavailable"}
	if ack := send(status); ack.State == "rejected" {
		t.Fatalf("ack = %+v", ack)
	}
	// A record that fails validation never reaches the sink.
	if ack := send(data.ApplicationRecord{Version: 1, Type: "event", ClientBootID: "unavailable"}); ack.State != "rejected" {
		t.Fatalf("a nameless event was accepted: %+v", ack)
	}
	select {
	case f := <-got:
		if f.appID != "sh.wendy.model.m-1" || f.rec.Name != "model.status" || f.rec.Attributes["state"] != "ready" {
			t.Fatalf("forwarded %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("the accepted record never reached the sink")
	}
	select {
	case f := <-got:
		t.Fatalf("a rejected record reached the sink: %+v", f)
	default:
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./go/internal/agent/services/ -run TestAppDataSocketForwardsAcceptedRecordsToSink`
Expected: FAIL to compile with `manager.SetRecordSink undefined`.

- [ ] **Step 3: Implement the sink**

In `go/internal/agent/services/app_data_socket.go`, add a field to `AppDataSocketManager` after `sockets`:

```go
	// recordSink also receives every record that passes validation; guarded by mu.
	recordSink func(appID string, rec data.ApplicationRecord)
```

Add these methods after `NewAppDataSocketManager`:

```go
// SetRecordSink sends every record the socket accepts to sink as well, after
// capture has seen it. The agent's model supervisor uses it to turn model
// host records into events (internal/agent/models). sink must not block: it
// runs on the connection's read path.
func (m *AppDataSocketManager) SetRecordSink(sink func(appID string, rec data.ApplicationRecord)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordSink = sink
}

func (m *AppDataSocketManager) sink() func(string, data.ApplicationRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recordSink
}
```

In `serveConn`, replace:

```go
		state, err := m.capture.RecordApplication(appID, rec)
		ack := dataAck{Version: 1, State: state}
```

with:

```go
		state, err := m.capture.RecordApplication(appID, rec)
		// A capture failure is about episode storage; live consumers still
		// get the record.
		if sink := m.sink(); sink != nil {
			sink(appID, rec)
		}
		ack := dataAck{Version: 1, State: state}
```

- [ ] **Step 4: Run the new test and the socket suite**

Run: `go test -race ./go/internal/agent/services/ -run 'AppDataSocket|DataProtocol'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/app_data_socket.go go/internal/agent/services/app_data_socket_sink_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
data socket: forward accepted records to an optional sink

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 12: containerd: the model host runtime

**Files:**
- Create: `go/internal/agent/containerd/model_host.go`
- Test: `go/internal/agent/containerd/model_host_test.go`

**Interfaces:**
- Consumes:
  - `models.HostSpec`, `models.HostExit` and the host path constants (Task 6);
  - `localoci.DefaultSpec`, `ResolveDeviceNode`, `RecordPinnedDevice`, `ApplyEntitlements`, `ApplyOptions{DataSocketDir}` and `DedupeDevices`;
  - `(*Client).applyNvidiaCDI`, `(*Client).applyQualcommNPURuntime`, `(*Client).terminateTask`, `(*Client).UnpackImage`, `c.dataSocketProvider` and `c.withNamespace`.
- Produces: `*containerd.Client` implements `models.Runtime` (`var _ models.Runtime = (*Client)(nil)`).

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/containerd/model_host_test.go`:

```go
package containerd

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func testModelHost() models.HostSpec {
	sha := strings.Repeat("1", 64)
	return models.HostSpec{
		InstanceID: "m-0000beef", AppID: models.AppIDPrefix + "m-0000beef",
		Image:  "ghcr.io/wendylabsinc/wendy-model-host-cpu@sha256:" + strings.Repeat("a", 64),
		Engine: models.EngineONNXRuntime, ModelID: "coco-detector", VariantID: "d-cpu", FileSHA256: sha,
		ModelFile: "/var/lib/wendy/models/files/sha256/" + sha, LabelsFile: "/var/lib/wendy/models/run/m-0000beef/labels.txt",
		CameraNode: "/dev/video255", CameraSource: "v4l2:/dev/video0", LogPath: "/var/lib/wendy/models/run/m-0000beef/host.log",
	}
}

func fakeCameraNode(t *testing.T) {
	t.Helper()
	orig := resolveModelCamera
	t.Cleanup(func() { resolveModelCamera = orig })
	resolveModelCamera = func(path, kind string, follow bool) (int64, int64, error) {
		if path != "/dev/video255" || kind != "c" || follow {
			t.Fatalf("resolved %q %q %v", path, kind, follow)
		}
		return 81, 255, nil
	}
}

var testHostImage = modelHostImage{Args: []string{"/app/host"}, Env: []string{"PATH=/usr/bin", "PYTHONPATH=/app"}, Cwd: "/app"}

func TestModelHostBaseSpecIsLockedDown(t *testing.T) {
	fakeCameraNode(t)
	spec, err := modelHostBaseSpec(testModelHost(), testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	u := spec.Process.User
	if u.UID != models.HostUID || u.GID != models.HostGID || !slices.Contains(u.AdditionalGids, uint32(modelHostVideoGID)) {
		t.Fatalf("user = %+v", u)
	}
	if !spec.Root.Readonly {
		t.Fatal("the root filesystem is writable")
	}
	if c := spec.Process.Capabilities; c == nil || len(c.Bounding)+len(c.Effective)+len(c.Permitted)+len(c.Inheritable)+len(c.Ambient) != 0 {
		t.Fatalf("capabilities = %+v", c)
	}
	if !slices.ContainsFunc(spec.Linux.Namespaces, func(ns localoci.LinuxNamespace) bool { return ns.Type == "network" }) {
		t.Fatal("the host shares the device's network; it must have none")
	}
	if !slices.Equal(spec.Process.Args, []string{"/app/host"}) || spec.Process.Cwd != "/app" {
		t.Fatalf("process = %+v", spec.Process)
	}
	for _, want := range []string{"PYTHONPATH=/app", "WENDY_CAMERA_NODE=/dev/video255", "WENDY_MODEL_FILE=" + models.HostModelFile} {
		if !slices.Contains(spec.Process.Env, want) {
			t.Fatalf("env lacks %q: %v", want, spec.Process.Env)
		}
	}
	var paths []string
	for _, kv := range spec.Process.Env {
		if strings.HasPrefix(kv, "PATH=") {
			paths = append(paths, kv)
		}
	}
	if !slices.Equal(paths, []string{"PATH=/usr/bin"}) {
		t.Fatalf("PATH entries = %v, want only the image's", paths)
	}
	var cameraRules int
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == 81 {
			cameraRules++
			if d.Minor == nil || *d.Minor != 255 {
				t.Fatalf("the camera rule is not scoped to one node: %+v", d)
			}
		}
	}
	if cameraRules != 1 {
		t.Fatalf("%d camera device rules, want 1", cameraRules)
	}
	mounts := map[string]localoci.Mount{}
	for _, m := range spec.Mounts {
		mounts[m.Destination] = m
	}
	for _, dst := range []string{models.HostModelFile, models.HostLabelsFile} {
		if m, ok := mounts[dst]; !ok || !slices.Contains(m.Options, "ro") {
			t.Fatalf("%s mount = %+v", dst, m)
		}
	}
	if m, ok := mounts["/dev"]; ok && m.Type == "bind" {
		t.Fatal("the device's /dev is bound into a model host")
	}
	if _, ok := mounts[models.HostEngineCacheDir]; ok {
		t.Fatal("an ONNX Runtime host got an engine cache")
	}
	if spec.Linux.CgroupsPath != "" {
		t.Fatal("the base spec must leave the cgroup path to finishModelHostSpec")
	}
}

func TestModelHostTensorRTGetsEngineCache(t *testing.T) {
	fakeCameraNode(t)
	h := testModelHost()
	h.Engine, h.EngineCache = models.EngineTensorRT, "/var/lib/wendy/models/engines/"+h.FileSHA256
	spec, err := modelHostBaseSpec(h, testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	var cache *localoci.Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == models.HostEngineCacheDir {
			cache = &spec.Mounts[i]
		}
	}
	if cache == nil || cache.Source != h.EngineCache || !slices.Contains(cache.Options, "rw") {
		t.Fatalf("engine cache mount = %+v", cache)
	}
	if !slices.Contains(spec.Process.Env, "WENDY_MODEL_ENGINE_CACHE="+models.HostEngineCacheDir) {
		t.Fatalf("env = %v", spec.Process.Env)
	}
}

func TestFinishModelHostSpecGrantsSocketAndScope(t *testing.T) {
	fakeCameraNode(t)
	dir, err := os.MkdirTemp("/tmp", "wmh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	lis, err := net.Listen("unix", filepath.Join(dir, "data.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	h := testModelHost()
	spec, err := modelHostBaseSpec(h, testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishModelHostSpec(spec, h, dir); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Process.Env, "WENDY_DATA_SOCKET=/run/wendy/data/data.sock") {
		t.Fatalf("env = %v", spec.Process.Env)
	}
	if !slices.Contains(spec.Process.User.AdditionalGids, uint32(2000)) {
		t.Fatal("no data socket group")
	}
	if want := "system.slice:" + sharedenv.SystemdServiceName() + ":" + h.AppID; spec.Linux.CgroupsPath != want {
		t.Fatalf("cgroup = %q, want %q", spec.Linux.CgroupsPath, want)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/containerd/ -run 'TestModelHost|TestFinishModelHostSpec'`
Expected: FAIL to compile with `undefined: modelHostBaseSpec`.

- [ ] **Step 3: Write the runtime**

`go/internal/agent/containerd/model_host.go`:

```go
package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	"go.uber.org/zap"
)

// Model host containers carry these labels and no sh.wendy/app.* label, so
// app listing, stats, restarts and camera sync never see them (as with the
// ROS 2 inspector). The supervisor owns their lifecycle.
const (
	labelKeyModelInstance = "sh.wendy/model.instance"
	labelKeyModelID       = "sh.wendy/model.id"
	labelKeyModelVariant  = "sh.wendy/model.variant"
	labelKeyModelFile     = "sh.wendy/model.file.sha256"
	modelHostPrefix       = "wendy-model-"
	// modelHostVideoGID opens the camera node, as the camera entitlement's
	// videoGroupGID does.
	modelHostVideoGID = 44
)

var _ models.Runtime = (*Client)(nil)

// resolveModelCamera finds a camera node's device numbers; tests replace it.
var resolveModelCamera = localoci.ResolveDeviceNode

func modelHostName(instanceID string) string { return modelHostPrefix + instanceID }

// HasImage reports whether a model host image is already present.
func (c *Client) HasImage(ctx context.Context, ref string) bool {
	_, err := c.client.GetImage(c.withNamespace(ctx), ref)
	return err == nil
}

// EnsureImage pulls and unpacks a model host image unless it is present.
func (c *Client) EnsureImage(ctx context.Context, ref string) error {
	ctx = c.withNamespace(ctx)
	image, err := c.client.GetImage(ctx, ref)
	if err != nil {
		if image, err = c.client.Pull(ctx, ref, containerd.WithPullUnpack); err != nil {
			return fmt.Errorf("pulling %s: %w", ref, err)
		}
	}
	unpacked, err := image.IsUnpacked(ctx, "")
	if err != nil {
		return fmt.Errorf("checking %s: %w", ref, err)
	}
	if !unpacked {
		if err := c.UnpackImage(ctx, image, nil); err != nil {
			return fmt.Errorf("unpacking %s: %w", ref, err)
		}
	}
	return nil
}

// StartHost creates and starts a model host. The channel receives once,
// when the host's process ends.
func (c *Client) StartHost(ctx context.Context, h models.HostSpec) (<-chan models.HostExit, error) {
	ctx = c.withNamespace(ctx)
	if c.dataSocketProvider == nil {
		return nil, errors.New("app data sockets are unavailable on this agent")
	}
	image, err := c.client.GetImage(ctx, h.Image)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", h.Image, err)
	}
	img, err := imageRunConfig(ctx, image)
	if err != nil {
		return nil, err
	}
	socketDir, err := c.dataSocketProvider.Ensure(h.AppID, "")
	if err != nil {
		return nil, fmt.Errorf("opening the data socket: %w", err)
	}
	release := func() { c.dataSocketProvider.ReleaseApp(h.AppID) }
	spec, err := c.modelHostSpec(h, img, socketDir)
	if err != nil {
		release()
		return nil, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		release()
		return nil, err
	}
	name := modelHostName(h.InstanceID)
	ctr, err := c.client.NewContainer(ctx, name,
		containerd.WithImage(image), containerd.WithNewSnapshot(name, image),
		containerd.WithContainerLabels(map[string]string{
			labelKeyModelInstance: h.InstanceID, labelKeyModelID: h.ModelID,
			labelKeyModelVariant: h.VariantID, labelKeyModelFile: h.FileSHA256,
		}),
		containerd.WithNewSpec(oci.WithSpecFromBytes(specJSON)),
	)
	if err != nil {
		release()
		return nil, fmt.Errorf("creating the model host: %w", err)
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
		release()
	}
	task, err := ctr.NewTask(ctx, cio.LogFile(h.LogPath))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating the model host task: %w", err)
	}
	// Wait before Start so an immediate exit is not missed; it outlives ctx.
	statusC, err := task.Wait(context.WithoutCancel(ctx))
	if err != nil {
		_, _ = task.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill)
		cleanup()
		return nil, fmt.Errorf("waiting on the model host: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill)
		cleanup()
		return nil, fmt.Errorf("starting the model host: %w", err)
	}
	exits := make(chan models.HostExit, 1)
	go func() {
		st := <-statusC
		code, _, err := st.Result()
		exits <- models.HostExit{Code: code, Err: err}
	}()
	return exits, nil
}

// RemoveHost stops and deletes a model host and releases its data socket.
func (c *Client) RemoveHost(ctx context.Context, instanceID string) error {
	ctx = c.withNamespace(ctx)
	defer func() {
		if c.dataSocketProvider != nil {
			c.dataSocketProvider.ReleaseApp(models.AppIDPrefix + instanceID)
		}
	}()
	ctr, err := c.client.LoadContainer(ctx, modelHostName(instanceID))
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if task, err := ctr.Task(ctx, nil); err == nil {
		if err := c.terminateTask(ctx, task, ctr.ID(), syscall.SIGTERM, stopGracePeriod, killWaitTimeout); err != nil {
			c.logger.Warn("stopping a model host failed", zap.String("instance", instanceID), zap.Error(err))
		}
	}
	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("deleting the model host: %w", err)
	}
	return nil
}

// ListHosts returns the instance ids of every model host container.
func (c *Client) ListHosts(ctx context.Context) ([]string, error) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyModelInstance))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ctr := range ctrs {
		labels, err := ctr.Labels(ctx)
		if err != nil {
			continue
		}
		if id := labels[labelKeyModelInstance]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// modelHostImage is the part of an image's config a model host inherits.
type modelHostImage struct {
	Args []string
	Env  []string
	Cwd  string
}

func imageRunConfig(ctx context.Context, image containerd.Image) (modelHostImage, error) {
	spec, err := image.Spec(ctx)
	if err != nil {
		return modelHostImage{}, fmt.Errorf("reading the model host image config: %w", err)
	}
	args := append(append([]string{}, spec.Config.Entrypoint...), spec.Config.Cmd...)
	if len(args) == 0 {
		return modelHostImage{}, errors.New("the model host image has no entrypoint or command")
	}
	cwd := spec.Config.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	return modelHostImage{Args: args, Env: spec.Config.Env, Cwd: cwd}, nil
}

// modelHostSpec is the full spec: the locked-down base, the engine's
// accelerator runtime, then the grants and cgroup scope.
func (c *Client) modelHostSpec(h models.HostSpec, img modelHostImage, socketDir string) (*localoci.Spec, error) {
	spec, err := modelHostBaseSpec(h, img)
	if err != nil {
		return nil, err
	}
	switch h.Engine {
	case models.EngineTensorRT:
		if err := c.applyNvidiaCDI(spec); err != nil {
			return nil, fmt.Errorf("adding the NVIDIA runtime: %w", err)
		}
	case models.EngineQNN:
		c.applyQualcommNPURuntime(spec)
	}
	if err := finishModelHostSpec(spec, h, socketDir); err != nil {
		return nil, err
	}
	return spec, nil
}

// modelHostBaseSpec runs a model host as an unprivileged user with a
// read-only root, no capabilities, and a network namespace of its own that no
// CNI configures, so it has no network: the agent fetches everything the host
// needs. The host sees exactly one camera node, not the whole /dev that the
// camera entitlement grants.
func modelHostBaseSpec(h models.HostSpec, img modelHostImage) (*localoci.Spec, error) {
	spec := localoci.DefaultSpec("rootfs", img.Args)
	spec.Process.Cwd = img.Cwd
	spec.Process.User = localoci.User{UID: models.HostUID, GID: models.HostGID, AdditionalGids: []uint32{modelHostVideoGID}}
	spec.Process.Capabilities = &localoci.LinuxCapabilities{}
	spec.Root.Readonly = true
	spec.Process.Env = mergeEnv(spec.Process.Env, img.Env, h.Env())
	ro := []string{"bind", "ro", "nosuid", "nodev", "noexec"}
	spec.Mounts = append(spec.Mounts,
		localoci.Mount{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=256m"}},
		localoci.Mount{Destination: models.HostModelFile, Type: "bind", Source: h.ModelFile, Options: ro},
		localoci.Mount{Destination: models.HostLabelsFile, Type: "bind", Source: h.LabelsFile, Options: ro},
	)
	if h.EngineCache != "" {
		spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: models.HostEngineCacheDir, Type: "bind",
			Source: h.EngineCache, Options: []string{"bind", "rw", "nosuid", "nodev", "noexec"}})
	}
	major, minor, err := resolveModelCamera(h.CameraNode, "c", false)
	if err != nil {
		return nil, fmt.Errorf("camera node %s: %w", h.CameraNode, err)
	}
	spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: h.CameraNode, Source: h.CameraNode, Type: "bind",
		Options: []string{"bind", "rw", "nosuid", "noexec"}})
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices,
		localoci.LinuxDeviceCgroup{Allow: true, Type: "c", Major: &major, Minor: &minor, Access: "rw"})
	localoci.RecordPinnedDevice(spec, h.CameraNode, "c", major, minor)
	return spec, nil
}

// mergeEnv combines environments. A later entry replaces an earlier one with
// the same key: a process reads the first match, so a plain append would let
// the default PATH shadow the image's, and the image could shadow the host
// contract.
func mergeEnv(lists ...[]string) []string {
	index := map[string]int{}
	var out []string
	for _, list := range lists {
		for _, kv := range list {
			key, _, _ := strings.Cut(kv, "=")
			if i, ok := index[key]; ok {
				out[i] = kv
				continue
			}
			index[key] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// finishModelHostSpec grants the data socket and the engine's accelerator
// through the same entitlement code apps use, then sets the cgroup scope that
// the data socket's peer check attributes to h.AppID.
func finishModelHostSpec(spec *localoci.Spec, h models.HostSpec, socketDir string) error {
	ents := []appconfig.Entitlement{{Type: appconfig.EntitlementEpisodeWrite}}
	switch h.Engine {
	case models.EngineTensorRT:
		ents = append(ents, appconfig.Entitlement{Type: appconfig.EntitlementGPU})
	case models.EngineQNN:
		ents = append(ents, appconfig.Entitlement{Type: appconfig.EntitlementNPU})
	}
	cfg := &appconfig.AppConfig{AppID: h.AppID, Entitlements: ents}
	if err := localoci.ApplyEntitlements(spec, cfg, localoci.ApplyOptions{DataSocketDir: socketDir}); err != nil {
		return fmt.Errorf("granting model host access: %w", err)
	}
	if spec.Linux.CgroupsPath != "" {
		return fmt.Errorf("security: CgroupsPath was set before assignment (%q)", spec.Linux.CgroupsPath)
	}
	spec.Linux.CgroupsPath = fmt.Sprintf("system.slice:%s:%s", sharedenv.SystemdServiceName(), h.AppID)
	localoci.DedupeDevices(spec)
	return nil
}
```

- [ ] **Step 4: Run the tests and the package**

Run: `go test ./go/internal/agent/containerd/ -run 'TestModelHost|TestFinishModelHostSpec'`
Expected: PASS.
Run: `go vet ./go/internal/agent/containerd/ && go test ./go/internal/agent/containerd/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/containerd/model_host.go go/internal/agent/containerd/model_host_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
containerd: run locked-down model host containers

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 13: services: `ModelService` gRPC handlers and adapters

**Files:**
- Create: `go/internal/agent/services/model_service.go`
- Create: `go/internal/agent/services/model_adapters.go`
- Test: `go/internal/agent/services/model_service_test.go`

**Interfaces:**
- Consumes:
  - `models.Supervisor` (Tasks 6–9) and `modelstest` fakes;
  - the generated `agentpbv2` types (Task 2);
  - `fakeServerStream[T]` (existing, `ros2_service_test.go:426`);
  - `(*VideoService).AcquireTwoPlaneNode` and `ReleaseTwoPlaneNode` (Task 10);
  - `detectGPUInfo`, `detectNPUInfo` and `gpudiscovery.Summary` (existing).
- Produces:
  - `NewModelService(*zap.Logger, *models.Supervisor) *ModelService`;
  - `ModelDeviceProfile() models.DeviceProfile`;
  - `ModelCameras{Video *VideoService; Data *data.Manager}`, which implements `models.Cameras`.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/services/model_service_test.go`:

```go
package services

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestModelService(t *testing.T) (*ModelService, *modelstest.Runtime) {
	t.Helper()
	rt := modelstest.NewRuntime()
	sup := models.NewSupervisor(models.Config{
		Catalog: modelstest.Catalog(models.EngineONNXRuntime), Device: models.DeviceProfile{Arch: "arm64"},
		Runtime: rt, Cameras: modelstest.NewCameras(models.Camera{SourceID: "v4l2:/dev/video0", Name: "front door"}),
		Files: modelstest.NewFiles(), Root: t.TempDir(), Clock: modelstest.NewClock(),
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sup.Shutdown(ctx)
	})
	return NewModelService(zap.NewNop(), sup), rt
}

func startTestModel(t *testing.T, svc *ModelService, rt *modelstest.Runtime) string {
	t.Helper()
	started, err := svc.StartModel(context.Background(), &agentpbv2.StartModelRequest{ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video0"})
	if err != nil {
		t.Fatal(err)
	}
	id := started.GetInstance().GetInstanceId()
	modelstest.Eventually(t, "the host to start", func() bool { return rt.Running(id) })
	return id
}

// watchInBackground runs WatchModel and hands every sent message to the test.
func watchInBackground(ctx context.Context, svc *ModelService, req *agentpbv2.WatchModelRequest) (<-chan *agentpbv2.ModelWatchMessage, <-chan error) {
	sent := make(chan *agentpbv2.ModelWatchMessage, 64)
	stream := &fakeServerStream[agentpbv2.ModelWatchMessage]{ctx: ctx}
	stream.onSend = func(n int) error { sent <- stream.sent[n-1]; return nil }
	done := make(chan error, 1)
	go func() { done <- svc.WatchModel(req, stream) }()
	return sent, done
}

func receive(t *testing.T, sent <-chan *agentpbv2.ModelWatchMessage, match func(*agentpbv2.ModelWatchMessage) bool) *agentpbv2.ModelWatchMessage {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sent:
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatal("the expected message never arrived")
		}
	}
}

func TestModelServiceCatalogStartAndList(t *testing.T) {
	svc, rt := newTestModelService(t)
	ctx := context.Background()
	cat, err := svc.ListCatalog(ctx, &agentpbv2.ListModelCatalogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.GetModels()) != 1 || cat.GetModels()[0].GetVariant().GetEngine() != models.EngineONNXRuntime ||
		len(cat.GetCameras()) != 1 || cat.GetMaxRunning() != 2 || cat.GetModels()[0].GetVariant().GetDownloadBytes() != 10 {
		t.Fatalf("catalog = %v", cat)
	}
	id := startTestModel(t, svc, rt)
	list, err := svc.ListModels(ctx, &agentpbv2.ListModelsRequest{})
	if err != nil || len(list.GetInstances()) != 1 || list.GetInstances()[0].GetInstanceId() != id {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list.GetInstances()[0].GetStartedUnixNanos() == 0 {
		t.Fatal("the start time is not reported")
	}
}

func TestModelServiceMapsErrorsToCodes(t *testing.T) {
	svc, _ := newTestModelService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"unknown model", func() error {
			_, err := svc.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: "nope", CameraSourceId: "v4l2:/dev/video0"})
			return err
		}, codes.NotFound},
		{"unknown camera", func() error {
			_, err := svc.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video9"})
			return err
		}, codes.NotFound},
		{"unknown instance", func() error {
			_, err := svc.StopModel(ctx, &agentpbv2.StopModelRequest{InstanceId: "m-missing"})
			return err
		}, codes.NotFound},
	}
	for _, tc := range cases {
		if got := status.Code(tc.call()); got != tc.want {
			t.Errorf("%s: code %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWatchModelStreamsEventsAndDetachesOnDisconnect(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)
	ctx, cancel := context.WithCancel(context.Background())
	sent, done := watchInBackground(ctx, svc, &agentpbv2.WatchModelRequest{InstanceId: id, Classes: []string{"person"}})

	started := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })
	if started.GetStarted().GetWatchId() == "" || started.GetStarted().GetInstance().GetInstanceId() != id {
		t.Fatalf("started = %v", started)
	}
	svc.supervisor.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Entered("car", 0.9, 1))
	svc.supervisor.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Entered("person", 0.9, 2))
	event := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetEvent() != nil }).GetEvent()
	if event.GetClassName() != "person" || event.GetSequence() != 2 || event.GetBox().GetHeight() == 0 {
		t.Fatalf("event = %v", event)
	}

	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatalf("WatchModel after disconnect = %v", err)
	}
	list, _ := svc.ListModels(context.Background(), &agentpbv2.ListModelsRequest{})
	if got := list.GetInstances()[0].GetWatchers(); got != 0 {
		t.Fatalf("watchers after disconnect = %d; the lost watch must be detached", got)
	}
}

func TestWatchModelEndsWithFinalStatus(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)
	sent, done := watchInBackground(context.Background(), svc, &agentpbv2.WatchModelRequest{InstanceId: id})
	receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })
	if _, err := svc.StopModel(context.Background(), &agentpbv2.StopModelRequest{InstanceId: id}); err != nil {
		t.Fatal(err)
	}
	final := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool {
		return m.GetStatus().GetState() == agentpbv2.ModelState_MODEL_STATE_STOPPED
	})
	if final.GetStatus().GetStateDetail() != "stopped" {
		t.Fatalf("final = %v", final)
	}
	if err := <-done; err != nil {
		t.Fatalf("WatchModel = %v, want a clean end", err)
	}
}

func TestModelCamerasListsHealthyLocalCameras(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{
			{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "C920 USB"},
			{ID: "ipcamera:200", Kind: "camera", Healthy: true, Detail: "porch"},
			{ID: "v4l2:/dev/video4", Kind: "camera", Healthy: false},
		}
	})
	got := ModelCameras{Data: m}.List(context.Background())
	if want := []models.Camera{{SourceID: "v4l2:/dev/video0", Name: "C920 USB"}}; !slices.Equal(got, want) {
		t.Fatalf("cameras = %+v, want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/services/ -run 'TestModelService|TestWatchModel|TestModelCameras'`
Expected: FAIL to compile with `undefined: NewModelService`.

- [ ] **Step 3: Write the service**

`go/internal/agent/services/model_service.go`:

```go
package services

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ModelService serves WendyModelService from the model supervisor.
type ModelService struct {
	agentpbv2.UnimplementedWendyModelServiceServer
	logger     *zap.Logger
	supervisor *models.Supervisor
}

func NewModelService(logger *zap.Logger, supervisor *models.Supervisor) *ModelService {
	return &ModelService{logger: logger, supervisor: supervisor}
}

func modelStatusError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, models.ErrUnknownModel), errors.Is(err, models.ErrUnknownCamera),
		errors.Is(err, models.ErrUnknownInstance), errors.Is(err, models.ErrUnknownWatch):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, models.ErrNoVariant), errors.Is(err, models.ErrCameraNotStreamable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, models.ErrCapacity):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, models.ErrInvalidFilter):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (s *ModelService) ListCatalog(ctx context.Context, _ *agentpbv2.ListModelCatalogRequest) (*agentpbv2.ListModelCatalogResponse, error) {
	view := s.supervisor.Catalog(ctx)
	resp := &agentpbv2.ListModelCatalogResponse{
		CatalogVersion: view.Version, MaxRunning: uint32(view.MaxRunning), Running: uint32(view.Running),
	}
	for _, e := range view.Models {
		m := &agentpbv2.CatalogModel{Id: e.Model.ID, Description: e.Model.Description, Kind: e.Model.Kind,
			Labels: e.Model.Labels, UnavailableReason: e.UnavailableReason}
		if e.Variant != nil {
			m.Variant = &agentpbv2.CatalogVariant{Id: e.Variant.ID, Engine: e.Variant.Engine,
				DownloadBytes: uint64(e.DownloadBytes), NeedsEngineBuild: e.NeedsEngineBuild, ImageCached: e.ImageCached}
		}
		resp.Models = append(resp.Models, m)
	}
	for _, c := range view.Cameras {
		resp.Cameras = append(resp.Cameras, &agentpbv2.ModelCamera{SourceId: c.SourceID, Name: c.Name})
	}
	return resp, nil
}

func (s *ModelService) StartModel(ctx context.Context, req *agentpbv2.StartModelRequest) (*agentpbv2.StartModelResponse, error) {
	info, reused, err := s.supervisor.Start(ctx, req.GetModelId(), req.GetCameraSourceId())
	if err != nil {
		return nil, modelStatusError(err)
	}
	return &agentpbv2.StartModelResponse{Instance: modelInstanceProto(info), Reused: reused}, nil
}

// WatchModel holds the instance for as long as the client stays connected.
// A client that goes away is detached, which starts the lease grace period.
func (s *ModelService) WatchModel(req *agentpbv2.WatchModelRequest, stream grpc.ServerStreamingServer[agentpbv2.ModelWatchMessage]) error {
	w, info, last, err := s.supervisor.Watch(models.WatchRequest{
		InstanceID: req.GetInstanceId(), Label: req.GetLabel(), AfterSequence: req.GetAfterSequence(),
		Filter: models.Filter{Classes: req.GetClasses(), MinConfidence: req.GetMinConfidence(), Types: req.GetEventTypes()},
	})
	if err != nil {
		return modelStatusError(err)
	}
	started := &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Started{Started: &agentpbv2.WatchStarted{
		WatchId: w.ID, Instance: modelInstanceProto(info), LastSequence: last}}}
	if err := stream.Send(started); err != nil {
		s.supervisor.Detach(w.InstanceID, w.ID)
		return err
	}
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			s.supervisor.Detach(w.InstanceID, w.ID)
			return status.FromContextError(ctx.Err()).Err()
		case msg, ok := <-w.C:
			if !ok {
				if final := w.Final(); final != nil {
					return stream.Send(modelStatusMessage(*final))
				}
				return nil // the watch was stopped explicitly
			}
			if err := stream.Send(modelWatchMessage(msg)); err != nil {
				s.supervisor.Detach(w.InstanceID, w.ID)
				return err
			}
		}
	}
}

func (s *ModelService) ListModels(context.Context, *agentpbv2.ListModelsRequest) (*agentpbv2.ListModelsResponse, error) {
	resp := &agentpbv2.ListModelsResponse{}
	for _, info := range s.supervisor.List() {
		resp.Instances = append(resp.Instances, modelInstanceProto(info))
	}
	return resp, nil
}

func (s *ModelService) StopModel(ctx context.Context, req *agentpbv2.StopModelRequest) (*agentpbv2.StopModelResponse, error) {
	info, err := s.supervisor.Stop(ctx, req.GetInstanceId(), req.GetWatchId())
	if err != nil {
		return nil, modelStatusError(err)
	}
	return &agentpbv2.StopModelResponse{Instance: modelInstanceProto(info)}, nil
}

func modelInstanceProto(i models.InstanceInfo) *agentpbv2.ModelInstance {
	return &agentpbv2.ModelInstance{
		InstanceId: i.ID, ModelId: i.ModelID, VariantId: i.VariantID, Engine: i.Engine, CameraSourceId: i.CameraSourceID,
		State: modelStateProto(i.State), StateDetail: i.StateDetail, Watchers: uint32(i.Watchers), WatchLabels: i.WatchLabels,
		StartedUnixNanos: i.StartedAt.UnixNano(), FileSha256: i.FileSHA256,
		Stats: &agentpbv2.ModelStats{ProcessedFps: i.Stats.ProcessedFPS, LatencyP50Ms: i.Stats.LatencyP50Ms, FramesSkipped: i.Stats.FramesSkipped},
	}
}

func modelStateProto(s models.State) agentpbv2.ModelState {
	switch s {
	case models.StatePreparing:
		return agentpbv2.ModelState_MODEL_STATE_PREPARING
	case models.StateStarting:
		return agentpbv2.ModelState_MODEL_STATE_STARTING
	case models.StateReady:
		return agentpbv2.ModelState_MODEL_STATE_READY
	case models.StateRestarting:
		return agentpbv2.ModelState_MODEL_STATE_RESTARTING
	case models.StateFailed:
		return agentpbv2.ModelState_MODEL_STATE_FAILED
	case models.StateStopped:
		return agentpbv2.ModelState_MODEL_STATE_STOPPED
	}
	return agentpbv2.ModelState_MODEL_STATE_UNSPECIFIED
}

func modelStatusMessage(i models.InstanceInfo) *agentpbv2.ModelWatchMessage {
	return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Status{Status: modelInstanceProto(i)}}
}

func modelWatchMessage(m models.WatchMessage) *agentpbv2.ModelWatchMessage {
	switch {
	case m.Status != nil:
		return modelStatusMessage(*m.Status)
	case m.Event != nil:
		e := m.Event
		return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Event{Event: &agentpbv2.ModelEvent{
			Sequence: e.Sequence, Type: e.Type, ClassName: e.Class, Confidence: e.Confidence, TrackId: e.TrackID,
			Box:      &agentpbv2.BoundingBox{X: e.Box.X, Y: e.Box.Y, Width: e.Box.Width, Height: e.Box.Height},
			SourceId: e.SourceID, SampleId: e.SampleID, TimeUnixNanos: e.Time.UnixNano(),
		}}}
	default:
		return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Gap{Gap: &agentpbv2.ModelGap{
			FirstMissing: m.Gap.FirstMissing, LastMissing: m.Gap.LastMissing}}}
	}
}
```

`go/internal/agent/services/model_adapters.go`:

```go
package services

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
)

// ModelDeviceProfile describes this device for model variant selection,
// using the same detection device info reports.
func ModelDeviceProfile() models.DeviceProfile {
	gpu := detectGPUInfo()
	npu := detectNPUInfo()
	_, backends := gpudiscovery.Summary(gpu.devices)
	return models.DeviceProfile{Arch: runtime.GOARCH, GPUVendor: gpu.vendor, GPUArch: gpu.gpuArch,
		ComputeBackends: backends, NPUBackends: npu.backends}
}

// ModelCameras serves models.Cameras: local V4L2 cameras from the data
// manager's sources, and their two-plane nodes from the video service.
type ModelCameras struct {
	Video *VideoService
	Data  *data.Manager
}

var _ models.Cameras = ModelCameras{}

func (c ModelCameras) List(ctx context.Context) []models.Camera {
	var out []models.Camera
	for _, src := range c.Data.Sources(ctx) {
		if src.Kind == "camera" && src.Healthy && strings.HasPrefix(src.ID, "v4l2:") {
			out = append(out, models.Camera{SourceID: src.ID, Name: src.Detail})
		}
	}
	return out
}

func (c ModelCameras) Acquire(ctx context.Context, owner, sourceID string) (string, error) {
	node, err := c.Video.AcquireTwoPlaneNode(ctx, owner, sourceID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", models.ErrCameraNotStreamable, err)
	}
	return node, nil
}

func (c ModelCameras) Release(ctx context.Context, owner string) { c.Video.ReleaseTwoPlaneNode(ctx, owner) }
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./go/internal/agent/services/ -run 'TestModelService|TestWatchModel|TestModelCameras'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/model_service.go go/internal/agent/services/model_adapters.go go/internal/agent/services/model_service_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
services: serve WendyModelService from the model supervisor

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 14: Wire the model service into the agent

**Files:**
- Create: `go/cmd/wendy-agent/models.go`
- Test: `go/cmd/wendy-agent/models_test.go`
- Modify: `go/cmd/wendy-agent/main.go`: after the camera loopback wiring (~line 392); after the tunnel registration on the mTLS server (~line 787); after `registerAllServices(localSocketServer)` (~line 959)

**Interfaces:**
- Consumes: Tasks 9, 11, 12 and 13.
- Produces: the agent serves `WendyModelService` over mTLS and on the admin socket. `WENDY_MODEL_CATALOG_FILE` overrides the catalog for development.

- [ ] **Step 1: Write the failing tests**

`go/cmd/wendy-agent/models_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestLoadModelCatalogDefaultsToBuiltIn(t *testing.T) {
	c, err := loadModelCatalog(zap.NewNop(), "")
	if err != nil || c.Version == "" {
		t.Fatalf("catalog = %+v, %v", c, err)
	}
}

func TestLoadModelCatalogOverride(t *testing.T) {
	sha := strings.Repeat("1", 64)
	variant := func(image string) string {
		return `{"version":"dev","models":[{"id":"fake-detector","kind":"detector","labels":["person"],"variants":[{"id":"v","engine":"onnxruntime","host_image":"` +
			image + `","file":{"url":"https://example.com/m","sha256":"` + sha + `","bytes":1},"input_size":1}]}]}`
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(good, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake@sha256:"+strings.Repeat("a", 64))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake:latest")), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := loadModelCatalog(zap.NewNop(), good); err != nil || c.Version != "dev" {
		t.Fatalf("override = %+v, %v", c, err)
	}
	if _, err := loadModelCatalog(zap.NewNop(), bad); err == nil {
		t.Fatal("an override with a tag-pinned image was accepted")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/cmd/wendy-agent/ -run TestLoadModelCatalog`
Expected: FAIL to compile with `undefined: loadModelCatalog`.

- [ ] **Step 3: Write the wiring helpers**

`go/cmd/wendy-agent/models.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	agentcontainerd "github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	agentdata "github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentmodels "github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
)

// modelRoot holds model files, built engines and per-instance run directories.
const modelRoot = "/var/lib/wendy/models"

// newModelSupervisor builds the model supervisor and removes model hosts a
// previous agent process left running.
func newModelSupervisor(ctx context.Context, logger *zap.Logger, ctrd *agentcontainerd.Client, video *services.VideoService, dataManager *agentdata.Manager) (*agentmodels.Supervisor, error) {
	catalog, err := loadModelCatalog(logger, os.Getenv("WENDY_MODEL_CATALOG_FILE"))
	if err != nil {
		return nil, err
	}
	sup := agentmodels.NewSupervisor(agentmodels.Config{
		Catalog: catalog,
		Device:  services.ModelDeviceProfile(),
		Runtime: ctrd,
		Cameras: services.ModelCameras{Video: video, Data: dataManager},
		Files:   agentmodels.NewFileCache(filepath.Join(modelRoot, "files"), nil),
		Root:    modelRoot,
		Logger:  logger.Named("models"),
	})
	if err := sup.CleanupOrphans(ctx); err != nil {
		logger.Warn("Removing model hosts left by a previous agent failed", zap.Error(err))
	}
	return sup, nil
}

// loadModelCatalog returns the catalog built into the agent, or the file at
// path when it is set. The override exists for development, for example to
// run the fake model host. Only root can set the agent's environment.
func loadModelCatalog(logger *zap.Logger, path string) (agentmodels.Catalog, error) {
	if path == "" {
		return agentmodels.DefaultCatalog()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return agentmodels.Catalog{}, fmt.Errorf("reading WENDY_MODEL_CATALOG_FILE: %w", err)
	}
	logger.Warn("Using a development model catalog", zap.String("path", path))
	return agentmodels.ParseCatalog(raw)
}
```

- [ ] **Step 4: Wire it into `main.go`**

In `go/cmd/wendy-agent/main.go`, directly after this block:

```go
	if ctrdClient != nil {
		ctrdClient.SetCameraLoopbackProvider(videoSvc)
		ctrdClient.SyncCameraLoopbacks(ctx)
		go ctrdClient.RunCameraLoopbackSync(ctx, time.Minute)
	}
```

add:

```go
	// Models the agent runs for clients such as wendy chat
	// (specs/2026-09-25-model-watch-design.md). Model hosts are containers,
	// so the service needs containerd.
	var modelSvc *services.ModelService
	if ctrdClient != nil {
		modelSupervisor, err := newModelSupervisor(ctx, logger, ctrdClient, videoSvc, dataManager)
		if err != nil {
			logger.Error("Model service disabled", zap.Error(err))
		} else {
			appDataSocketManager.SetRecordSink(modelSupervisor.PublishApplicationRecord)
			modelSvc = services.NewModelService(logger, modelSupervisor)
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				modelSupervisor.Shutdown(shutdownCtx)
			}()
		}
	}
```

After `agentpbv2.RegisterWendyTunnelServiceServer(srv, services.NewTunnelService(logger))` in the mTLS server setup, add:

```go
		// WendyModelService runs containers with camera and accelerator access
		// for a client. Like the tunnel, it stays off the plaintext
		// provisioning listener; the admin socket registers it separately.
		if modelSvc != nil {
			agentpbv2.RegisterWendyModelServiceServer(srv, modelSvc)
		}
```

After `registerAllServices(localSocketServer)`, add:

```go
		if modelSvc != nil {
			agentpbv2.RegisterWendyModelServiceServer(localSocketServer, modelSvc)
		}
```

- [ ] **Step 5: Run the tests, then build the agent for both architectures**

Run: `go test ./go/cmd/wendy-agent/ -run TestLoadModelCatalog`
Expected: PASS.
Run: `go vet ./go/cmd/wendy-agent/ && (cd go && make build-agent-linux-arm64 build-agent-linux-amd64)`
Expected: success, producing `go/bin/wendy-agent-linux-arm64` and `go/bin/wendy-agent-linux-amd64`.
Run: `grep -n "RegisterWendyModelServiceServer" go/cmd/wendy-agent/main.go`
Expected: exactly two lines, one under the mTLS server and one under the local socket server. Neither is inside `registerAllServices`.

- [ ] **Step 6: Commit**

```bash
git add go/cmd/wendy-agent/models.go go/cmd/wendy-agent/models_test.go go/cmd/wendy-agent/main.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
agent: serve WendyModelService over mTLS and the admin socket

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 15: CLI: `wendy device model catalog | list | stop`

**Files:**
- Create: `go/internal/cli/commands/device_model.go`
- Test: `go/internal/cli/commands/device_model_test.go`
- Modify: `go/internal/cli/commands/device.go`: add `newDeviceModelCmd(),` to `addToGroup("apps", …)`
- Modify: `go/internal/cli/commands/commands_test.go`: add `"model"` to `expectedSubs` in `TestNewDeviceCmd`

**Interfaces:**
- Consumes: `grpcclient.AgentConnection.ModelService` (Task 2), and the existing helpers `connectToAgent`, `encodeProtoJSON`, `tui.RenderTable`, `jsonOutput`, `startUDSAgent` and `captureCommandStdout`.
- Produces:
  - commands: `newDeviceModelCmd()`, `newDeviceModelCatalogCmd()`, `newDeviceModelListCmd()`, `newDeviceModelStopCmd()`;
  - helpers: `withModelClient(ctx, fn)`, `modelServiceErr(err)`, `modelStateLabel(*agentpbv2.ModelInstance) string` and `modelStateText(*agentpbv2.ModelInstance) string`. Task 16 uses these.

- [ ] **Step 1: Write the failing tests**

`go/internal/cli/commands/device_model_test.go`:

```go
package commands

import (
	"context"
	"strings"
	"sync"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

// fakeModelService records calls; Task 16 extends it with watching.
type fakeModelService struct {
	agentpbv2.UnimplementedWendyModelServiceServer
	catalog *agentpbv2.ListModelCatalogResponse
	list    *agentpbv2.ListModelsResponse
	events  []*agentpbv2.ModelWatchMessage
	hold    bool // keep WatchModel open until the client leaves

	mu      sync.Mutex
	stopped []string // "instance/watch"
	start   *agentpbv2.StartModelRequest
	watch   *agentpbv2.WatchModelRequest
}

func (f *fakeModelService) ListCatalog(context.Context, *agentpbv2.ListModelCatalogRequest) (*agentpbv2.ListModelCatalogResponse, error) {
	return f.catalog, nil
}

func (f *fakeModelService) ListModels(context.Context, *agentpbv2.ListModelsRequest) (*agentpbv2.ListModelsResponse, error) {
	return f.list, nil
}

func (f *fakeModelService) StopModel(_ context.Context, req *agentpbv2.StopModelRequest) (*agentpbv2.StopModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req.GetInstanceId()+"/"+req.GetWatchId())
	return &agentpbv2.StopModelResponse{Instance: &agentpbv2.ModelInstance{
		InstanceId: req.GetInstanceId(), State: agentpbv2.ModelState_MODEL_STATE_STOPPED, StateDetail: "stopped"}}, nil
}

func (f *fakeModelService) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func serveModels(t *testing.T, fake *fakeModelService) {
	t.Helper()
	startUDSAgent(t, func(srv *grpc.Server) { agentpbv2.RegisterWendyModelServiceServer(srv, fake) })
	old := jsonOutput
	t.Cleanup(func() { jsonOutput = old })
}

func TestDeviceModelCatalogHumanAndJSON(t *testing.T) {
	serveModels(t, &fakeModelService{catalog: &agentpbv2.ListModelCatalogResponse{
		CatalogVersion: "test", MaxRunning: 2,
		Models: []*agentpbv2.CatalogModel{{Id: "coco-detector", Kind: "detector", Labels: []string{"person", "car"},
			Variant: &agentpbv2.CatalogVariant{Id: "d-cpu", Engine: "onnxruntime", DownloadBytes: 12 << 20}}},
		Cameras: []*agentpbv2.ModelCamera{{SourceId: "v4l2:/dev/video0", Name: "C920"}},
	}})
	cmd := newDeviceModelCatalogCmd()
	cmd.SetContext(context.Background())

	jsonOutput = false
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"coco-detector", "onnxruntime", "2 classes", "downloads its runtime image", "12 MB", "v4l2:/dev/video0", "0 of 2 model slots"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	jsonOutput = true
	out, err = captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil || !strings.Contains(out, `"catalog_version":"test"`) {
		t.Fatalf("json = %s, %v", out, err)
	}
}

func TestDeviceModelListShowsState(t *testing.T) {
	serveModels(t, &fakeModelService{list: &agentpbv2.ListModelsResponse{Instances: []*agentpbv2.ModelInstance{{
		InstanceId: "m-1", ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video0",
		State: agentpbv2.ModelState_MODEL_STATE_READY, Watchers: 1, FileSha256: strings.Repeat("ab", 32),
		Stats: &agentpbv2.ModelStats{ProcessedFps: 9.8}}}}})
	jsonOutput = false
	cmd := newDeviceModelListCmd()
	cmd.SetContext(context.Background())
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"m-1", "coco-detector", "ready (9.8 fps)", "abababababab…"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDeviceModelStopStopsOutright(t *testing.T) {
	fake := &fakeModelService{}
	serveModels(t, fake)
	jsonOutput = false
	cmd := newDeviceModelStopCmd()
	cmd.SetContext(context.Background())
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, []string{"m-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "m-1/" {
		t.Fatalf("stops = %v, want an outright stop of m-1", got)
	}
	if !strings.Contains(out, "Stopped m-1") {
		t.Fatalf("output = %q", out)
	}
}

func TestDeviceModelExplainsOldAgents(t *testing.T) {
	startUDSAgent(t) // serves no model service
	cmd := newDeviceModelListCmd()
	cmd.SetContext(context.Background())
	_, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "does not run models yet") {
		t.Fatalf("err = %v", err)
	}
}
```

In `go/internal/cli/commands/commands_test.go` `TestNewDeviceCmd`, change `expectedSubs` to include `"model"`:

```go
	expectedSubs := []string{"info", "version", "set-default", "get-default", "unset-default", "setup", "update", "model"}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/cli/commands/ -run 'TestDeviceModel|TestNewDeviceCmd'`
Expected: FAIL to compile with `undefined: newDeviceModelCatalogCmd`.

- [ ] **Step 3: Write the commands**

`go/internal/cli/commands/device_model.go`:

```go
package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newDeviceModelCmd is `wendy device model`: run catalog models on a device.
func newDeviceModelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Run AI models from the catalog on a device's cameras",
		Long: `Run detectors and other catalog models on a device's cameras.

A model runs while something watches it. 'wendy device model run' watches in
the foreground; after it exits, the device stops the model a minute later
unless another client is watching.`,
	}
	cmd.AddCommand(newDeviceModelCatalogCmd(), newDeviceModelListCmd(), newDeviceModelStopCmd())
	return cmd
}

func withModelClient(ctx context.Context, fn func(agentpbv2.WendyModelServiceClient) error) error {
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn.ModelService)
}

// modelServiceErr explains an agent that predates the model service.
func modelServiceErr(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errors.New("this device's agent does not run models yet; update it with `wendy device update`")
	}
	return err
}

func newDeviceModelCatalogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "catalog",
		Short: "List the models and cameras this device can use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				resp, err := client.ListCatalog(cmd.Context(), &agentpbv2.ListModelCatalogRequest{})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				fmt.Fprint(cmd.OutOrStdout(), renderModelCatalog(resp))
				return nil
			})
		},
	}
}

func renderModelCatalog(resp *agentpbv2.ListModelCatalogResponse) string {
	var b strings.Builder
	if len(resp.GetModels()) == 0 {
		b.WriteString("No models in this agent's catalog yet.\n")
	} else {
		rows := make([][]string, 0, len(resp.GetModels()))
		for _, m := range resp.GetModels() {
			engine, firstStart := "—", m.GetUnavailableReason()
			if v := m.GetVariant(); v != nil {
				engine, firstStart = v.GetEngine(), modelFirstStartCost(v)
			}
			rows = append(rows, []string{m.GetId(), engine, fmt.Sprintf("%d classes", len(m.GetLabels())), firstStart})
		}
		b.WriteString(tui.RenderTable([]string{"Model", "Engine", "Detects", "First start"}, rows))
	}
	if len(resp.GetCameras()) == 0 {
		b.WriteString("No cameras a model can watch.\n")
	} else {
		rows := make([][]string, 0, len(resp.GetCameras()))
		for _, c := range resp.GetCameras() {
			rows = append(rows, []string{c.GetSourceId(), c.GetName()})
		}
		b.WriteString(tui.RenderTable([]string{"Camera", "Name"}, rows))
	}
	fmt.Fprintf(&b, "%d of %d model slots in use\n", resp.GetRunning(), resp.GetMaxRunning())
	return b.String()
}

// modelFirstStartCost says what a first start still has to do on this device.
func modelFirstStartCost(v *agentpbv2.CatalogVariant) string {
	var parts []string
	if !v.GetImageCached() {
		parts = append(parts, "downloads its runtime image")
	}
	if n := v.GetDownloadBytes(); n > 0 {
		parts = append(parts, "downloads "+modelBytes(n)+" of model")
	}
	if v.GetNeedsEngineBuild() {
		parts = append(parts, "builds a TensorRT engine (minutes)")
	}
	if len(parts) == 0 {
		return "ready to start"
	}
	return strings.Join(parts, ", ")
}

func modelBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
}

func newDeviceModelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the models running on a device",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				resp, err := client.ListModels(cmd.Context(), &agentpbv2.ListModelsRequest{})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				if len(resp.GetInstances()) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No models running.")
					return nil
				}
				rows := make([][]string, 0, len(resp.GetInstances()))
				for _, i := range resp.GetInstances() {
					rows = append(rows, []string{i.GetInstanceId(), i.GetModelId(), i.GetCameraSourceId(),
						modelStateText(i), fmt.Sprintf("%d", i.GetWatchers()), shortModelDigest(i.GetFileSha256())})
				}
				fmt.Fprint(cmd.OutOrStdout(), tui.RenderTable([]string{"Instance", "Model", "Camera", "State", "Watchers", "File"}, rows))
				return nil
			})
		},
	}
}

func newDeviceModelStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <instance>",
		Short: "Stop a running model, whoever is watching it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
				defer cancel()
				resp, err := client.StopModel(ctx, &agentpbv2.StopModelRequest{InstanceId: args[0]})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s (%s).\n", args[0], modelStateLabel(resp.GetInstance()))
				return nil
			})
		},
	}
}

// modelStateLabel is the state and what it is doing, e.g.
// "preparing: pulling host image".
func modelStateLabel(i *agentpbv2.ModelInstance) string {
	state := strings.ToLower(strings.TrimPrefix(i.GetState().String(), "MODEL_STATE_"))
	if d := i.GetStateDetail(); d != "" {
		return state + ": " + d
	}
	return state
}

// modelStateText adds the frame rate to a ready instance's label.
func modelStateText(i *agentpbv2.ModelInstance) string {
	if i.GetState() == agentpbv2.ModelState_MODEL_STATE_READY && i.GetStats().GetProcessedFps() > 0 {
		return fmt.Sprintf("ready (%.1f fps)", i.GetStats().GetProcessedFps())
	}
	return modelStateLabel(i)
}

func shortModelDigest(sha string) string {
	if len(sha) > 12 {
		return sha[:12] + "…"
	}
	return sha
}
```

In `go/internal/cli/commands/device.go`, add `newDeviceModelCmd(),` to the `addToGroup("apps", …)` call, after `newVolumesCmd(),`.

- [ ] **Step 4: Run the tests**

Run: `go test ./go/internal/cli/commands/ -run 'TestDeviceModel|TestNewDeviceCmd'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/cli/commands/device_model.go go/internal/cli/commands/device_model_test.go go/internal/cli/commands/device.go go/internal/cli/commands/commands_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
cli: add wendy device model catalog, list and stop

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 16: CLI: `wendy device model run`

**Files:**
- Modify: `go/internal/cli/commands/device_model.go`: add `run` and register it in `newDeviceModelCmd`
- Modify: `go/internal/cli/commands/device_model_test.go`: add the fake's `StartModel` and `WatchModel`, plus the run tests

**Interfaces:**
- Consumes: Task 15.
- Produces: `newDeviceModelRunCmd()`, `runModelWatch(ctx, client, out io.Writer, modelRunOptions) error` and `modelWatchLine(*agentpbv2.ModelWatchMessage) string`.

- [ ] **Step 1: Write the failing tests**

Append to `go/internal/cli/commands/device_model_test.go`:

```go
func (f *fakeModelService) StartModel(_ context.Context, req *agentpbv2.StartModelRequest) (*agentpbv2.StartModelResponse, error) {
	f.mu.Lock()
	f.start = req
	f.mu.Unlock()
	return &agentpbv2.StartModelResponse{Instance: &agentpbv2.ModelInstance{
		InstanceId: "m-1", State: agentpbv2.ModelState_MODEL_STATE_PREPARING}}, nil
}

func (f *fakeModelService) WatchModel(req *agentpbv2.WatchModelRequest, stream grpc.ServerStreamingServer[agentpbv2.ModelWatchMessage]) error {
	f.mu.Lock()
	f.watch = req
	f.mu.Unlock()
	started := &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Started{Started: &agentpbv2.WatchStarted{
		WatchId: "w-1", Instance: &agentpbv2.ModelInstance{InstanceId: "m-1", State: agentpbv2.ModelState_MODEL_STATE_READY}}}}
	if err := stream.Send(started); err != nil {
		return err
	}
	for _, m := range f.events {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	if f.hold {
		<-stream.Context().Done()
	}
	return nil
}

func (f *fakeModelService) watched() *agentpbv2.WatchModelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watch
}

func personEntered() *agentpbv2.ModelWatchMessage {
	return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Event{Event: &agentpbv2.ModelEvent{
		Sequence: 1, Type: "entered", ClassName: "person", Confidence: 0.91, TrackId: 12, TimeUnixNanos: time.Now().UnixNano()}}}
}

func finalStatus(state agentpbv2.ModelState, detail string) *agentpbv2.ModelWatchMessage {
	return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Status{Status: &agentpbv2.ModelInstance{
		InstanceId: "m-1", State: state, StateDetail: detail}}}
}

func TestModelRunPrintsEventsAndDetaches(t *testing.T) {
	fake := &fakeModelService{events: []*agentpbv2.ModelWatchMessage{personEntered(), finalStatus(agentpbv2.ModelState_MODEL_STATE_STOPPED, "stopped")}}
	serveModels(t, fake)
	jsonOutput = false
	var out strings.Builder
	err := withModelClient(context.Background(), func(c agentpbv2.WendyModelServiceClient) error {
		return runModelWatch(context.Background(), c, &out, modelRunOptions{Model: "coco-detector", Camera: "v4l2:/dev/video0", Classes: []string{"person"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "person") || !strings.Contains(out.String(), "entered") || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("output = %q", out.String())
	}
	if got := fake.watched().GetClasses(); len(got) != 1 || got[0] != "person" {
		t.Fatalf("watched classes = %v", got)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "m-1/w-1" {
		t.Fatalf("stops = %v, want the watch detached", got)
	}
}

func TestModelRunDetachesOnInterrupt(t *testing.T) {
	fake := &fakeModelService{events: []*agentpbv2.ModelWatchMessage{personEntered()}, hold: true}
	serveModels(t, fake)
	jsonOutput = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out strings.Builder
	go func() {
		done <- withModelClient(ctx, func(c agentpbv2.WendyModelServiceClient) error {
			return runModelWatch(ctx, c, &out, modelRunOptions{Model: "coco-detector", Camera: "v4l2:/dev/video0"})
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for fake.watched() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the started message arrive
	cancel()                          // Ctrl+C
	if err := <-done; err != nil {
		t.Fatalf("an interrupted run returned %v", err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "m-1/w-1" {
		t.Fatalf("stops = %v, want the watch detached after Ctrl+C", got)
	}
}

func TestModelRunReportsFailure(t *testing.T) {
	serveModels(t, &fakeModelService{events: []*agentpbv2.ModelWatchMessage{finalStatus(agentpbv2.ModelState_MODEL_STATE_FAILED, "no frames from camera")}})
	jsonOutput = false
	err := withModelClient(context.Background(), func(c agentpbv2.WendyModelServiceClient) error {
		return runModelWatch(context.Background(), c, &strings.Builder{}, modelRunOptions{Model: "coco-detector", Camera: "v4l2:/dev/video0"})
	})
	if err == nil || !strings.Contains(err.Error(), "no frames from camera") {
		t.Fatalf("err = %v", err)
	}
}
```

Add `"time"` to the test file's imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/cli/commands/ -run TestModelRun`
Expected: FAIL to compile with `undefined: runModelWatch`.

- [ ] **Step 3: Implement `run`**

In `go/internal/cli/commands/device_model.go`:

1. Add `"io"` to the imports.
2. In `newDeviceModelCmd`, change the `AddCommand` line to:

```go
	cmd.AddCommand(newDeviceModelCatalogCmd(), newDeviceModelRunCmd(), newDeviceModelListCmd(), newDeviceModelStopCmd())
```

3. Append:

```go
func newDeviceModelRunCmd() *cobra.Command {
	var opts modelRunOptions
	cmd := &cobra.Command{
		Use:   "run <model>",
		Short: "Start a model on a camera and print what it sees until Ctrl+C",
		Example: `  wendy device model run coco-detector --camera v4l2:/dev/video0 --watch person
  wendy device model run coco-detector --camera v4l2:/dev/video0 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Camera == "" {
				return errors.New("choose a camera with --camera; `wendy device model catalog` lists them")
			}
			opts.Model = args[0]
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				return runModelWatch(cmd.Context(), client, cmd.OutOrStdout(), opts)
			})
		},
	}
	cmd.Flags().StringVar(&opts.Camera, "camera", "", "Camera source to watch, e.g. v4l2:/dev/video0")
	cmd.Flags().StringSliceVar(&opts.Classes, "watch", nil, "Only report these classes (default: all)")
	cmd.Flags().Float32Var(&opts.MinConfidence, "min-confidence", 0, "Only report detections at least this confident (0-1)")
	cmd.Flags().StringVar(&opts.Label, "label", "", "A name for this watch, shown to other clients")
	return cmd
}

type modelRunOptions struct {
	Model, Camera, Label string
	Classes              []string
	MinConfidence        float32
}

// runModelWatch starts the model, watches it until ctx ends or the model
// stops, then detaches, so the device stops the model unless someone else is
// watching.
func runModelWatch(ctx context.Context, client agentpbv2.WendyModelServiceClient, out io.Writer, opts modelRunOptions) error {
	started, err := client.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: opts.Model, CameraSourceId: opts.Camera})
	if err != nil {
		return modelServiceErr(err)
	}
	id := started.GetInstance().GetInstanceId()
	stream, err := client.WatchModel(ctx, &agentpbv2.WatchModelRequest{
		InstanceId: id, Label: opts.Label, Classes: opts.Classes, MinConfidence: opts.MinConfidence})
	if err != nil {
		return modelServiceErr(err)
	}
	var watchID, lastStatusLine string
	var last *agentpbv2.ModelInstance
	defer func() {
		if watchID == "" {
			return
		}
		// Detach on the way out, even after Ctrl+C has cancelled ctx.
		detachCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = client.StopModel(detachCtx, &agentpbv2.StopModelRequest{InstanceId: id, WatchId: watchID})
	}()
	if !jsonOutput {
		cliLogln("Watching %s on %s (instance %s). Press Ctrl+C to stop.", opts.Model, opts.Camera, id)
	}
	for {
		msg, err := stream.Recv()
		if ctx.Err() != nil {
			return nil
		}
		if err == io.EOF {
			if last.GetState() == agentpbv2.ModelState_MODEL_STATE_FAILED {
				return fmt.Errorf("the model failed: %s", last.GetStateDetail())
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("watching %s: %w", id, modelServiceErr(err))
		}
		switch {
		case msg.GetStarted() != nil:
			watchID, last = msg.GetStarted().GetWatchId(), msg.GetStarted().GetInstance()
		case msg.GetStatus() != nil:
			last = msg.GetStatus()
		}
		if jsonOutput {
			if err := encodeProtoJSON(out, msg); err != nil {
				return err
			}
			continue
		}
		line := modelWatchLine(msg)
		if msg.GetStarted() != nil || msg.GetStatus() != nil {
			if line == lastStatusLine {
				continue // heartbeats repeat the state
			}
			lastStatusLine = line
		}
		if line != "" {
			fmt.Fprintln(out, line)
		}
	}
}

// modelWatchLine renders one watch message for a person.
func modelWatchLine(msg *agentpbv2.ModelWatchMessage) string {
	switch {
	case msg.GetStarted() != nil:
		return "state: " + modelStateLabel(msg.GetStarted().GetInstance())
	case msg.GetStatus() != nil:
		return "state: " + modelStateLabel(msg.GetStatus())
	case msg.GetEvent() != nil:
		e := msg.GetEvent()
		at := time.Unix(0, e.GetTimeUnixNanos()).Format("15:04:05")
		return fmt.Sprintf("%s  %-12s %.2f %s (track %d)", at, e.GetClassName(), e.GetConfidence(), e.GetType(), e.GetTrackId())
	case msg.GetGap() != nil:
		return fmt.Sprintf("missed events %d-%d", msg.GetGap().GetFirstMissing(), msg.GetGap().GetLastMissing())
	}
	return ""
}
```

- [ ] **Step 4: Run the tests and the CLI's command tests**

Run: `go test -race ./go/internal/cli/commands/ -run 'TestModelRun|TestDeviceModel|TestNewDeviceCmd'`
Expected: PASS.
Run: `go run ./go/cmd/wendy device model run --help`
Expected: help text with `--camera`, `--watch`, `--min-confidence` and `--label`.

- [ ] **Step 5: Commit**

```bash
git add go/internal/cli/commands/device_model.go go/internal/cli/commands/device_model_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
cli: add wendy device model run

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 17: The fake model host, its workflow, and a device smoke test

**Files:**
- Create: `go/modelhost/fakehost/main.go`
- Test: `go/modelhost/fakehost/main_test.go`
- Create: `go/modelhost/fakehost/Dockerfile`
- Create: `go/modelhost/fakehost/README.md`
- Create: `.github/workflows/model-host.yml`

**Interfaces:**
- Consumes: the host contract. That is the data-socket framing (a 4-byte big-endian length followed by JSON), the acks `{"version","state","error"}`, the record names from Task 4, and `WENDY_DATA_SOCKET`, `WENDY_MODEL_VARIANT` and `WENDY_CAMERA_SOURCE` from Task 6.
- Produces: the image `ghcr.io/wendylabsinc/wendy-model-host-fake`. It reports ready, heartbeats every 5 s, and alternates a person entering and leaving every `FAKEHOST_PERIOD_SECONDS` (default 20).

- [ ] **Step 1: Write the failing test**

`go/modelhost/fakehost/main_test.go`:

```go
package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestHostSpeaksTheDataSocketContract(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	h := &host{conn: client, variant: "fakehost", source: "v4l2:/dev/video0"}
	got := make(chan record, 4)
	go func() { // the agent's side: read a frame, acknowledge it
		for {
			var prefix [4]byte
			if _, err := io.ReadFull(server, prefix[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(prefix[:]))
			if _, err := io.ReadFull(server, body); err != nil {
				return
			}
			var r record
			_ = json.Unmarshal(body, &r)
			got <- r
			ack, _ := json.Marshal(map[string]any{"version": 1, "state": "buffered"})
			binary.BigEndian.PutUint32(prefix[:], uint32(len(ack)))
			_, _ = server.Write(append(prefix[:], ack...))
		}
	}()
	stop := make(chan os.Signal)
	heartbeat := make(chan time.Time)
	period := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- h.loop(stop, heartbeat, period) }()

	if r := <-got; r.Name != "model.status" || r.Attributes["state"] != "ready" {
		t.Fatalf("first record = %+v", r)
	}
	period <- time.Now()
	if r := <-got; r.Name != "model.entered" || r.Attributes["class"] != "person" || r.Inputs[0].SampleID != 1 || r.Inputs[0].SourceID != "v4l2:/dev/video0" {
		t.Fatalf("second record = %+v", r)
	}
	heartbeat <- time.Now()
	if r := <-got; r.Name != "model.status" {
		t.Fatalf("heartbeat record = %+v", r)
	}
	period <- time.Now()
	if r := <-got; r.Name != "model.left" {
		t.Fatalf("fourth record = %+v", r)
	}
	stop <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./go/modelhost/fakehost/`
Expected: FAIL to compile with `undefined: host`.

- [ ] **Step 3: Write the fake host**

`go/modelhost/fakehost/main.go`:

```go
// Command fakehost is a stand-in model host for testing the agent's model
// service before real hosts exist. It speaks the host side of the contract in
// internal/agent/models: it reports ready, heartbeats, and reports a person
// entering and leaving on a fixed period. It never reads the camera node it
// is given.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type record struct {
	Version    int            `json:"version"`
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Model      string         `json:"model,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Inputs     []sampleRef    `json:"inputs,omitempty"`
	BootID     string         `json:"boot_id"`
}

type sampleRef struct {
	SourceID string `json:"source_id"`
	SampleID uint64 `json:"sample_id"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakehost:", err)
		os.Exit(1)
	}
}

func run() error {
	sock := os.Getenv("WENDY_DATA_SOCKET")
	if sock == "" {
		return errors.New("WENDY_DATA_SOCKET is not set")
	}
	period := 20 * time.Second
	if v := os.Getenv("FAKEHOST_PERIOD_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("FAKEHOST_PERIOD_SECONDS=%q is not a positive integer", v)
		}
		period = time.Duration(n) * time.Second
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	h := &host{conn: conn, variant: os.Getenv("WENDY_MODEL_VARIANT"), source: os.Getenv("WENDY_CAMERA_SOURCE")}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	return h.loop(stop, time.NewTicker(5*time.Second).C, time.NewTicker(period).C)
}

type host struct {
	conn    net.Conn
	variant string
	source  string
	sample  uint64
	inView  bool
}

// loop reports ready, heartbeats on every tick, and alternates a person
// entering and leaving on every period.
func (h *host) loop(stop <-chan os.Signal, heartbeat, period <-chan time.Time) error {
	if err := h.status("ready"); err != nil {
		return err
	}
	for {
		select {
		case <-stop:
			return nil
		case <-heartbeat:
			if err := h.status("ready"); err != nil {
				return err
			}
		case <-period:
			h.inView = !h.inView
			name := "model.left"
			if h.inView {
				name = "model.entered"
			}
			if err := h.detection(name); err != nil {
				return err
			}
		}
	}
}

func (h *host) status(state string) error {
	return h.send(record{Version: 1, Type: "event", Name: "model.status", BootID: "unavailable",
		Attributes: map[string]any{"state": state, "processed_fps": 10.0, "latency_p50_ms": 1.0}})
}

func (h *host) detection(name string) error {
	h.sample++
	return h.send(record{Version: 1, Type: "event", Name: name, Model: h.variant, BootID: "unavailable",
		Attributes: map[string]any{"class": "person", "confidence": 0.9, "track_id": 1,
			"box": map[string]any{"x": 0.4, "y": 0.2, "width": 0.2, "height": 0.6}},
		Inputs: []sampleRef{{SourceID: h.source, SampleID: h.sample}}})
}

// send writes one length-prefixed record and reads the agent's
// acknowledgement, framed as internal/agent/services/app_data_socket.go does.
func (h *host) send(r record) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if _, err := h.conn.Write(append(prefix[:], body...)); err != nil {
		return err
	}
	if _, err := io.ReadFull(h.conn, prefix[:]); err != nil {
		return err
	}
	ack := make([]byte, binary.BigEndian.Uint32(prefix[:]))
	if _, err := io.ReadFull(h.conn, ack); err != nil {
		return err
	}
	var parsed struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(ack, &parsed); err != nil {
		return err
	}
	if parsed.State == "rejected" {
		return fmt.Errorf("the agent rejected %s: %s", r.Name, parsed.Error)
	}
	return nil
}
```

`go/modelhost/fakehost/Dockerfile`, built from the repo root with BuildKit (`docker buildx`). The build stage runs on the build machine and cross-compiles, so an arm64 image builds on an x86 host without QEMU:

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY go/modelhost/fakehost ./go/modelhost/fakehost
RUN go test ./go/modelhost/fakehost/... && CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -o /out/fakehost ./go/modelhost/fakehost

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fakehost /usr/local/bin/fakehost
ENTRYPOINT ["/usr/local/bin/fakehost"]
```

`.github/workflows/model-host.yml`:

```yaml
name: Model hosts

on:
  pull_request:
    paths:
      - 'go/modelhost/**'
      - '.github/workflows/model-host.yml'
  workflow_dispatch:
    inputs:
      publish:
        description: Publish the fake model host for development catalogs
        type: boolean
        default: false

permissions:
  contents: read

jobs:
  fakehost:
    runs-on: ubuntu-24.04-arm
    timeout-minutes: 20
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0
      - name: Test and build
        run: docker build -f go/modelhost/fakehost/Dockerfile -t fakehost:test .
      - name: Publish
        if: github.event_name == 'workflow_dispatch' && inputs.publish
        env:
          REGISTRY_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          IMAGE: ghcr.io/wendylabsinc/wendy-model-host-fake:dev
        run: |
          printf '%s' "$REGISTRY_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
          docker tag fakehost:test "$IMAGE"
          docker push "$IMAGE"
          docker buildx imagetools inspect "$IMAGE" --format '{{json .Manifest.Digest}}'
```

`go/modelhost/fakehost/README.md`:

````markdown
# fakehost

A stand-in model host for the agent's model service
(`specs/2026-09-25-model-watch-design.md`). It speaks the host side of the
contract in `go/internal/agent/models`:

- It reports `ready`, then sends a `model.status` heartbeat every 5 s.
- It reports a `person` entering, then leaving, every `FAKEHOST_PERIOD_SECONDS`
  (default 20).
- It never reads the camera node it is given, so it tests the agent's model
  service, not perception.

Once published, reference it from a development catalog by digest, and point
the agent at that catalog with `WENDY_MODEL_CATALOG_FILE`. The smoke test in
`specs/2026-09-25-model-watch-plan-2-agent-service.md` (Task 17) walks through it.
````

- [ ] **Step 4: Run the test and build the image locally**

Run: `go test ./go/modelhost/fakehost/`
Expected: PASS.
Prerequisite: Docker with the buildx plugin, which the Dockerfile's `$BUILDPLATFORM` needs. The development machine this plan was written on lacked it; on Arch or CachyOS, install it with `sudo pacman -S docker-buildx`.
Run: `docker buildx build --load -f go/modelhost/fakehost/Dockerfile -t fakehost:test .`
Expected: the build succeeds, and the `go test` line inside it passes.

- [ ] **Step 5: Commit**

```bash
git add go/modelhost/fakehost .github/workflows/model-host.yml
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
modelhost: add a fake model host for testing the model service

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

- [ ] **Step 6: Smoke-test on a device**

This step is manual and needs a person. Use a Jetson with a USB camera, since the two-plane path is proven there (`Examples/WendyDataModelApp`). Record the outcome in the PR description.

1. **Publish the fake host for this branch.** The workflow can only be dispatched once it is on the default branch.
   ```bash
   echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin   # a token with write:packages
   docker buildx build --platform linux/arm64,linux/amd64 -f go/modelhost/fakehost/Dockerfile \
     -t ghcr.io/wendylabsinc/wendy-model-host-fake:m2-smoke --push .
   DIGEST=$(docker buildx imagetools inspect ghcr.io/wendylabsinc/wendy-model-host-fake:m2-smoke --format '{{json .Manifest.Digest}}' | tr -d '"')
   echo "$DIGEST"
   ```
   If the package is new, make it public once, as the ROS 2 inspector image is: GitHub → wendylabsinc → Packages → wendy-model-host-fake → Package settings → Change visibility → Public.

2. **Write the development catalog.** The model file is this repo's `LICENSE` at a pinned commit; the fake host never reads it.
   ```bash
   cat > /tmp/model-catalog.dev.json <<EOF
   {
     "version": "dev-fakehost",
     "models": [{
       "id": "fake-detector",
       "description": "Reports a person entering and leaving every 20 s; reads no frames",
       "kind": "detector",
       "labels": ["person"],
       "variants": [{
         "id": "fakehost",
         "engine": "onnxruntime",
         "requires": {},
         "host_image": "ghcr.io/wendylabsinc/wendy-model-host-fake@${DIGEST}",
         "file": {
           "url": "https://raw.githubusercontent.com/wendylabsinc/WendyOS/f36f361afec38d064f383203fb75b85f9411a2b0/LICENSE",
           "sha256": "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4",
           "bytes": 11357
         },
         "input_size": 1
       }]
     }]
   }
   EOF
   ```

3. **Install this branch's agent with the catalog.**
   ```bash
   (cd go && make build-agent-linux-arm64)
   wendy device shell --device <jetson>
   ```
   In that shell:
   1. Create `/var/lib/wendy/models/catalog.dev.json`: run `cat > /var/lib/wendy/models/catalog.dev.json`, paste the file from step 2, then press Ctrl+D.
   2. Add a systemd drop-in that sets `Environment=WENDY_MODEL_CATALOG_FILE=/var/lib/wendy/models/catalog.dev.json`. The agent unit is `edge-agent.service` on WendyOS images and `wendy-agent.service` from the Debian and Arch packages. Use `systemctl edit <unit>`.
   3. Exit the shell.

   Then run:
   ```bash
   wendy device update --binary go/bin/wendy-agent-linux-arm64 --device <jetson>
   ```

4. **Exercise the service**, using the CLI built from this branch:
   ```bash
   go run ./go/cmd/wendy device model catalog --device <jetson>
   go run ./go/cmd/wendy device model run fake-detector --camera v4l2:/dev/video0 --device <jetson>
   ```
   Expected:
   - `catalog` lists `fake-detector` and the camera.
   - `run` prints `state: preparing: pulling host image`, then `state: ready`, then a `person … entered` line about 20 s later and `left` about 20 s after that.
   - In a second terminal, `wendy device model list --device <jetson>` shows the instance as `ready (10.0 fps)` with 1 watcher.

5. **Check cleanup.** Kill the `run` process with `kill -9` so it cannot detach, then time how long the instance takes to go. `wendy device model list` must show nothing within 90 s. After that, `wendy device shell` followed by `ctr -n default containers ls | grep wendy-model-` must print nothing.

6. **Check the boot cleanup.** Start `run` again, and restart the agent with `systemctl restart <unit>` in a device shell. The `run` stream ends. Within a few seconds of the agent coming back, `ctr -n default containers ls | grep wendy-model-` prints nothing.

7. **Remove the drop-in** and restart the agent, so the device returns to the built-in (empty) catalog.

---

## Self-Review Notes

- **Spec coverage.** Each part of the spec that M2 owns has a task:

  | Spec section | Task |
  |---|---|
  | §5 contract | 2 |
  | §6.2 environment | 6: `HostSpec.Env` |
  | §6.4 catalog rules | 3 |
  | §7.2 steps 1–7 | 6, 9, 12 |
  | §7.3 leases, restarts, cleanup | 7, 9 |
  | §7.4 demand | 10 |
  | §7.5 tap | 11, 8 |
  | §8.1 CLI | 15, 16 |
  | §9 rows owned by the agent | 6–9, 12, 13 |
  | §10 container lockdown | 12 |
  | §10 no free-form artifacts | 3, 14 |
  | §11 agent tests | throughout |
- **Deferred elsewhere.** The model host itself (§6.1) is milestone M1; the TensorRT and QNN images are M4; the MCP tools and chat (§8.2, §8.3) are M3.
- **Placeholders.** None. The image digest in Task 17 comes from the command that prints it. `<jetson>` and `<unit>` in the manual smoke test name the tester's own device and its agent unit.
- **Name consistency.** Names are checked across tasks:
  - `Supervisor.Stop(ctx, instanceID, watchID)` keeps one signature from Task 6 on.
  - `Watch` returns `(*Watch, InstanceInfo, uint64, error)` in Tasks 7, 8 and 13.
  - `modelStateLabel` and `modelStateText` are defined in Task 15 and used in Task 16.
  - `resolveModelCamera` and `modelHostVideoGID` are defined in Task 12 and used in its test.
