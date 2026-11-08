# Go2 camera search

Ask for **a watering can**, check the highlighted object, preview a route, then
start Go2 walking over. The fixed ESP32 camera supplies live video through
SensorLink. The gateway's existing `qwen3-vl:4b` VLM identifies objects in ordinary
language. There is no YOLO class list and no coordinate-entry form.

Built on [WendyOS PR #2725](https://github.com/wendylabsinc/WendyOS/pull/2725),
commit `b13e444e11bc10366174db2d5550fc4d8d2a8d06`. Robot localization, bounded
rotation search, route planning and walking use its existing Linux digital-twin
controller. The PR's LiDAR-to-room registration investigation still applies.
The VLM never generates motor commands.

## Open the demo

The development deployment is on the AGX Orin at
`http://192.168.100.126:18993/`. No login or access token is required. Anyone who
can reach this port can view the camera and control the demo, so keep it on your
trusted LAN. Cross-site browser requests are rejected; this is not authentication.

1. Choose the camera and saved room, then **Use this camera**. Camera preview works
   even while Go2 is offline. The app loads the room's actual model and the
   camera's saved lens and pose from Wendy.
2. Check the floor grid drawn over the image. If it lies on the real floor,
   choose **The grid matches the floor**. Estimated camera placements are
   explicitly labelled. If the grid is wrong or placement is missing, align the
   camera with this room in Wendy's visual Digital Twin, then retry setup.
3. Choose **Locate Go2**. The Linux controller matches fresh robot observations
   against the room architecture and saved scan references. Review the blue robot
   marker on the map, then choose **That's where Go2 is**. No numbers are entered.
4. If a stationary scan is insufficient, **Look around** explicitly starts the
   controller's rotation search. Go2 turns in small increments, pauses and
   matches again. Keep the surroundings clear and the tab visible. Stop cancels
   the search; the demo adds a two-minute overall bound. Finding a pose still
   requires confirmation, and does not start a walk.
5. Enter an object description such as `watering can` or `the green watering can`.
   The VLM examines a camera frame. A local tracker then rechecks that candidate
   in fresh frames. Check the highlighted object before **That's it · preview route**.
6. Review the route and stopping point, then **Start walk**. **Stop Go2** is always
   available. Enable the optional browser bark if desired.

## Services and build

The same gateway needs:

- A paired, connected Go2 and fixed ESP32 SensorLink camera.
- The PR stack's `dev.wendy.digital-twin` service on loopback port 8791, with a
  saved room model and visual camera placement for this exact room revision.
- Ollama on loopback port 11434 with `qwen3-vl:4b` installed. `VLM_MODEL` can select
  another compatible vision model. This app never pulls a model automatically.

The camera's stable SensorLink ID selects its multiplexed agent stream, avoiding
V4L2 reader conflicts. The app uses the local agent API through `admin`, host
networking to reach the twin and Ollama, and an `http` entitlement for port 18993.
It needs no direct camera or GPU device access. VLM inference runs in the existing
Ollama service. `admin` grants full local agent access, so treat this as a trusted
first-party example.

From this directory:

```sh
wendy project validate .
wendy --json device camera list --device GATEWAY:50052
wendy --json run --device GATEWAY:50052 --detach --wait-ready --yes
```

Open `http://GATEWAY:18993/`. Startup does not open a robot control session or
issue motion. Camera setup opens only the camera; Locate and Look around acquire
the robot session. Close other robot control views before doing that.

## Perception and geometry

The VLM returns structured object boxes in normalized image coordinates. The app
rejects malformed boxes, multiple matches, clipped or tiny objects, and responses
that do not identify a floor object. Its output is a candidate, not proof of the
object's identity or support surface. The operator reviews the highlighted image.

VLM inference may take seconds. The app therefore keeps streaming camera images
independently. When inference finishes, normalized template matching revalidates
the original object crop in the latest image. The template never updates itself,
limiting drift; weak or spatially ambiguous matches are rejected. Three stable
fresh-frame projections are required before route preview. Tracking is not a
second semantic classification and can confuse visually similar objects.

The saved camera lens, distortion and optical-to-room pose project the detection's
bottom centre onto the floor from the saved USDZ room model. Only points inside
actual floor triangles are eligible. Image-size, capture-mode, room-revision and
pose-expiry checks prevent reuse across incompatible configurations. The floor
overlay lets you review placement visually. A moved camera still needs alignment
again, even when its identity and image size are unchanged.

Objects on shelves, occluded bases, lens-estimation error and uneven floors can
invalidate the estimate. Only floor objects in a single-level room are supported.
The approach point is 0.8 metres short of the object toward the robot. The existing
planner checks its route using live Go2 observations. Blocked approaches fail
rather than automatically trying other sides.

Robot localization reads the same model's transformed mesh in metric Y-up
coordinates. It keeps architecture and furniture separate and excludes movable
doors and chair boxes, matching the native controller's geometry conventions.
The controller includes its saved scan references. Suggested poses need a visual
confirmation before becoming the navigation alignment.

## Movement and timing

Walks stop after 30 seconds or 5 metres of accumulated travel. Stale camera or
tracking observations, a changed target, browser loss, a changed room revision,
and robot observation faults stop movement. Nothing restarts it automatically.
The preview expires after ten seconds. Failed Start requests attempt Stop too,
since a response can be lost after the command was accepted.

The app uses the same supervised arrival timing as macOS for rounded Go2 timestamps. Capture age remains unverified. It enables the same under-body and unobserved-floor allowances as macOS after opening the robot session. Detected obstacles and drops still block movement. Readiness failures are shown, never
silently bypassed. Look around may also refuse to turn without the observations
needed by the native controller. Keep the physical stop control available.

Camera freshness measures encoded-video delivery and local processing time, not
verified ESP32 exposure time. Fresh visual tracking avoids treating VLM completion
time as a new measurement, but cannot prove an image's age inside upstream buffers.
Physical latency and stopping distance require hardware measurement.

The arrival bark plays in the browser after the controller reports arrival near
the reviewed endpoint. It does not play on Go2 or imply object pickup. Audio
attribution is in [audio/SOURCE.txt](audio/SOURCE.txt).

## Verification

Tests cover visual projection, USDZ transforms, VLM result validation, current-frame
tracking, H.264 decoding, localization confirmation, rotation-search bounds, route
admission, stale data, stop failures, and arrival reporting. Controller tests use
a fake twin; they do not establish physical navigation accuracy.

```sh
uv venv .venv --python 3.11
uv pip install --python .venv/bin/python -r requirements.txt \
  grpcio-tools==1.76.0 pytest==8.3.5 httpx==0.28.1
.venv/bin/python -m grpc_tools.protoc -Iproto --python_out=. --grpc_python_out=. \
  proto/video.proto proto/unitree.proto proto/calibrated.proto
.venv/bin/python -m pytest tests -q
```

The proto copies retain the authoritative packages and field numbers. The
calibrated-frame proto's import path is adjusted to the local `video.proto` name.

`esp-camera-02` was removed from the Thor's pairings and verified connected on the
AGX Orin. Its stable camera identity, room model, saved camera placement and local
Qwen3-VL installation were verified. No physical walking or rotation test has been
performed by this development session.

### Camera-assisted initial position

If Locate Go2 or Look around cannot resolve the room match, choose **Mark Go2 in camera** after confirming a camera placement with measured lens calibration and connecting Go2. Estimated lenses cannot be used for robot marking; reviewing the floor grid does not convert an estimated calibration into a measured one. Keep the robot still. Click the floor midway between its front feet, then midway between its rear feet. Do not click the elevated body. Check the blue map marker and heading against the live image, then confirm within 15 seconds.

This is an operator-confirmed manual alignment, not an automatic camera detection or a verified LiDAR match. Projection uses the saved camera calibration; inaccurate calibration or marks produce inaccurate positions. The preview rejects invalid floor projections and front/rear separations outside 25–85 cm. Stop cancels the preview. Navigation still requires a separate route preview and Start walk, with existing obstacle checks.

### Recalibrate camera in the browser

Choose **Recalibrate camera** beneath the live image to rerun room-reference
alignment. The web app and Apple clients send full-range luminance frames to
`POST /v1/camera-alignment` on the Linux twin. The service chooses the lens,
checks two fresh frames, and saves metadata with a revision check. Keep the
camera fixed. After success, review the new floor grid. Stop or changing the
selected room/camera cancels pending work. A failed solve keeps saved metadata.

A matching measured lens is retained. A measured lens for a different capture
mode requires the matching mode or a new lens calibration. With no measurement,
the service tries a joint lens/pose fit, a compatible stored estimate, then a
broad field-of-view search. Estimates retain assumed provenance and cannot be
used for manual robot marking. This button does not replace the iPhone screen
calibration when room references cannot constrain the lens, and does not fix
Go2 automatic localization.

Deploy the matching Linux twin service before this app. The native solver and
OpenCV runtime are bundled with the twin; the web image has no solver copy.
Calibration decisions are tested in `go/internal/twinvision` and the transaction
lifecycle in `go/internal/twin`. Python tests cover capture and UI cancellation.
