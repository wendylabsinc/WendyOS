# Model watch: `wendy chat` starts a detector on a device camera and hears what it sees

**Date:** 2026-09-25
**Status:** Design (approved in brainstorming; not yet planned)
**Linear:** project P-WDY-258 "Wendy Chat: Let LLM spawn 'any model'" (initiatives I-129 "Wendy should offer runtime options for AI Models" and I-119 "Wendy Chat")
**Builds on:** the two-plane camera path (`go/internal/agent/services/hub_loopback_pump.go`, `Examples/WendyDataModelApp`), the app data socket (`go/internal/agent/services/app_data_socket.go`), the agent-managed ROS 2 inspector (`go/internal/agent/containerd/ros2_host.go`), and the MCP bridge in `wendy chat` (`go/internal/cli/chat/mcp.go`)
**Scope:** The first slice of P-WDY-258. With one tool call, the chat model starts a catalog detector on a camera attached to the device. The device runs it in a container the agent owns, and the pipeline around the network is written in Mojo. When the detector sees what the user asked about, the open chat is told.

## 1. Summary

P-WDY-258 asks for WendyOS and the Wendy Chat agent to run models (VLMs, VLAs, perception networks, TensorRT engines, RL policies and more) as easily as calling an MCP tool. It should be built on Mojo, so that it runs on any inference device. That covers several subsystems. This spec covers the first slice:

> "Tell me when someone comes to the door." The chat model starts a person detector on the device's front-door camera. When someone arrives, Wendy says so in the open chat without being asked.

This slice was chosen because it is the project's first sentence ("allow a Wendy Chat LLM agent to observe its environment"), it moves nothing physical, and it makes every layer exist once.

Models already run on devices today, but only as ordinary apps that the platform cannot tell apart from any other container. The chat model can look at single camera snapshots with its own vision. Nothing watches continuously on its behalf. This slice adds:

- `WendyModelService` in the Go agent. It starts catalog models in containers the agent owns, keeps each alive while someone watches it, and streams its events.
- A model host whose pipeline (preprocessing, NMS, tracking, event rules) is Mojo. The network runs on the best engine for each device: TensorRT on Jetson, ONNX Runtime on a Raspberry Pi 5, and QNN on Qualcomm.
- `wendy device model …` commands and `model_*` MCP tools.
- Chat plumbing that turns pushed events into turns.

## 2. Where this fits in P-WDY-258

| # | Sub-project | In this slice |
|---|---|---|
| 1 | **Model contract:** what a model is to Wendy (kind, files, inputs, outputs, accelerator needs) | Internal only: a catalog entry plus the host's environment contract (§6.2). A public manifest arrives with bring-your-own models. |
| 2 | **Model host:** runs a model on the right engine, connected to sensors | A detector host for three engines (§6) |
| 3 | **Control surface:** agent API, CLI, MCP tools, chat skills | `WendyModelService`, `wendy device model`, `model_*` tools (§5, §8) |
| 4 | **Observation loop:** chat reads observations, and events start turns | Pushed into the open chat; polled everywhere else (§8) |
| 5 | **Catalog and hardware matrix** | One detector on five devices (§6.4, §11) |
| 6 | **Policies that drive motors** (RL, ACT, VLA, diffusion policies) | Not in this slice. Needs the `motion` entitlement (WDY-3128) and supervised motion (WDY-3133). |

Later slices already named:

- an on-device VLM for "ask about the scene";
- watches that outlive chat (Companion notifications, `wendy agent serve`);
- bring-your-own models;
- a MAX engine adapter;
- the Mac agent;
- x86 devices;
- IP, ROS 2 and remote-mounted cameras;
- moving campaign inference (`go/internal/agent/inference`) onto this service.

Related issues: artifact identity (WDY-3131) and `wendy artifacts fetch` (WDY-3139). This slice follows their shape: files are addressed by content, and every engine an agent builds records which file it came from (§6.3). Mojo and MAX with Qualcomm (WDY-2558). The Mojo vision demo (WDY-940), whose preprocessing and NMS kernels could share code with §6.1.

## 3. Goals and non-goals

**Goals**

- From `wendy chat`, one approved tool call starts a catalog detector on a named local camera of the connected device.
- Detections that match the watch reach the open chat and start a turn by themselves, and the chat model tells the user.
- Works on:
  - Jetson Orin Nano, AGX Orin and Thor (TensorRT);
  - Raspberry Pi 5 (ONNX Runtime on the CPU);
  - the Qualcomm board the `npu` entitlement was built on (QNN on the Hexagon NPU; see WDY-3052).
- Preprocessing, postprocessing, tracking and event rules are Mojo 1.0 code, shared by all three engines.
- A watch belongs to the chat session that started it. If chat or the laptop dies, the model stops and the camera and accelerator are released.
- Frames never leave the device.

