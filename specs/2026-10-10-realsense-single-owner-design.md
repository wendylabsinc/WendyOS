# RealSense single owner: StreamVideo served from the calibrated capture

**Status:** design — implementation in progress on `cn/realsense-single-owner`
(stacked on #2041, `cn/calibrated-frame-sensor-source`).

## The problem (carried over from the calibrated-frame spec §14)

A RealSense's colour sensor is one V4L2 node, and two parts of the agent open
it without knowing about each other:

- `VideoService`'s producer (`deviceHub`) opens it for `StreamVideo` — H.264
  viewers, the raw tap, episode capture, app loopback nodes.
- The calibrated-frame helper (`wendy-realsense-source`) opens it through
  librealsense for `StreamCalibratedFrames`, because depth aligned to colour
  exists only inside the vendor SDK.

Whoever opens it second is refused. PR #2041 shipped with that documented and
with each refusal naming the other holder (`cameraHolderHint`,
`errRealSenseHeldByStreamVideo`); this design removes the lockout itself.

## The decision

**While a calibrated capture runs, the helper is the camera's only owner, and
`StreamVideo` is served from the helper's colour plane** — re-exposed on a
v4l2loopback node so every existing consumer (GStreamer H.264 producer, raw
tap, episode capture, app loopback) works unchanged against a node that is, to
them, just another camera.

This is the pattern every shared-camera system converged on (a ROS 2 camera
driver node, PipeWire, Android's camera service): one process owns the device,
everyone else subscribes to its streams. It is also the third use of loopback
re-exposure in this agent, after sensor pairing (`mcusource` pumping a remote
device's cameras into local nodes via `ros2camera.CameraWriter`) and the
two-plane data path (`video_service_two_plane.go` pumping the agent's own
producer hub into aux nodes). Nothing architecturally new is introduced.

## Mechanics

### 1. Ownership registry (authoritative, not inferred)

A small table in `VideoService`: V4L2 node path → the calibrated source that
owns it. `CalibratedFrameService` registers nodes when a capture hub starts
and deregisters when the hub drains. `cameraHolderHint` stops guessing from
sysfs and reads the table — the refusal for *other* RealSense nodes (depth,
IR) stays, but the colour node stops being refused at all (§3).

The table lives behind the same nil-safe seam style as `cameraLoopback`: an
agent built without the calibrated service behaves exactly as today.

### 2. The bridge node

When a capture hub starts on a RealSense:

- allocate an aux loopback node (`AllocateAuxNodeNumber` / `EnsureAuxNode`,
  labelled `wendy-realsense-bridge-<serial>`);
- tap the hub (not subscribe: a subscriber would keep the hub alive after the
  last real consumer left, and the bridge must drain with them) and pump each
  frame's colour plane into the node, latest-wins so publish never blocks.

The pump reuses `ros2camera.CameraWriter` with one addition: a raw codec
entry. The helper's colour is `BGR3` (24-bit packed BGR,
`capture_realsense.go: colourFourcc`); the writer's codec→pixel-format table
gains that case. v4l2loopback carries raw formats natively and GStreamer's
`v4l2src` negotiates BGR without help.

The raw tap's format table (`rawPixelFormats`) gains `BGR3` (3 bytes/pixel,
8 sample bits) so raw subscribers of the bridge node are served too.

### 3. Redirection

Device resolution in `VideoService` consults the registry: a `StreamVideo`
(or raw-tap) request naming an owned colour node is transparently served from
its bridge node. The camera list shows the camera once, as before — the
bridge node is an implementation detail and is excluded from enumeration the
same way other aux nodes are.

A request that names an explicit geometry the bridge cannot satisfy (the
helper fixes the capture mode when it starts) is refused with a reason naming
the active calibrated capture and the geometry it is pinned to — the same
shape as today's "already in use with different stream parameters", but with
the owner named.

### 4. Handoff: video first, then depth

`StreamCalibratedFrames` arrives while `deviceHub` holds the colour node
directly:

1. the video producer is stopped with `CAMERA_PRODUCER_RESTARTED` — the
   existing recoverable end-of-stream that episode capture already uses, which
   clients answer by reconnecting;
2. the helper starts, the bridge node comes up, the registry entry lands;
3. reconnecting viewers resolve through §3 onto the bridge node.

One visible blink, bounded by helper start time. The reverse transition (last
calibrated subscriber leaves) does **not** tear the helper down while bridged
video subscribers remain: the helper keeps streaming and the registry entry
stays until *nobody* is consuming either stream. Only then does the hub drain,
the bridge node is removed, and the next `StreamVideo` opens the device
directly as today. This avoids a second blink and keeps the rule simple: the
helper runs while it has any consumer, direct capture resumes only from idle.

### 5. Failure honesty

- Helper dies mid-bridge: the hub ends (existing lifecycle), the pump stops,
  bridged video streams end with `CAMERA_PRODUCER_RESTARTED`; reconnecting
  viewers find the registry empty and open the device directly. Depth
  subscribers get the existing helper-exit error.
- Loopback module unavailable (`cameraLoopback.Available()` fails): the bridge
  cannot exist, so the lockout remains — the refusal text falls back to
  exactly today's behaviour. Documented, not silent.

## Non-goals

- Zero-copy frame passing (pipe copies are measured-affordable; shared memory
  is a contained later optimization).
- Seamless (blink-free) handoff — `CAMERA_PRODUCER_RESTARTED` reconnection is
  the accepted cost of the once-per-session ownership flip.
- Multi-plane sharing for non-RealSense devices; nothing else on the fleet has
  this shape today.

## Testing

- Unit: fake provider + fake hub + fake loopback (the seams exist); takeover
  and reverse-idle transitions asserted on the registry and reason codes.
- Hardware: the G1's D435i (`unitree-g1-nx-2`) — concurrent
  `wendy device camera view` + `wendy device camera frames --require
  aligned-depth`, both orders, plus helper kill mid-stream.
