# WendyDataModelApp — reference app for the model harness

The smallest complete app that proves the three model-harness contracts on
WendyOS, so a Wendy Arm demo needs nothing beyond `wendy run` and
`wendy data campaign deploy campaign.yaml`.

| Contract | Mechanism in this app |
|---|---|
| Frames in | Camera frames **fed by the harness** at the camera's own device path (`WENDY_CAMERA_DEVICE`, default `/dev/video0`), through the node the `camera` entitlement gives the app. The app never opens the physical device. |
| Predictions out | One `prediction` record per scored frame, naming the sample identifiers it was computed from, plus a `person_detected` event, over the app-private data socket granted by the `episode-write` entitlement (`WENDY_DATA_SOCKET`) |
| Actuation out | A Robot Operating System 2 (ROS 2) `geometry_msgs/Twist` on `/cmd_vel` when ROS 2 is enabled; the identical control decision is logged when it is not |

```
WendyDataModelApp/
├── wendy.json          ← camera + episode-write entitlements
├── Dockerfile          ← YOLOv8n ONNX export, CPU runtime
├── app.py              ← read, detect, record, actuate loop
├── wendyframes.py      ← reads the app's camera node (V4L2, PyAV and OpenCV decode)
├── wendydata.py        ← data socket client (standard library only)
├── test_wendydata.py   ← unit tests for framing, uncertainty, input references
└── campaign.yaml       ← the single flight-recorder plan, armed by the records
```

## The contract this app demonstrates

The harness feeds the model, and the episode records exactly what it fed
it.

1. `wendy.json` asks for the camera as
   `{"type": "camera", "format": "native", "stamp": "frame"}`. The app opens
   the camera's own path and gets the node the agent feeds for it: the
   camera's own frames, H.264 or Motion-JPEG, with the agent's frame number
   in each buffer's timestamp, encoded as `tv_sec * 1000000 + tv_usec`. This
   field carries identity, not clock time; it is the `sample_id` of the
   episode ledger, and predictions reference `(source_id, sample_id)`. The
   app's dequeue time is separate.
2. Because that node is fed by the agent, the app is one more consumer of
   the producer the campaign's capture adapter consumes. Video4Linux2
   admits a single holder of a capture device; the agent is that holder, so
   the app and a campaign that records the camera run at the same time.
3. Each `prediction` record carries `inputs: [{source_id, sample_id}]`.
   The camera capture's `index.jsonl` carries the same identifier on the
   bytes it kept. Match each prediction's source and sample ID to that index
   to locate the recorded input. The V4L2 example does not currently report
   individual deliveries to `model_inputs.jsonl`; that ledger is populated
   by the Agent's managed inference path. Do not infer delivery completeness
   or zero dropped inputs from this example's prediction records.

## What the app does

Per delivered frame (scoring at most 5 per second, see rate limiting
below):

1. Opens the camera node, reads its pixel format, and decodes accordingly:
   whole H.264 access units with PyAV, Motion-JPEG frames with OpenCV.
   Private packet timestamps preserve the original sample identity through
   H.264 decoder reordering. A node keeps only its newest frames, so after
   frames the app never dequeued (inference slower than the camera) H.264
   decoding resumes at the camera's next keyframe, and the frames in between
   count as dropped. Missing identities, malformed access units, and
   excessive buffered identities stop capture rather than assign a prediction
   to guessed input bytes. YUYV, BGR3 and RGB3 pictures are read too. The
   agent serves pictures on a camera whose own frames it cannot hand on, and
   `wendy run` says so; but a `"frame"` stamp is never replaced by a time, so
   when those pictures carry no frame number the node gets no frames at all.
   The app then logs, every 10 seconds, that its camera node is not being
   fed, and `wendy device camera list` shows the node as `cannot serve` with
   the reason.
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

Set `WENDY_CAMERA_DEVICE` to read another camera; it defaults to
`/dev/video0`. `WENDY_CAMERA_SOURCE` is the source ID the prediction records
name and defaults to `v4l2:` followed by the device path, which is how
`wendy data sources` lists a local camera.

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

## Run on a dev box (no GPU, no ROS 2)

The default image is CPU-only and needs no ROS 2 stack:

```sh
cd Examples/WendyDataModelApp
wendy run
```

The build has one throwaway stage: it installs Ultralytics (pinned to
8.3.63) and exports `yolov8n.onnx` at opset 12. The runtime image carries
onnxruntime, OpenCV, NumPy and PyAV.

Frame input is not optional the way the data socket is: if the camera
cannot be opened the app has no frames to score and exits with the error.

A camera that was there and then goes away is a different case, and is
treated as transient: it can be unplugged and plugged back in while the app
itself is perfectly healthy. The app reopens it, up to
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
still arrive through the camera node.

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

For SDK-free opaque packets, durable Protobuf envelopes, additional media types,
and batched time series, see [application recording streams](../WendyRecording/README.md).
This example continues to use the compatible legacy JSON endpoint.