**Success criteria** (measured by the hardware validation in §11)

- **Alert latency:** from a person entering the frame to the event line appearing in the chat transcript takes at most 2 s on Jetson and Qualcomm and at most 3 s on the Pi 5, over the LAN. The chat model's reply time belongs to the provider and is not counted.
- **Warm start:** with image, model file and engine already cached, `StartModel` reaches `READY` in at most 15 s.
- **Throughput:** at least 10 processed frames per second on every target.
- **Cleanup:** after chat is killed, the model container is removed within 90 s.
- **Correctness:** a scripted scene (a person walks in, stands, walks out, twice) yields exactly two `entered` and two `left` events on every target.

**Non-goals (this slice)**

- VLMs, VLAs, policies, or actuation of any kind.
- Models outside the catalog, and files or URLs supplied by users or the chat model.
- Watches that outlive the chat session, fleet-wide watches, and alerts to the Companion app.
- The Mac agent, x86 devices, and cameras other than local V4L2 ones (USB, CSI).
- A MAX engine adapter. The engine interface allows one; it lands once MAX is proven on a target.
- Scheduling around GPU memory. This slice has only a fixed limit of two models per device.

## 4. Architecture

```
wendy chat (TUI) ── MCP (stdio) ──▶ wendy mcp serve ── gRPC / mTLS ──▶ wendy-agent (device)
  event inbox ◀──── notifications ───┘  model_* tools                   WendyModelService
                                         holds one WatchModel             ├─ catalog (built in)
                                         stream per watch                 ├─ supervisor ──▶ model-host container
                                                                          │   camera ─ two-plane node ─┘ (Mojo + engine)
                                                                          └─ event tap ◀── app data socket ──┘
```

1. **Catalog.** Built into the agent (§6.4). Each entry lists class labels and a variant per platform. A variant is a host image pinned by digest, a model file URL with its sha256, and an engine. The agent picks the variant from its own device info.
2. **`WendyModelService`.** A new v2 gRPC service (§5). It is registered on the mTLS server (next to Shell, Tunnel and Driver) and on the admin socket, not on the plaintext server that runs before provisioning.
3. **Model supervisor.** Pulls the host image, fetches and verifies the model file, and creates and removes the host container. It also enforces admission limits and leases, restarts crashed hosts, and cleans up at boot (§7).
4. **Model host.** One container image per engine, running a compiled Mojo program (§6).
5. **Event tap.** A non-blocking fan-out per app, added where the app data socket accepts records (§7.5).
6. **CLI and MCP.** `wendy device model catalog|run|list|stop`, and five `model_*` tools (§8.1, §8.2).
7. **Chat.** `wendy mcp serve` holds one `WatchModel` stream per watch and forwards events as MCP notifications. Chat queues them and turns them into turns (§8.3).

## 5. The `WendyModelService` contract

The service lives in a new file, `Proto/wendy/agent/services/v2/model_service.proto`, in package `wendy.agent.services.v2` with `go_package …/agentpb/v2;agentpbv2`. It is added to `V2_AGENT_PROTOS` in `go/scripts/generate-proto.sh`. It is not added to the Swift proto list in this slice.

