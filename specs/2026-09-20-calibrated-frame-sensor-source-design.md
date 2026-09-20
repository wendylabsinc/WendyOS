# Calibrated Frame Sensor Source — the agent publishes frames, not video

**Date:** 2026-09-20
**Status:** Design (not yet planned)
**Builds on:** `specs/2026-08-31-agent-sensor-source-design.md` (an agent exposing its sensors over gRPC), `specs/2026-08-28-remote-sensor-mounting-design.md` (the `sensorlink` payload messages), and the raw frame tap in `go/internal/agent/services/video_raw_tap.go`
**Scope:** A new agent-owned capture contract for RGB-D cameras: one capture, published as colour plus depth aligned to that colour, with intrinsics, one timestamp and the settings that produced it — to consumers that declare what they require and are refused by name when it cannot be met.

## 1. Summary

The agent already multiplexes one camera to many subscribers: `deviceHub`
fans an encoded stream out to viewers and, since the raw tap, the
untouched capture bytes out to analytic subscribers
(`go/internal/agent/services/video_service.go:464`,
`video_raw_tap.go:15-18`). That is what lets two apps share one webcam
instead of fighting over the V4L2 node.

It carries **pictures**. An RGB-D camera does not produce a picture; it
produces a *measurement* — colour, a depth value per colour pixel, and
the intrinsics that turn both into metres. The agent has no vocabulary
for any of that: `VideoFrame` is bytes plus a timestamp plus a codec,
and `RawFormat` is width, height, fourcc and stride
(`Proto/wendy/agent/services/v1/wendy_agent_v1_video_service.proto:166-179`).
So an app that needs depth cannot use the agent at all. It opens the
device directly through the vendor SDK, takes exclusive ownership of the
node, and every other app on that camera is locked out — the exact
problem the raw tap was built to remove, reappearing one layer up.

This design adds a second thing the agent can publish: a **calibrated
frame**. Alignment, pairing and calibration happen once, at the source,
in the only place that owns the sensor. Consumers **declare** what they
require and get a refusal naming what is missing, instead of a stream
that is quietly worth less than they think.

## 2. Field evidence — what silent degradation actually cost

Observed 2026-09-20 on a Unitree G1 Nx (WendyOS) running `identity`, the
badge-reading app from `github.com/wendylabsinc/wendy-voice` (branch
`codex/badge-conference-readiness`), with an Intel RealSense D435i.

- `identity` opens the D435i through librealsense, exclusively, because
  that is the only way to get depth. While it holds the camera nothing
  else can use it.
- A sibling demo (`g1-rock-paper-scissors`) avoids the fight by reading
  the agent's shared stream via `wendy device camera view --raw --stdout`
  (`go/internal/cli/commands/camera.go:426`). That path exists and works
  — and gives colour only.
- The app's own frame type already carries the right fields:
  `bgr`, `frame_id`, `captured_at_ns`, `depth_m`, `intrinsics`,
  `capture_settings` (`badge-id/badge_id/camera.py`, `Frame`). It gets
  them by calling `rs.align(rs.stream.color)`, `get_depth_scale()` and
  the live stream profile's intrinsics itself.
- When depth was absent, four independent safety checks — the distance
  window, the visitor-depth consistency check, the physical-badge-size
  check and the pixel budget — all became inert at once, and the person
  mask degraded to a rectangle. The robot then offered a name it had
  read off a **wall poster**.

Losing depth produced no error. It produced confident nonsense. The app
has since been given an explicit startup gate that refuses to name
anybody on an unaligned or depth-less frame
(`identity/identity/recognise/stream.py`, `BadgeCameraPolicy.check`) —
which is the right consumer-side answer, and is also an admission that
the platform handed it a frame it could not trust.

## 3. Goals / Non-goals

**Goals**
- One message per capture instant carrying: colour pixels; depth in
  metres, aligned pixel-for-pixel to those colour pixels; intrinsics
  with their provenance; **one** `frame_id` and **one** `captured_at_ns`
  shared by both planes; and the capture settings that were actually
  applied.
- Alignment performed **once, at the source**, from the module's factory
  extrinsics — never per consumer, never approximated downstream.
- Consumers declare requirements; an unmet requirement is a
  `FailedPrecondition` naming what is missing, before the first frame —
  the discipline the raw tap already applies (`video_raw_tap.go:22-26`,
  `streamreason.RawUnavailable`).
- Mid-stream loss of a required property ends the stream with the same
  reason, rather than continuing without it.
