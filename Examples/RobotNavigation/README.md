# RobotNavigation

A deployable Wendy app implementing robot readiness, supervised local navigation,
and calibrated person approach. Wendy supplies a goal over MCP; Nav2 plans and
controls on the device. A separate velocity guard and motor process enforce
expiring commands. **The default configuration emits no motor commands.**

This adds the application layer for steps 4–6 of the robotics work. It does not
install a sensor driver or create depth/odometry from an ordinary camera snapshot.

## Test locally

From this directory:

```sh
python3 -m unittest discover -s tests

docker build --target test -t robot-navigation-test .
docker run --rm --network none robot-navigation-test
docker run --rm --network none robot-navigation-test python3 tests/ros_smoke.py
```

The ROS smoke test runs real Nav2 and MCP against a planar simulator inside a
container with no external networking. It overrides motor output to
`/simulation/cmd_vel`, checks readiness and idempotent goals, then tests sensor
loss, cancellation, lease expiry, unknown-space rejection, and calibrated person
approach with target loss. The pure tests run without ROS, OpenCV or the Unitree SDK;
ROS perception tests skip when those dependencies are unavailable.

The ROS perception tests publish synthetic RGB-D, camera calibration, person
detections and capture-time transforms inside that isolated container. They check
grounding, image/ID correspondence, physical standoff allowances, target loss and
expiry. These tests do not establish sensor calibration or stopping performance
on Woof.

Build the deployment image with `docker build -t robot-navigation .`. To deploy
the default observation-only configuration using this branch's CLI:

```sh
../../go/bin/wendy run --device Woof --dockerfile Dockerfile
```

Wendy discovers the app's MCP tools after deployment. Its HTTP endpoint binds
only to `127.0.0.1:8128/mcp`; the authenticated device agent proxies it. ROS/DDS
participants on the configured network are trusted; this is not DDS access
control. Configure the actual robot domain in `wendy.json` before deploying.

## Fixing “no ROS 2 app is running”

The Wendy CLI and device agent on this branch support an explicit host diagnostic
scope. An app is no longer required for these read-only MCP calls:

```json
{"scope":"host","domain_id":0}
```

Use that with `ros2_topics`, substituting the device's **known** ROS domain.
Then use `ros2_topic_info`, `ros2_topic_sample` and `ros2_topic_hz` with the same
scope/domain and a discovered topic. Host inspection starts its own stock Humble
inspector; it needs the updated device agent as well as the CLI. An older agent
is detected and rejected instead of silently inspecting a different graph.

The stock inspector discovers topic names/types and decodes installed standard
message types. Custom Unitree messages still need matching typesupport in an
app. No samples means unknown, including a wrong domain/interface or absent
publisher. Native DDS topics do not become standard ROS sensor messages merely
because the graph is discoverable. For battery information, Wendy's `device_info`
can use native device telemetry independently of any ROS 2 app.

If RobotNavigation is deployed with its `frameworks.ros2` declaration, normal
app-scoped inspection works too. See the [robotics diagnostics guide](../../go/internal/cli/assets/docs/integrations/robot-diagnostics.mdx).

## Required device inputs

Edit `config.json` (or mount a local configuration and set
`ROBOT_NAVIGATION_CONFIG`). All timestamps and transforms must share the device's
ROS clock. Frame names are checked; stale, future, replayed, partial or malformed
inputs are blockers.

| Input | Default | Contract |
| --- | --- | --- |
| Obstacle scan | `/scan`, `sensor_msgs/LaserScan` | Full 360° finite valid returns, calibrated planar scan-to-base TF, angular spacing no greater than 0.02 radians by default. Unknown rays block. `+inf` is accepted only after explicitly commissioning that sensor convention. |
| Odometry | `/odom`, `nav_msgs/Odometry` | `odom` → `base_link`, pose and actual twist, positive measured x/y/yaw covariance within configured bounds. |
| Attitude | `/imu/data`, `sensor_msgs/Imu` | Orientation already expressed for `base_link`, normalized quaternion and available orientation estimate; excessive roll/pitch blocks motion. “Upright” reports tilt, not standing/gait state. |
| Navigation TF | `odom` → `base_link` | Fresh dynamic transform from localization/odometry. No invented identity pose. |
| Rectified RGB | `/camera/color/image_rect` | `sensor_msgs/Image`, calibrated optical frame, exact detection capture stamp; `rgb8`, `bgr8` or `mono8`. |
| Aligned depth | `/camera/aligned_depth_to_color/image_raw` | Depth registered to that exact rectified RGB geometry; `16UC1` millimetres or `32FC1` metres. |
| Camera calibration | `/camera/color/camera_info` | Matching dimensions/frame; depth and CameraInfo stamps within 0.05 seconds of detections by default. Rectified `P` intrinsics with identity `R` and no stereo offset. Cropped/binned or other models reject until converted correctly. |
| Person detections | `/robot_navigation/people` | Humble `vision_msgs/Detection2DArray`, source image header and stable per-person track IDs. |