```proto
service WendyModelService {
  rpc ListCatalog(ListModelCatalogRequest) returns (ListModelCatalogResponse);
  rpc StartModel(StartModelRequest) returns (StartModelResponse);
  rpc WatchModel(WatchModelRequest) returns (stream ModelWatchMessage);
  rpc ListModels(ListModelsRequest) returns (ListModelsResponse);
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
  repeated string labels = 4;         // class names
  CatalogVariant variant = 5;         // chosen for this device; unset if none fits
  string unavailable_reason = 6;      // set when variant is unset
}
message CatalogVariant {
  string id = 1;                      // e.g. "yolox-s-tensorrt"
  string engine = 2;                  // "tensorrt" | "onnxruntime" | "qnn"
  uint64 download_bytes = 3;          // 0 when the image and file are cached
  bool needs_engine_build = 4;        // no cached engine for this device yet
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
  bool reused = 2;                    // an identical instance was already running
}

message ModelInstance {
  string instance_id = 1;
  string model_id = 2;
  string variant_id = 3;
  string engine = 4;
  string camera_source_id = 5;
  ModelState state = 6;
  string state_detail = 7;            // "building TensorRT engine", a failure reason, …
  uint32 watchers = 8;
  repeated string watch_labels = 9;
  google.protobuf.Timestamp started_at = 10;
  ModelStats stats = 11;              // from the latest heartbeat
  string file_sha256 = 12;            // what is running (WDY-3131 identity)
}
enum ModelState {
  MODEL_STATE_UNSPECIFIED = 0;
  MODEL_STATE_PREPARING = 1;          // pulling, downloading, waiting for or building an engine
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

message WatchModelRequest {
  string instance_id = 1;
  string label = 2;                   // "front door"
  repeated string classes = 3;        // empty = all
  float min_confidence = 4;           // 0 = the host's floor
  repeated string event_types = 5;    // "entered", "left"; empty = both
  uint64 after_sequence = 6;          // replay ring entries after this sequence
}
message ModelWatchMessage {
  oneof message {
    WatchStarted started = 1;         // always first
    ModelInstance status = 2;         // state changes and heartbeats
    ModelEvent event = 3;
    ModelGap gap = 4;                 // requested events already left the ring
  }
}
message WatchStarted {
  string watch_id = 1;
  ModelInstance instance = 2;
}
message ModelEvent {
  uint64 sequence = 1;                // per instance, assigned by the agent
  string type = 2;                    // "entered" | "left"
  string class_name = 3;
  float confidence = 4;
  uint64 track_id = 5;
  BoundingBox box = 6;                // normalized to the frame, 0..1
  string source_id = 7;
  uint64 sample_id = 8;               // two-plane identity of the triggering frame
  google.protobuf.Timestamp time = 9;
}
message BoundingBox { float x = 1; float y = 2; float width = 3; float height = 4; }
message ModelGap { uint64 first_missing = 1; uint64 last_missing = 2; }

message ListModelsRequest {}
message ListModelsResponse { repeated ModelInstance instances = 1; }

message StopModelRequest {
  string instance_id = 1;
  string watch_id = 2;                // detach this watch; empty = stop the instance outright
}
message StopModelResponse { ModelInstance instance = 1; }
```

**Semantics**

- **`StartModel`** validates the request and returns at once. The state is `PREPARING`, or the current state when the instance is reused. A request for the same `model_id` on the same `camera_source_id` reuses the running instance (`reused: true`).
- **Leases.** A model lives while it has watchers:
  - An instance runs while at least one `WatchModel` stream is attached.
  - If the last stream ends without `StopModel` (a client crash or a network loss), a 60 s grace period starts. A new `WatchModel` within the grace cancels it, and `after_sequence` replays what was missed.
  - `StopModel{instance_id, watch_id}` detaches that watch. If no watches remain, the instance stops at once, with no grace.
  - `StopModel{instance_id}` without a `watch_id` stops the instance outright, and ends every watch with a final `status` of `STOPPED`.
  - An instance that no watch attaches to within 60 s of `StartModel` stops.
- **`WatchModel` messages.** `WatchStarted` comes first. Then `status` arrives on every state change and heartbeat (at most one every 5 s), `event` for each event that passes the filter, and `gap` when `after_sequence` is older than the ring. The ring holds the last 100 events per instance.
- **Filters** apply in the agent, per watch. Several watches can therefore share one instance with different classes and thresholds.
- **Errors from calls** use gRPC codes:
  - `NOT_FOUND`: unknown model, camera or instance.
  - `FAILED_PRECONDITION`: no variant for this device, or the camera cannot stream to a model.
  - `RESOURCE_EXHAUSTED`: two instances are already running.
  - `INVALID_ARGUMENT`: a class that is not in the model's labels.
- **Errors after `StartModel` returns** arrive on the watch as `status` with `FAILED` and a `state_detail` (§9).

## 6. Model host

### 6.1 Program

The host is one Mojo program compiled with `mojo build` against the `mojo==1.0.x` pip package. It follows `Examples/mojo/simple-web-server`: a Mojo entry point that uses Python modules through `std.python`. Each processed frame goes through these stages:

1. **Capture** (a Python module called through interop). Reads the agent-fed two-plane node with V4L2 and recovers the hub's `sample_id` from the buffer timestamp, as `Examples/WendyDataModelApp/wendyframes.py` does. Decodes with PyAV. The decoder follows each sample's encoding. At most 10 frames per second are processed. Other frames are decoded only as far as an inter-frame codec requires, then skipped.
2. **Preprocess** (Mojo). Letterbox to the model's input size, convert colour, normalize, and lay out as NCHW, using SIMD on the CPU.
3. **Infer** (engine adapter, §6.3).
4. **Postprocess** (Mojo). Decode boxes and class scores, drop anything below the confidence floor of 0.30, and run class-wise NMS at IoU 0.45.
5. **Track** (Mojo). Greedy IoU matching per class across processed frames, at IoU ≥ 0.3.
6. **Event rules** (Mojo). A track is confirmed once it appears in 3 of the last 5 processed frames. Confirmation emits `entered` with the track's best confidence so far. A confirmed track unseen for 3 s emits `left`.
7. **Emit** (Mojo). Length-prefixed JSON records on `WENDY_DATA_SOCKET`, using the existing framing (`app_data_socket.go`, `readDataFrame` / `writeDataFrame`).