- Fan-out is latest-wins per subscriber: a slow analytics consumer never
  stalls a viewer, and never receives an aging window either.
- The camera stays shared. Nothing about owning depth may re-introduce
  exclusive ownership of the node.

**Non-goals (v1)**
- Replacing `StreamVideo`. Viewers, the companion app and the raw tap
  are unchanged; this is an additional surface, not a migration.
- Depth from a camera that has none. A colour-only camera publishes a
  calibrated frame with no depth plane and is refused to a consumer that
  required one — it is not given synthesised depth.
- Point clouds, IMU fusion, temporal filtering, multi-camera extrinsics.
- Cloud-relayed calibrated frames; LAN/local only, like the raw tap.
- Lossy depth. See §6.

## 4. The contract

New v2 service beside the existing sensor services
(`Proto/wendy/agent/services/v2/`), reusing the agent's mTLS server and
port exactly as `WendySensorService` does
(`specs/2026-08-31-agent-sensor-source-design.md` §5).

```proto
service WendyCalibratedFrameService {
  rpc ListCalibratedSources(ListCalibratedSourcesRequest) returns (ListCalibratedSourcesResponse);
  rpc StreamCalibratedFrames(StreamCalibratedFramesRequest) returns (stream CalibratedFrame);
}

message CameraIntrinsics {
  enum Provenance {
    PROVENANCE_UNSPECIFIED = 0;
    MEASURED = 1;   // read from the device's own calibration
    ASSUMED  = 2;   // derived from a datasheet FOV, or a default
  }
  float  fx = 1; float fy = 2; float cx = 3; float cy = 4;
  uint32 width = 5; uint32 height = 6;   // the resolution they describe
  Provenance provenance = 7;
  string note = 8;                       // where they came from, for the operator
}

message DepthPlane {
  enum Alignment {
    ALIGNMENT_UNSPECIFIED = 0;
    ALIGNED_TO_COLOUR = 1;  // depth[y][x] describes colour[y][x]
    SENSOR_NATIVE     = 2;  // the depth sensor's own frame; NOT interchangeable
  }
  bytes     data = 1;            // uint16 little-endian, row-major
  uint32    width = 2; uint32 height = 3;
  uint32    bytes_per_line = 4;
  float     scale_m = 5;         // metres per unit. Absent is a refusal, not 0.001.
  Alignment alignment = 6;
}

message CaptureSettings {         // APPLIED, read back from the device. Never requested.
  uint32 exposure_raw = 1;        // the driver's own number
  float  exposure_us = 2;         // converted, when the unit is known
  float  exposure_unit_us = 3;    // 0 when the unit is unknown -- do not infer one
  float  gain = 4;
  bool   auto_exposure = 5;
}

message CalibratedFrame {
  uint64 frame_id = 1;            // monotonic per stream
  uint64 captured_at_ns = 2;      // ONE capture instant, shared by both planes
  bytes  colour = 3;              // layout in colour_format
  RawFormat colour_format = 4;    // reuse: width/height/fourcc/bytes_per_line
  DepthPlane depth = 5;           // absent when the source has none
  CameraIntrinsics intrinsics = 6;
  CaptureSettings settings = 7;
  string source = 8;              // stable camera identity, as `camera list` reports it
}
```

Four rules the **source** enforces, so no consumer has to discover them:

1. When `alignment == ALIGNED_TO_COLOUR`, the depth plane's width and
   height **equal** the colour plane's. This is exactly the check the
   badge app runs today before it will name anyone; the source asserting
   it is what makes that check never fire.
2. `scale_m > 0` or no depth plane is sent. Depth in unknown units is
   not depth. A source that cannot read its scale must refuse, not
   default to 1 mm.
3. `provenance` is load-bearing, not decoration: a consumer computing a
   physical size from pixels must be able to tell a measured focal
   length from a guessed one, and the D435i reports a measured one.
4. Both planes carry the same `frame_id` and `captured_at_ns`. Pairing
   is the source's job; two independently-timestamped streams are not a
   frame.

## 5. Requiring, and being refused

```proto
message StreamCalibratedFramesRequest {
  enum Requirement {
    REQUIREMENT_UNSPECIFIED = 0;
    ALIGNED_DEPTH       = 1;
    MEASURED_INTRINSICS = 2;
    CAPTURE_SETTINGS    = 3;
  }
  string source = 1;
  uint32 width = 2; uint32 height = 3; uint32 framerate = 4;
  repeated Requirement require = 5;
}
```

