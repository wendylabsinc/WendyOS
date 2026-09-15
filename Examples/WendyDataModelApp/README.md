# WendyDataModelApp — reference app for the model harness

The smallest complete app that proves the three model-harness contracts on
WendyOS, so a Wendy Arm demo needs nothing beyond `wendy run` and
`wendy data campaign deploy campaign.yaml`.

| Contract | Mechanism in this app |
|---|---|
| Frames in | Camera frames **fed by the harness** on a v4l2loopback node granted by the `camera` entitlement (`WENDY_CAMERA_NODE`). The app never opens the physical device. |
| Predictions out | One `prediction` record per scored frame, naming the sample identifiers it was computed from, plus a `person_detected` event, over the app-private data socket granted by the `episode-write` entitlement (`WENDY_DATA_SOCKET`) |
| Actuation out | A Robot Operating System 2 (ROS 2) `geometry_msgs/Twist` on `/cmd_vel` when ROS 2 is enabled; the identical control decision is logged when it is not |

```
WendyDataModelApp/
├── wendy.json          ← camera + episode-write entitlements
├── Dockerfile          ← YOLOv8n ONNX export + gRPC stub generation, CPU runtime
├── app.py              ← subscribe, detect, record, actuate loop
├── wendyframes.py      ← reads the agent-fed node (V4L2 + OpenCV decode)
├── wendydata.py        ← data socket client (standard library only)
├── test_wendydata.py   ← unit tests for framing, uncertainty, input references
├── proto/              ← build-time copy of sensor_service.proto (parity-tested)
└── campaign.yaml       ← the single flight-recorder plan, armed by the records
```

## The contract this app demonstrates

The harness feeds the model, and the episode records exactly what it fed
it.

1. The app reads the agent-fed v4l2loopback node and gets, with each frame,
   the agent's `CLOCK_BOOTTIME` receipt in the V4L2 buffer timestamp. That
   timestamp is the frame's identity, and the episode records the same
   value, so a prediction can name the frame it scored.
2. Because the app reads that node rather than `/dev/video0`, it is one
   more consumer of the producer the campaign's capture adapter consumes.
   Video4Linux2 admits a single holder of a capture device; the agent is
   that holder. **This is why the example no longer ships a
   telemetry-only campaign variant** — there is no conflict left to work
   around.
3. Each `prediction` record carries `inputs: [{source_id, sample_id}]`.
   The agent writes every delivered sample into the episode's
   `model_inputs.jsonl` under the same identifier, and the camera
   capture's `index.jsonl` carries that identifier on the bytes it kept.
   Joining on `(source_id, sample_id)` reconstructs (input, outcome)
   pairs — which is what makes the episode training data rather than a
   log of conclusions.

## What the app does

Per delivered frame (scoring at most 5 per second, see rate limiting
below):

1. Receives an encoded sample from the harness and decodes it with PyAV.
   A decoded frame may be assembled from more than one sample on
   transports that deliver byte-stream chunks rather than whole access
   units, which is why `inputs` is a list.
2. Runs YOLOv8n, exported to Open Neural Network Exchange (ONNX) at image
   build time, through onnxruntime.
3. Sends a `prediction` record: model `yolov8n`, the pinned model version,
   the frame's uncertainty score, the top detections, and the sample
   identifiers the frame came from.
4. Sends a `person_detected` event when a person newly enters the view
   (edge-triggered, so a person standing still fires the campaign once).
5. Computes an actuation decision — turn towards the largest person and
   creep forward when centered — and publishes it as a Twist (ROS 2 mode)
   or logs it (default mode).

Set `WENDY_CAMERA_NODE` to pick the node explicitly; it defaults to
`/dev/video10`, the first node the agent creates. `WENDY_CAMERA_SOURCE`
labels the frames in prediction records and defaults to the node path.

A camera the harness lists but cannot stream to models is reported with the
reason rather than silently skipped. Only camera sources are subscribable in
this release: audio and ROS 2 sources are captured into episodes, and
subscribing to one is refused with an error saying that kind is not
subscribable yet.