The host sends three kinds of `event` record over the data socket:

- **`model.entered` and `model.left`.** `Model` is the variant id. `Attributes` holds `class`, `confidence`, `track_id` and `box` (normalized x, y, width, height). `Inputs` is `[{source_id, sample_id}]` for the frame that triggered the event.
- **`model.status`.** Sent every 5 s and on every state change. It carries `state` (`building_engine`, `ready` or `failed`), `reason` when failed, processed fps, p50 latency per stage, and frames skipped.

No `prediction` records are sent in this slice.

If no frame arrives for 10 s, the host fails with reason "no frames from camera". This covers an unplugged camera, and a camera another process holds so the agent's producer cannot open it. On any unrecoverable error, the host sends a final `model.status` with `state: failed` and a reason, then exits with a non-zero status.

### 6.2 Environment contract (agent to host)

| Variable | Meaning |
|---|---|
| `WENDY_MODEL_INSTANCE` | Instance id |
| `WENDY_MODEL_VARIANT` | Catalog variant id |
| `WENDY_MODEL_FILE` | Model file, read-only (ONNX, or whatever spike S2 selects for QNN) |
| `WENDY_MODEL_LABELS` | Class names, read-only, one per line |
| `WENDY_MODEL_ENGINE_CACHE` | TensorRT only: read-write directory for the built engine |
| `WENDY_CAMERA_NODE` | The two-plane node for the chosen camera |
| `WENDY_CAMERA_SOURCE` | Canonical source id, e.g. `v4l2:/dev/video0` |
| `WENDY_DATA_SOCKET` | `/run/wendy/data/data.sock` |
| `WENDY_MODEL_MAX_FPS` | `10` |
| `WENDY_MODEL_CONFIDENCE_FLOOR` | `0.30` |

### 6.3 Engines and images

All images are `linux/arm64`. They are published to `ghcr.io/wendylabsinc/wendy-model-host-<target>` and referenced by digest from the catalog.

| Target | Image | Engine adapter | First start |
|---|---|---|---|
| Jetson Orin Nano, AGX Orin, Thor | `wendy-model-host-jetson` | TensorRT Python API, with TensorRT and CUDA libraries from the device through CDI | Builds the engine from ONNX into `WENDY_MODEL_ENGINE_CACHE`, reporting `building_engine`. Times out after 15 min. |
| Raspberry Pi 5 | `wendy-model-host-cpu` | ONNX Runtime (CPU) Python API | Download only |
| Qualcomm | `wendy-model-host-qualcomm` | QNN, using the runtime the `npu` entitlement code mounts | Download only (spike S2 picks the file format) |

The adapter interface is `load(model_file, cache_dir) -> Engine`, `Engine.input_shape()` and `Engine.infer(tensor) -> [tensor]`. Adapters are Python modules called from Mojo. A MAX adapter would be a fourth implementation of the same interface.

**Engine cache (TensorRT).** Built engines are stored at `/var/lib/wendy/models/engines/<file-sha256>/<gpu_arch>-trt<version>.plan`. A sidecar `.json` next to each records `{from: <file sha256>, gpu_arch, tensorrt_version, built_at}`: the `from` provenance proposed in WDY-3131. The supervisor mounts only that file's engine directory read-write.

### 6.4 Catalog

- **Where it lives.** The catalog is built into the agent as `go/internal/agent/models/catalog.json` and versioned with the agent. A client can name only a catalog id and a camera source id, never an image or a URL. This is the same rule `chat.ModelSpec` applies to endpoints and credentials.
- **Entry shape.** An entry has `id`, `description`, `kind: detector`, `labels` and `variants[]`. A variant has:
  - `id` and `engine`;
  - `requires`, predicates over device info (`gpu_vendor`, `compute_backends`, `npu_backends`, `arch`);
  - `host_image`, a reference pinned by digest;
  - `file {url, sha256, bytes}` and `input_size`.
- **Variant selection.** The first variant, in catalog order, whose `requires` all hold. Catalog order is QNN, then TensorRT, then CPU.
- **Slice 1's only entry is `coco-detector`,** an Apache-2.0 COCO detector that spike S1 chooses from YOLOX, RTMDet and RF-DETR. The size may differ per variant (for example, a smaller one on the Pi), and the variant id records which. Ultralytics YOLO is excluded because AGPL-3.0 weights should not ship in a platform catalog. The `Examples/WendyDataModelApp` example may keep using it.
- **Model files** are served from a new public bucket, `gs://wendy-models-public`, at `https://storage.googleapis.com/wendy-models-public/sha256/<digest>`. This follows the existing public buckets (`wendyos-images-public`, `wendy-install-public`). Paths are content-addressed, so the file at a URL never changes.