A sensor bridge for Woof must provide these standard interfaces and calibrated
transforms. A raw LiDAR point cloud is not a 360° free-space scan: points alone do
not identify unobserved rays, drop-offs or blind regions. The configured scan
plane, gait footprint and workspace must cover relevant obstacles. This example
assumes a commissioned flat navigation plane; it has no stair/drop-off planner.

The bundled optional OpenCV HOG detector downloads no model weights. It preserves
IDs only for unambiguous one-to-one box overlaps; gaps, disappearance, overlap
ambiguity and splits produce new IDs. These are spatial tracks, not proof of human
identity. HOG is a baseline for upright people; it can miss seated or occluded
people and cannot exclude posters or mirror reflections. Its sigmoid score ranks
SVM detections and is not a calibrated probability. Replace it with a
validated detector (`start_detector: false`) publishing the same message contract
for the intended environment. Reflective surfaces must be excluded independently;
even plausible depth is not proof a detection is a real person.

The built-in detector bounds work to one pending image, at most 640 × 480 pixels
and five processing attempts per second by default. `HOG_MAX_WIDTH`,
`HOG_MAX_HEIGHT`, `HOG_MAX_FPS`, `HOG_HIT_THRESHOLD` and `HOG_MAX_SOURCE_AGE` tune
those local limits. The launcher supplies `RGB_TOPIC` and `DETECTIONS_TOPIC` from
`config.json`. Slow inference expires its source frame instead of publishing a
fresh timestamp for old pixels.

## Motion configuration and lifecycle

Default `motion_enabled: false`, `watchdog_commissioned: false`, and
`motor.mode: disabled` keep hardware output inactive while reporting blockers.
To enable a commissioned installation, configure a motor backend and both local
flags. These values cannot be changed through MCP.

- `twist` forwards guarded commands to an existing robot driver configured in
  `motor.output_topic`. That driver must enforce its own command expiry.
- `unitree` lazily uses `unitree_sdk2py` `SportClient.Move` / `StopMove` on the
  explicit network interface. The default image does not include the optional SDK;
  missing dependencies report unavailable and emit no motion. The optional image
  below packages that dependency. This backend never issues StandUp or changes
  locomotion mode.

Build the optional Unitree image from this directory:

```sh
docker build -t robot-navigation:local .
docker build -f Dockerfile.unitree --build-arg BASE_IMAGE=robot-navigation:local -t robot-navigation-unitree:local .
../../go/bin/wendy run --device Woof --builder docker --dockerfile Dockerfile.unitree
```

[`Dockerfile.unitree`](Dockerfile.unitree) pins SDK commit
`db9b2d210081387fcd1e7ed9ac4c56a02983bb85`, verifies its archive checksum, and builds
the Cyclone DDS Python 0.10.5 binding against Humble's existing C 0.10.5 library.
It explicitly overrides the SDK's 0.10.2 Python dependency metadata, preserves
the image's OpenCV/NumPy packages, and restores a missing upstream package marker.
The network-disabled build check verifies imports and the loaded ROS library
path; it does not construct an SDK client or initialize discovery. This has been
tested on ARM64; physical Unitree transport, firmware compatibility and stopping
remain unverified.

The derived image inherits disabled motor configuration. Its deployment command
uses the locally built base image through the Docker builder; build that base for
the target architecture first, and rebuild it after each configuration change.
The earlier observation-only deployment command
explicitly selects the default Dockerfile. Configure `motor.mode: unitree`, the actual `network_interface`, and
the local commissioning flags only after the installation checks below.