- Every requirement is checked **before the first frame**. An unmet one
  returns `FailedPrecondition` carrying a new `streamreason` code
  (`REQUIREMENT_UNMET`) whose metadata names each missing property and
  why this source cannot provide it — the mechanism
  `streamreason.RawUnavailable` already uses
  (`go/internal/shared/streamreason/streamreason.go:14-22`).
- A requirement that **stops** being met mid-stream (the depth sensor
  drops out, auto-exposure is taken over) ends the stream with that same
  reason. It never continues with the property silently absent. This is
  the single rule that would have turned the field failure in §2 into a
  visible outage.
- Nothing is ever downgraded to satisfy a request. A consumer that
  requires nothing gets whatever the source has, and can read the
  message to find out what that was.

## 6. Where the pixels come from

A `FrameSource` seam inside the agent, one implementation per capture
technology:

- **`realsense`** — librealsense: one `rs2::pipeline` with colour and
  depth enabled, `rs2::align(RS2_STREAM_COLOR)` per frame set,
  `get_depth_scale()` for `scale_m`, and the colour stream profile's
  intrinsics as `MEASURED`. This is the only v1 source that can emit
  `ALIGNED_TO_COLOUR`. It is also the only place the vendor extrinsics
  exist, which is the whole argument for doing this in the agent.
- **`v4l2`** — the existing capture path, colour only, no depth plane,
  intrinsics `ASSUMED` or absent. It exists so one consumer API covers
  every camera on the fleet rather than only the RGB-D ones.

**Process model.** librealsense is a C++ SDK; linking it into the agent
binary would put a cgo dependency on every WendyOS image whether or not
a RealSense is attached. v1 should instead run the RealSense source as a
**supervised helper process the agent spawns and owns**, handing frames
back over a pipe — the same shape as the `gst-launch` child the video
service already supervises, whose raw branch writes to fd 3
(`video_raw_tap.go:46-58`). The agent keeps the hub, the fan-out, the
lifecycle and the refusals; the helper only captures. This is the
largest open cost in the design and the first thing a plan should pin
down.

**Depth is never compressed.** A lossy depth plane is a plane of
plausible wrong distances, which is the failure mode this whole document
is about. Colour may be uncompressed or JPEG, at the consumer's choice.

**Message size.** One raw frame is one gRPC message, and the default
receive limit is 4 MiB (`video_raw_tap.go:60-65`). A 1280x720 YUYV
colour plane and a 1280x720 16-bit depth plane are 1,843,200 bytes each
— 3.5 MiB together, which fits, barely. At 1920x1080, the resolution the
badge app needs to read a badge at a metre, the colour plane alone is
4,147,200 bytes and does not. So the source must state the frame size in
`ListCalibratedSources`, the client must raise its own limit
(`grpc.MaxCallRecvMsgSize`, as `go/internal/cli/mcp/tools_camera_snapshot.go:142`
already does for `StreamVideo`), and a combination that would exceed the
negotiated limit is **refused at subscribe time with the numbers** —
never truncated, never silently downscaled.

## 7. Why the raw tap cannot be widened into this

The obvious cheap answer is to add `Z16` — the RealSense depth fourcc —
to the raw tap's format table and tee a second V4L2 node. It does not
work, and the reasons are worth recording so nobody spends the
afternoon again.

1. **GStreamer cannot capture Z16.** Each entry in `rawPixelFormats`
   pairs a V4L2 fourcc with a *caps spelling*, because `v4l2src` picks
   the V4L2 format from the caps — the table's own comment calls that
   "the real boundary" (`video_raw_tap.go:132-136`). `gst_v4l2_formats[]`
   in gst-plugins-good contains no `V4L2_PIX_FMT_Z16` entry at all
   (checked against branches `1.24` and `main`). `GRAY16_LE` maps back
   to `Y16`/`Y16_BE`, which a depth node does not advertise, so those
   caps would fail to negotiate rather than deliver depth.
2. **`RawFormat` cannot describe depth.** It carries width, height,
   fourcc and stride and nothing else. A subscriber would receive uint16
   in device units with no scale, no way to obtain one, and no way to
   notice — reproducing §2's failure in a new place.
3. **Two taps are two captures.** Colour and depth are separate
   `/dev/video*` nodes on a RealSense. Two independent V4L2 pipelines
   give two clocks, no frame pairing, and no extrinsics; alignment is
   not something a consumer can reconstruct from that.