## 7. Agent

### 7.1 Layout

- `go/internal/agent/models/`: catalog, variant selection, file cache, supervisor, lease and grace logic, event ring and filters.
- `go/internal/agent/services/model_service.go`: the gRPC handlers.
- `go/internal/agent/containerd/model_host.go`: container operations, next to `ros2_host.go`.

### 7.2 Starting an instance

1. **Validate.** The model id is known. A variant fits this device. The camera is a `v4l2:` source listed by `DataService.Sources` that the two-plane path can bind. Fewer than two instances are running. If an identical instance is already running, reuse it.
2. **Pull the image.** If `host_image` is absent, pull it: `GetImage`, falling back to `Pull(WithPullUnpack)`, as in `ros2_host.go`.
3. **Fetch the model file** into `/var/lib/wendy/models/files/sha256/<digest>`. Download to a temporary file, verify the sha256, then rename. On a mismatch, delete the temporary file and fail; it is never mounted.
4. **Resolve the camera node.** Mark two-plane demand for the camera and resolve its node with `TwoPlaneNodePath`.
5. **Open the data socket.** Ensure the app data socket for app id `sh.wendy.model.<instance>` with `AppDataSocketManager.Ensure`.
6. **Create the container** `wendy-model-<instance>`:
   - **Labels:** `sh.wendy/model.instance`, `sh.wendy/model.id`, `sh.wendy/model.variant` and `sh.wendy/model.file.sha256`. There is no `sh.wendy/app.version` label, so app lists, stats and `ContainerMonitor` skip the container, as they skip the ROS 2 inspector.
   - **Cgroup scope:** named so the data socket's peer check (`appIDFromCgroup`) attributes the process to `sh.wendy.model.<instance>`.
   - **Devices:** the one camera node, plus either the GPU nodes and CDI edits (Jetson) or the NPU nodes and QNN runtime mounts (Qualcomm). These reuse the entitlement code (`applyGPU`, `applyNvidiaCDI`, `applyNPU`, `cdi.ApplyQualcommNPURuntime`).
   - **Mounts:** the model file and labels read-only, the engine cache read-write (TensorRT only), and the data-socket directory read-only.
   - **Isolation:** no network, a read-only root filesystem and a tmpfs `/tmp`. The process runs as non-root, with the device groups its nodes need. It runs as root only if spike S3 shows TensorRT requires it.
7. **Start the task.** The state becomes `STARTING`, then `READY` on the first `model.status` with `state: ready`.

Only one engine build runs at a time on a device. A second start that needs a build waits in `PREPARING` with the detail "waiting for another engine build".

### 7.3 Leases, restarts and cleanup

- **Leases.** Lease and grace follow §5. The logic takes an injectable clock.
- **Restarts.** A host that exits, or sends no `model.status` for 15 s, is restarted with backoff: 2 s, then 8 s, then 30 s. Watchers see `RESTARTING`. A fourth failure sets `FAILED`, removes the container and ends the watches.
- **Camera loss.** The host's "no frames from camera" failure (§6.1) is not restarted. It sets `FAILED` and ends the watches straight away, because a restart cannot bring a camera back.
- **Removal.** Kill the task (`terminateTask`), delete the container with snapshot cleanup, and release the two-plane demand and the data socket.
- **Boot.** Before serving, delete every container labeled `sh.wendy/model.instance`. Leases do not survive an agent restart.

### 7.4 Two-plane demand

Today `SyncCameraLoopbacks` (`containerd/camera_wiring.go`) creates two-plane nodes when any running app container has the camera entitlement. It also counts running model hosts.

### 7.5 Event tap

- **Publishing.** In `serveConn` (`app_data_socket.go`), each record that `RecordApplication` accepts is also published as `(appID, record)` to a tap. The tap fans out without blocking and drops records for a slow subscriber, as `TelemetryBroadcaster.PublishLogs` does.
- **Consuming.** The supervisor subscribes to app ids under `sh.wendy.model.`. It converts `model.entered` and `model.left` into `ModelEvent`s, with the agent assigning `sequence`. It keeps the last 100 events per instance and applies each watch's filter. `model.status` records update the instance state.
- **Unchanged paths.** The existing single-slot `appObserver` (campaign triggers) is untouched. Records still go into open episodes as they do today, which records useful provenance.

## 8. CLI, MCP and chat

### 8.1 CLI

`go/internal/cli/commands/device_model.go` adds:

- `wendy device model catalog`: what this device can run, and its cameras.
- `wendy device model run <model> --camera <source> [--watch person,car] [--min-confidence 0.6]`: starts a model and watches it in the foreground. Events print as they arrive, one JSON object per line with `--json`. Ctrl+C detaches the watch.
- `wendy device model list`.
- `wendy device model stop <instance>`: stops the instance outright.