The independent controller watchdog must be measured on the actual robot,
including loss of the motor process or host power. Set the enclosing **gait**
footprint, minimum clearance, braking deceleration, speed limits and reaction
budget from those measurements. The reaction budget must cover the guard timeout,
motor timeout and bounded command delivery. A physical emergency stop and a
standing robot under its normal balance controller remain operator prerequisites.

Nav2 uses rolling obstacle costmaps in `odom`, blocks unknown space, and replans
locally. The behavior tree contains planning and path following; it does not
clear obstacle maps or perform automatic backup/spin recoveries. The full inflated
footprint is checked along the route. Only single-pose goals are supported; the
separate Nav2 through-poses behavior is configured to reject waypoint requests.
Goals beyond observed free space may fail.
The separate guard conservatively stops for any obstacle inside an age-expanded
radial stopping envelope, including behind the robot.

All Nav2 velocity output goes to `/robot_navigation/unsafe_cmd_vel`, through the
guard to stamped `/robot_navigation/safe_cmd_vel`, then through the motor process.
An expired sensor, command or supervision permit produces zero output. A failed
child causes the app supervisor to terminate the other children. Motion requires
Nav2 to be managed by this app so an external action cannot survive its restart.

SQLite on `/data` persists goal IDs and outcomes. Restarted goals are marked
`abandoned` and never resumed; stopping is not claimed from persisted data. The
last 256 requests are retained by default, so use globally unique request IDs.

## MCP tools

| Tool | Behavior |
| --- | --- |
| `robot_status` | Readiness blockers, source/receipt ages, coverage, pose, measured velocity, limits, guard/motor state and active goal. |
| `robot_targets` | Up to 16 fresh calibrated observations, subject to a byte budget, with opaque target IDs, source frames/stamps, uncertainty and expiry; total/truncation fields identify omitted observations. |
| `robot_observe` | The matching RGB capture as a JPEG up to 40 KiB, with numbered labels mapped to exactly the target IDs included in the response. |
| `navigation_goal` | Submit `request_id`, `x`, `y`, `yaw`, `frame_id`, `max_speed`, lease and timeout. Identical retries return the original goal. |
| `navigation_status` | Read state and stopping evidence. Reading does **not** renew a lease. |
| `navigation_renew` | Explicitly renew supervision of an active goal, within its original total timeout. |
| `navigation_cancel` | Inhibit output and cancel Nav2. Remains stopping until the action ends and fresh odometry confirms a stopped dwell. |
| `robot_stop` | Cancel and latch the guard. Operator-local process restart resets the software latch; there is no remote reset tool. |
| `approach_person` | Select a fresh observed `target_id`, compute a protected standoff endpoint, and submit it to the same local planner. |

Person endpoints add requested physical standoff (minimum 1 m; default 1.5 m),
gait radius, depth and pose uncertainty, planner endpoint tolerance, and bounded
target drift. Those allowances describe the chosen endpoint; they do not prove
the actual separation is safe. A goal is rejected with
`no goal accepted: already_within_standoff` when that protected radius leaves no
meaningful forward approach. It does not cause automatic reversing or report
arrival. Accepted retries return the persisted request outcome without selecting
or grounding a different person.

Grounding always requires aligned depth. It samples the central torso and rejects
insufficient valid samples, material depth clusters, excessive spread, mismatched
geometry or timestamps, and positions inconsistent with the configured floor.
Targets expire after 0.75 seconds by default. A lost, invalid, expired or jumped
selection ID is retired; reappearance requires a new explicit selection. During
an accepted goal, target loss or excess movement cancels navigation instead of
switching to another person. Observations expose source/depth/TF timestamps,
position uncertainty and the unresolved mirror limitation.

A goal being submitted/accepted is not proof of arrival. `stopped_confirmed`
requires both terminal Nav2 acknowledgment and multiple fresh odometry samples
below the stopped thresholds. Uncertain action transport leaves the goal inhibited
and stopping until the backend is known to have ended.

The key upstream interfaces are [Nav2 NavigateToPose](https://api.nav2.org/actions/humble/navigatetopose.html),
[Humble vision_msgs](https://github.com/ros-perception/vision_msgs/tree/humble/vision_msgs/msg),
and the [Unitree Python SDK](https://github.com/unitreerobotics/unitree_sdk2_python).