4. **The agent has no Z16 anywhere.** `video_service.go:42-47` defines
   six pixel formats (H264, YUYV, MJPG, UYVY, Y16, GREY); `Z16` does not
   appear in `go/` at all.

The accompanying change to this spec does the only part of that which
*is* correct today: it teaches the refusal to say "this is a depth node"
instead of listing formats the node will never have. It delivers no
depth.

## 8. Fan-out

Reuse `deviceHub`, with one change. Today a subscriber that falls behind
loses the **newest** frame: the broadcast does a non-blocking send into
a 4-deep channel and drops on a full buffer
(`video_service.go:478-486`, `video_service.go:395-396`), so a slow
consumer works through an aging backlog. For an encoded stream that is
tolerable. For a measurement it is not — a depth frame's value is that
it describes *now*.

Calibrated-frame subscribers get **latest-wins**: a depth-1 slot per
subscriber, overwritten on arrival. A slow analytics consumer then reads
stale-by-at-most-one-frame data and never stalls the viewer sharing the
camera, which is the property the tee's leaky queue already protects on
the producer side (`video_raw_tap.go:54-58`).

## 9. Error handling

- No RealSense present, or the helper cannot start → `FailedPrecondition`
  naming the source, never an empty stream.
- Depth requested from a colour-only camera → `REQUIREMENT_UNMET` listing
  `ALIGNED_DEPTH`.
- Depth scale unreadable → no depth plane, and therefore the same
  refusal to anyone who required it. Never a default scale.
- Alignment unavailable (no extrinsics) → `SENSOR_NATIVE` depth, which
  fails `ALIGNED_DEPTH`. Never approximate alignment.
- Frame would exceed the negotiated message limit → refused at subscribe
  with both numbers (§6).
- The CLI never surfaces a raw `rpc error: code = ...`; every reason
  above has a sentence naming the fix, as the raw tap's do.

## 10. Testing

- **Contract (Go, CI, no hardware):** a fake `FrameSource` emitting a
  known colour/depth/intrinsics triple; assert both planes share
  `frame_id` and `captured_at_ns`, that aligned depth matches the colour
  resolution, and that `scale_m == 0` suppresses the depth plane rather
  than shipping it.
- **Requirements:** each `Requirement` against a source that lacks it →
  `FailedPrecondition` with `REQUIREMENT_UNMET` and the property named;
  each against a source that has it → frames. Plus the mid-stream case:
  a source that stops providing depth ends the stream instead of
  continuing.
- **Fan-out:** a subscriber that does not read must not stall another
  that does, and must then receive the newest frame, not the oldest.
- **Refusal wording** for a depth node, which ships with this spec (see
  `TestPlan_DepthNodeIsRefusedAsDepthAtARequestedSize` and siblings in
  `go/internal/agent/services/video_raw_tap_test.go`).
- **Hardware-gated (manual):** a D435i on a Jetson, two consumers at
  once — one requiring aligned depth, one taking colour only — and
  neither locking the other out. Plus the regression that started this:
  unplug depth mid-run and confirm the requiring consumer *stops*.

## 11. Suggested plan decomposition

1. **Contract + v4l2 source + requirement refusals.** Proto, service,
   hub wiring, latest-wins fan-out, `REQUIREMENT_UNMET`. Colour only —
   so every requirement path is exercised in CI by being refused.
2. **RealSense source (helper process).** librealsense capture, align,
   depth scale, measured intrinsics, applied-settings readback, and the
   agent-side supervision of the helper.
3. **Consumer ergonomics.** `wendy device camera frames --require
   aligned-depth`, and the MCP surface, so the refusal is legible to an
   operator and not only to an app.

## 12. Concrete file map (indicative)

**New**
- `Proto/wendy/agent/services/v2/calibrated_frame_service.proto`
- `go/internal/agent/services/calibrated_frame_service.go` — service,
  requirement checks, latest-wins fan-out
- `go/internal/agent/framesource/` — the `FrameSource` seam, the v4l2
  source, the RealSense helper client
- `go/cmd/wendy-realsense-source/` — the supervised capture helper

**Modified**
- `go/internal/shared/streamreason/streamreason.go` — `REQUIREMENT_UNMET`
- `go/internal/agent/services/video_service.go` — share camera
  ownership/hub lifecycle with the new service
- `go/internal/cli/commands/camera.go` — the consumer-side command