`grpcclient.AgentConnection` gains a `ModelService` client.

### 8.2 MCP tools (`go/internal/cli/mcp/tools_model.go`)

| Tool | Annotation | Parameters | Returns |
|---|---|---|---|
| `model_catalog` | read-only | none | Models this device can run (id, description, labels, engine, first-start cost), cameras, free slots |
| `model_start` | mutating, so needs approval | `model` (catalog id), `camera` (source id), `watch` (1–10 labels of that model), `min_confidence` (0.3–0.95, default 0.5), `label` (optional, defaults to the camera name) | `watch_id`, `instance_id`, state, `reused`, engine |
| `model_list` | read-only | none | Instances on the device, with this session's watches marked |
| `model_stop` | mutating, so needs approval | `watch_id` | That the watch ended, and whether the model is still running for other watchers |
| `model_events` | read-only | `watch_id`, optional `after_sequence`, `wait_seconds` (0–120) | Events after the sequence from this session's buffer (the last 100 per watch), and the current state |

- **`model_start`** opens the `WatchModel` stream with `event_types` set to both and the requested classes and confidence. It returns when the instance is `READY`, when it fails, or after 30 s, in which case the state is `PREPARING`. A later `READY` or `FAILED` arrives as a notification.
- **`model_stop`** detaches this session's watch only. Stopping an instance outright is left to `wendy device model stop`.
- **Notifications.** The server keeps `srv` (today a local in `Start`) and sends two custom notifications to the stdio session with `SendNotificationToSpecificClient("stdio", …)`:
  - `notifications/wendy/model_event`, with params `{instance_id, watch_id, label, event}`;
  - `notifications/wendy/model_status`, with params `{instance_id, watch_id, label, status}`, sent on watch start and end, on state changes, and for gaps.

  mcp-go queues notifications in a 100-slot channel and drops them when it is full. The sequence numbers expose such a gap.
- **Reconnects.** When a stream breaks, the server re-attaches within the grace period using `after_sequence`. A `gap`, or a failed re-attach, is forwarded as a status notification.
- **Other MCP clients** such as Claude Code or Cursor get the same tools. They ignore the custom notifications and wait with `model_events`.
- **Housekeeping.** The tools are added to the `wendy://guide` tool list (`mcp/tools_guide.go`) and the server's registration tests.

### 8.3 Chat

- **Receiving notifications (`chat/mcp.go`).** `startMCP` registers `OnNotification` on the concrete client. The handler parses the two methods and does a non-blocking send into an inbox owned by the session. The inbox holds 64 items; overflow is counted and reported with the next delivered item. The handler must not block, because it runs on the stdio reader goroutine that also delivers tool results.
- **The inbox belongs to the session.** `chat.Session` exposes it, and `commands/chat.go` passes it to `UIOptions`. Events that arrive while a setup screen is showing are therefore kept.
- **TUI (`chat/tui.go`).**
  - A wait command on the inbox is re-armed after each message.
  - Every item is shown at once as a compact `event` transcript entry: `· 14:02:11  front door  person 0.91 entered`.
  - Some items start a turn:
    - `entered` events;
    - statuses that end a watch (`FAILED`, `STOPPED`, "device restarted");
    - a `READY` that follows a `model_start` which returned `PREPARING`, so the chat model can say the watch is now active.

    A turn starts when chat is idle and at least 10 s have passed since the last event turn. Otherwise the item waits in a typed event queue, kept separate from `queuedPrompts`. Queued items are merged into one turn when the running turn ends and the 10 s window has passed.
  - `left` events and gap reports are shown but never start a turn by themselves. They are included in the next event turn's prompt.
  - `/watches` lists this session's watches. `/watches stop all` stops them through the MCP server directly, without the chat model.
  - The status bar shows `watching: N` while N watches are active.
- **Event turn prompt.** Built from the watch and the event, with the JSON escaped as a string inside the tags, as `agentservice.sensorEventPrompt` does. That helper moves to a shared place.

  ```
  Your watch "front door" (coco-detector, v4l2:/dev/video0) reported:
  <untrusted_sensor_event_json>
  "{…escaped event JSON…}"
  </untrusted_sensor_event_json>
  Tell the user if this is what they asked to be alerted about.
  ```

  A merged turn uses one block holding a JSON array of the events.
- **Engine (`chat/engine.go`).** Event turns skip memory recall and memory learning, because their text is not a statement from the user.
- **Delegated children.** `model_start` is removed from a child's tools, because a child's MCP process ends with its task. Children keep the read-only tools.
- **Profiles and prompts.**
  - `toolGroup` maps the `model_` prefix to `sensors`.
  - `skills/device-sensors/SKILL.md` covers when to start a watch, choosing classes and a threshold, and stopping watches.
  - The chat system prompt (`chat/prompt.go`) lists the tools.