### The uncertainty formula

```
uncertainty = 1 - max(confidence over detections >= threshold)
            = 1.0 when nothing was detected above the threshold
```

Clamped to 0..1. A frame the model is sure about scores near 0; an empty
or ambiguous frame scores 1.0. The formula lives in
`wendydata.uncertainty_score` and is unit-tested.

Read that second line carefully before arming a `model.uncertainty` trigger
on it. Because the score is 1 minus a confidence, it does not separate "the
model was unsure about what it saw" from "the model recognised nothing": any
scene without a class the detector knows sits at exactly 1.0. A threshold of
0.65 therefore fires on every prediction, which is why `campaign.yaml` no
longer arms one. Measured on a Jetson with nobody in frame, it produced a
fresh episode every 30 seconds. The score is still worth recording, and every
prediction carries it, so it remains queryable after the fact; triggering on
it needs entropy across classes or the margin between the top two.

### Prediction rate limiting

The agent rejects more than 200 records per second per app. This app stays
far below it by skipping frames: it scores at most
`WENDY_PREDICTIONS_PER_SECOND` of the frames it receives (default 5 per
second) and sends one prediction per scored frame. Acknowledgements are
honored: `buffered` and `recorded` are success, `rejected` is logged with
the agent's reason, and the client reconnects and retries once when the
socket drops.

Skipping happens after delivery, so the episode's ledger still records
every frame the model was handed; each prediction reports how many frames
were skipped since the previous one in
`attributes.frames_skipped_since_last_prediction`. If the app falls behind
far enough that the harness has to drop samples before it, the sample
carries `dropped_before` and the app logs a warning — a gap in the sample
identifiers is always explained, never silent.

`encoding` is a per-sample field, not a per-stream one, so the decoder
follows the samples: it is rebuilt whenever the encoding changes, and
anything the retired decoder still holds is drained first and attributed to
the samples that produced it. A decoder pinned to whichever encoding
arrived first would decode nothing at all after a mid-stream switch.

## Run on a dev box (no GPU, no ROS 2)

The default image is CPU-only and needs no ROS 2 stack:

```sh
cd Examples/WendyDataModelApp
wendy run
```

The build has one throwaway stage: it installs Ultralytics (pinned to
8.3.63) and exports `yolov8n.onnx` at opset 12. The runtime image carries
onnxruntime, OpenCV, NumPy and grpcio.

Frame input is not optional the way the data socket is: if the node cannot
be opened the app has no frames to score and exits with a clear message
naming the missing entitlement.

A node that was there and then goes away is a different case, and is treated
as transient: an agent restart removes the node while the app itself is
perfectly healthy. The app reopens it, up to
`WENDY_CAMERA_RECONNECT_ATTEMPTS` consecutive times (default 5), one every
`WENDY_CAMERA_RECONNECT_DELAY_SECONDS` (default 2), and the budget is
refilled by every frame that arrives, so weeks of unrelated restarts do not
add up to a shutdown. Only once the stream stays gone for the whole budget
does the app exit, and it says so rather than stopping quietly with the
campaign still armed. Set the attempts to 0 to exit on the first failure
instead.

Without a data socket (running outside WendyOS, or without the
`episode-write` entitlement) it keeps running and logs each dropped record;
without ROS 2 it logs each actuation decision.

`proto/wendy/agent/apps/v1/sensor_service.proto` is a copy of the
canonical `Proto/wendy/agent/apps/v1/sensor_service.proto`, because the
image build context is this directory. A Go test
(`TestExampleSensorProtoMatchesCanonical`) fails if the two drift; when the
canonical proto changes, copy it over.

Unit tests (no camera or agent needed):

```sh
python3 -m unittest discover Examples/WendyDataModelApp
```

## Run on a Jetson with the arm (GPU, ROS 2 on)

Two independent switches:

