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

### Tasks 9–17

Tasks 9–17 and the self-review notes continue in `specs/2026-09-25-model-watch-plan-2-agent-service-part-2.md`.