- **Headless (`--prompt`).** There is no inbox. The chat model uses `model_start` (which needs `--yes`) and then `model_events` with `wait_seconds`.

## 9. Error handling

| Condition | Detected by | What the user sees |
|---|---|---|
| Unknown model, camera or instance | `StartModel`, `WatchModel` | Tool error `NOT_FOUND`, listing valid ids |
| No variant for this device | `StartModel` | Tool error naming the unmet requirement |
| Camera the two-plane path cannot bind | `StartModel` | Tool error "camera cannot stream to a model: <reason>" |
| No frames for 10 s (camera unplugged, or held by another process) | host | `FAILED` "no frames from camera", with no restart |
| Two instances already running | `StartModel` | Tool error listing the running instances |
| Image pull or file download fails | supervisor | `FAILED` with the cause; starting again retries |
| Model file sha256 mismatch | supervisor | `FAILED` "model file failed verification"; the file is deleted and never mounted |
| Engine build fails or passes 15 min | host, supervisor | `FAILED` with the last 4 KiB of host output |
| Host exits or stalls for 15 s | supervisor | `RESTARTING`, then `FAILED` after three restarts |
| Watch stream breaks | MCP server | Re-attach with `after_sequence` within 60 s; an unfilled gap is reported as "N events missed" |
| Agent restarts | boot cleanup, MCP server | "Watch ended: device restarted" |
| MCP notification channel or chat inbox full | mcp-go, chat | Item dropped; the gap is reported with the next item, and `model_events` can catch up |
| Device not connected | MCP server | The existing not-connected error |

A failure that arrives within `model_start`'s 30 s is returned as the tool's error. A later failure reaches chat as a status notification and starts an event turn.

## 10. Security and privacy

- **Tighter than an app.** The host container sees only the one camera node, where the `camera` entitlement grants all of major 81 and the host's `/dev`. It also gets its GPU or NPU nodes and its data socket. It has no network, because the agent does all fetching. Its root filesystem is read-only. It runs as non-root with device groups, subject to spike S3.
- **No free-form artifacts.** Clients name only a catalog id and a camera id. Images are pinned by digest in the agent's catalog, and files by sha256. Nothing is run that has not been verified.
- **Frames stay on the device.** Only structured events (class, confidence, box, time) reach chat, and through chat the user's LLM provider. `docs/guides/chat.mdx` states this.
- **The user stays in control.** Starting a watch needs approval. The status bar shows active watches, and `/watches stop all` ends them without the chat model. Quitting chat stops everything within the grace period.
- **Events are treated as untrusted input.** They are wrapped as untrusted sensor JSON even though our own host generates them, because `label` comes from the chat model.
- **Authorization** is unchanged from other services: the mTLS tenant check (`CheckMTLS`) on the network, and the admin socket for containers with the `admin` entitlement. There are no per-call roles.

## 11. Testing and validation

- **Agent (Go):**
  - variant selection from fake device info;
  - admission limits and reuse;
  - lease, grace and "never watched" expiry on a fake clock;
  - the supervisor against a fake containerd client and puller;
  - file cache verification, including a mismatch;
  - event-tap fan-out with a slow subscriber;
  - ring replay and gaps;
  - boot cleanup;
  - an end-to-end data-socket test with a fake host process.
- **Model host:**
  - Mojo unit tests for letterboxing, box decoding, NMS, the tracker and the event rules;
  - a golden image set that checks TensorRT, ONNX Runtime and QNN produce the same detections within a tolerance.

  A new `.github/workflows/model-host.yml`, modelled on `ros2-inspector.yml`, builds the images on arm64 runners and runs the Mojo tests and the CPU image's golden tests. It publishes on manual dispatch, and the published digests are pinned in `catalog.json`.
- **MCP:**
  - registration and annotation tests;
  - handlers against a fake `WendyModelService` gRPC server;
  - notification forwarding and re-attach after a broken stream.
- **Chat:**
  - an event while idle starts a turn;
  - while busy, events queue and merge;
  - the 10 s window;
  - `left` does not start a turn;
  - `/watches` and the status indicator;
  - escaping in the event wrapper;
  - memory is skipped for event turns;
  - `model_start` is hidden from children;
  - a headless run that uses `model_events`.
- **Hardware validation** on Orin Nano, AGX Orin, Thor, Pi 5 and the Qualcomm board:
  - first-start and warm-start times;
  - steady processed fps;
  - latency per stage;
  - CPU, GPU and NPU load;
  - the scripted scene from §3;
  - cleanup after killing chat.

  Results are recorded in `specs/<date>-model-watch-validation.md`, in the style of `specs/wdy-2906-2913-validation.md`.