**GPU**: swap the runtime wheel for the Jetson build of onnxruntime. In
the Dockerfile's runtime stage replace `onnxruntime` with
`onnxruntime-gpu` resolved against the Jetson wheel index — the same
wheel `Examples/HelloONNX` resolves with the Stagefile's `cuda: true` GPU
profile — and add the `gpu` entitlement to `wendy.json`. The app already
passes `ort.get_available_providers()`, so the CUDA (Compute Unified
Device Architecture) provider is picked up automatically when present.
Note that the `gpu` entitlement is about the model, not the camera: frames
still arrive through the sensor socket.

**ROS 2 actuation**: build from a ROS base image so `rclpy` is importable
(for example `ros:humble` plus this Dockerfile's pip packages), set the
environment gate, and declare the ROS 2 framework so the agent injects the
Data Distribution Service (DDS) domain:

```jsonc
// wendy.json additions for the ROS 2 variant
"env": { "WENDY_MODEL_APP_ROS2": "1" },
"frameworks": {
  "ros2": { "domainId": 42, "rmw": "rmw_cyclonedds_cpp", "distro": "humble" }
}
```

See `Examples/ROS2` for what `frameworks.ros2` injects
(`ROS_DOMAIN_ID`, `RMW_IMPLEMENTATION`) and the shared-ipc isolation a
multi-container ROS graph wants. With the gate off, or with `rclpy`
missing, the app logs the same decisions it would have published, so the
demo works on a plain Jetson without any ROS 2 stack.

## Deploy the campaign

```sh
wendy data campaign deploy campaign.yaml
wendy data campaign list
```

`campaign.yaml` arms one trigger, the edge-triggered `person_detected`
event, and captures the camera continuously and uncapped so the episode
holds the frames the model actually consumed. The `applications` source is
always captured; it does not appear under `sources:` because it cannot be
deselected.

This one campaign works with the app running, on a single-camera device.
It replaces the pair of campaigns the example used to ship.

To fire an episode without waiting for a trigger:

```sh
wendy data campaign trigger model-harness-demo --reason commissioning
```

## What to expect in an episode

```sh
wendy data episodes
wendy data inspect <episode-id>
```

- `events.jsonl` — the application records: the app's `prediction`
  records (model, model_version, uncertainty, top detections, and the
  `inputs` list naming the samples each prediction came from) and any
  `person_detected` events, each stamped with agent receipt time and
  clock-uncertainty metadata. Records honor the 10-second pre-trigger
  buffer.
- `model_inputs.jsonl` — one line per sample the harness handed to the
  model: `source_id`, `sample_id`, `episode_nanos`, `payload_bytes`, the
  consuming app and model, and `dropped_before`. This is the ledger of
  what the model actually saw.
- `cameras/<source>/segment-*.h264` plus `cameras/<source>/index.jsonl` —
  the recorded video and, per frame, the `sample_id` together with the
  segment file and byte offset holding its bytes. The payload is stored
  once: the ledger references it rather than duplicating it.
- A manifest tying sources, trigger reason (`event:person_detected` or
  `model_uncertainty:<value>`), and the campaign revision together, plus a
  `model_io` block that states the join keys, counts the predictions that
  named their inputs, and reports per source whether the episode retains
  payloads for every consumed sample or only the subset the capture policy
  kept.

### Reconstructing (input, outcome) pairs

```python
import json

ledger = {(e["source_id"], e["sample_id"]): e
          for e in map(json.loads, open("model_inputs.jsonl"))}
frames = {(source, r["sample_id"]): r
          for r in map(json.loads, open(f"cameras/{source}/index.jsonl"))}

for record in map(json.loads, open("events.jsonl")):
    if record.get("type") != "prediction":
        continue
    for ref in record.get("inputs", []):
        key = (ref["source_id"], ref["sample_id"])
        frame = frames.get(key)          # None: the capture policy kept no bytes
        # frame["segment"], frame["byte_offset"], frame["byte_size"] locate
        # the exact bytes the model consumed for this prediction.
```

A prediction with no `inputs`, or a reference with no matching frame, is
not a failure to hide: the manifest's `model_io` counts both cases so a
consumer knows how much of the episode is usable as training data.

With `upload.when: always`, finished episodes upload whenever the device
has connectivity.