## 12. Spikes (before implementation)

| # | Question | Options | Decision rule | Output |
|---|---|---|---|---|
| S1 | Which detector | YOLOX (nano, tiny, s), RTMDet (tiny, s), RF-DETR (nano) | Apache-2.0; exports to ONNX; converts for TensorRT and QNN; at least 10 fps on the Pi 5 CPU and within the §3 latency on every target. Among those that pass, pick the highest person-class AP. | Model, per-variant sizes, ONNX files and hashes |
| S2 | Qualcomm engine path | (a) a QNN context binary compiled off the device for the SoC, loaded through the QNN API; (b) ONNX Runtime's QNN execution provider using the device's QNN libraries | Whichever runs the S1 model on the Hexagon NPU with the board's QNN version. If both work, prefer (b), since one file format then serves CPU and NPU. | Adapter and file format |
| S3 | Jetson TensorRT | TensorRT Python bindings in the image, against the TensorRT libraries CDI mounts from JetPack 7.2 on Orin and Thor | Bindings load and build an engine on both; measure build time; find whether `/dev/nvmap` access needs root | Image recipe, user, build-time estimate |
| S4 | Decoding on the Pi | CPU cost of decoding the hub's encoded frames on a Pi 5 during inference, at the camera's resolution. That is H.264 where the agent encodes, and MJPEG where the camera provides it; the Pi 5 has no H.264 hardware decoder. | If it breaks the 10 fps budget, choose between asking the producer for MJPEG or a lower resolution, and processing keyframes only | Decode strategy |
| S5 | Mojo interop | Per-frame cost of Mojo↔Python calls for capture and inference; compiled binary and image sizes per engine | If interop costs more than 2 ms per frame, move capture or engine calls to Mojo's C FFI | Interop boundary, image sizes |

## 13. Suggested plan decomposition

Each milestone gets its own implementation plan. M2 does not depend on the spikes and can start in parallel, against a fake host that speaks the §6.1 records. M1 needs S1, S4 and S5; M4 needs S2 and S3.

1. **M0: spikes S1–S5.**
2. **M1: host and CPU image as a plain app.** The model host plus the CPU image, run as an ordinary app on a Pi 5 with the `camera` and `episode-write` entitlements. This proves the harness contract before any agent changes.
3. **M2: agent and CLI.** `WendyModelService`: proto, catalog, file cache, supervisor, leases, event tap, boot cleanup. Plus `wendy device model`.
4. **M3: MCP and chat.** The MCP tools and notifications; chat's inbox, event turns, `/watches`, skills and docs.
5. **M4: Jetson and Qualcomm.** The Jetson image with the TensorRT adapter and engine cache, and the Qualcomm image with the QNN adapter.
6. **M5: validation.** Hardware validation and its write-up.

## 14. File map (indicative)

- **Proto:** `Proto/wendy/agent/services/v2/model_service.proto`; `go/scripts/generate-proto.sh`; generated code in `go/proto/gen/agentpb/v2/`.
- **Agent:**
  - `go/internal/agent/models/` (new): `catalog.go`, `catalog.json`, `select.go`, `files.go`, `supervisor.go`, `lease.go`, `events.go`;
  - `go/internal/agent/services/model_service.go` (new);
  - `go/internal/agent/services/app_data_socket.go`: the tap;
  - `go/internal/agent/containerd/model_host.go` (new);
  - `go/internal/agent/containerd/camera_wiring.go`: demand;
  - `go/cmd/wendy-agent/main.go`: registration and boot cleanup.
- **Model host:** `go/modelhost/` (new, alongside `go/ros2/inspector/`):
  - `src/*.mojo`;
  - `engines/{tensorrt,onnxruntime,qnn}.py`;
  - `capture.py`;
  - `images/{jetson,cpu,qualcomm}/Dockerfile`;
  - `tests/`.

  Plus `.github/workflows/model-host.yml`.
- **CLI:** `go/internal/cli/commands/device_model.go` (new); `go/internal/cli/grpcclient/client.go`.
- **MCP:** `go/internal/cli/mcp/tools_model.go` (new); `server.go` (keep `srv`, register); `tools_guide.go`.
- **Chat:**
  - `go/internal/cli/chat/mcp.go`: notifications;
  - `agents.go`: session inbox, removing `model_start` from children;
  - `tui.go`: inbox, event entries, event queue, `/watches`, status;
  - `engine.go`: turns without memory;
  - `profiles.go`: `model_` prefix;
  - `prompt.go`;
  - `skills/device-sensors/SKILL.md`;
  - `go/internal/cli/agentservice/service.go`: the shared event wrapper.
- **Docs:** `docs/guides/chat.mdx` gets a "Watching with models" section; the CLI reference is generated from the new commands.
