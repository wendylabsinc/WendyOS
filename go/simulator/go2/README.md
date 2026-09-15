# Go2 virtual robot for Wendy Simulator

A Go2 profile turns a WendyOS VM into a robot development target. A managed
container runs MuJoCo, a pinned ONNX locomotion policy, ROS 2 Humble/CycloneDDS,
and a browser sandbox. Applications in that VM receive causal robot observations
and send ROS commands that move a freely walking, contact-supported robot.
Physics, policy inference and robot camera sensor rendering use the VM's CPU.
The sandbox renders through WebGL in the browser. The profile defaults to four
vCPUs and 4 GiB RAM on accelerated ARM64.

The managed VM passed a ten-minute run with real ROS readers and continuous
motion commands: 499.8 Hz native state, 199.9 Hz IMU, 10 Hz lidar, and 14.83 FPS
camera delivery. Native SDK, navigation, two-VM isolation and restart/reconnect
checks also pass. Exact scope and results are in [validation](validation/README.md).

## Create or configure a simulator

Use a CLI built from this checkout. In the Simulator tab, create a simulator
and select **Unitree Go2**, then connect to it. The first connection builds the
runtime from pinned sources, downloads verified assets, deploys it, and waits
for robot readiness. Docker must be available on the development host.

The agent must advertise `go2-virtual-robot` support. Older VM images get an
explicit update instruction before any runtime build. Agent maintenance stays
available through the named VM even when robot startup fails. During development,
use an agent built from the same checkout: `make build-agent-linux-arm64`, then
`wendy --device vm:go2-sim device update --binary bin/wendy-agent-linux-arm64`.
A published supporting agent release can use ordinary `device update`.

The diagnostic CLI supports the same workflow:

```sh
wendy vm create go2-sim --profile go2
wendy vm robot start go2-sim
wendy vm robot open go2-sim
```

For an existing ordinary VM, use `wendy vm robot configure <name>` before
`wendy vm robot start <name>`. Configuration preserves the VM disk and existing
applications. Each VM gets its own verified loopback sandbox URL; use
`wendy vm robot status <name>` to find it. Do not assume a shared host port.

The persisted profile pins the runtime source and policy bundle. When they
differ from the CLI, interactive connections such as `wendy run` offer to update
the runtime and reset its robot world, then continue after it is ready. Declining
cancels the connection. For noninteractive runs (including `run --yes`), update
first with `wendy vm robot update <name>`.
Reconnecting with the same runtime preserves a running world's state; a stopped runtime
is restarted. `wendy vm robot restart <name>` explicitly recovers a failed
runtime. `wendy vm robot reset <name>` resets the world and revokes command
ownership. Stopping a user application leaves the managed robot running.

## Deploy a ROS application

Explore the [sample apps](examples/README.md):

- [Roam](examples/roam/README.md): autonomous obstacle avoidance with Start/Stop controls.
- [Patrol](examples/patrol/README.md): a finite square or configurable waypoint route.
- [Teleop](examples/teleop/README.md): browser driving with hold-to-drive keys and touch controls.
- [Sensors](examples/sensors/README.md): camera, lidar, odometry trail, IMU and joint dashboard.

Each is a standalone ROS application deployable to a Go2 VM. Roam also has a
local runner for the standalone browser preview. The read-only sensor dashboard
can run alongside any driving app.

Declare ROS in the application's `wendy.json`:

```json
{
  "frameworks": {
    "ros2": {
      "distro": "humble",
      "rmw": "rmw_cyclonedds_cpp",
      "domainId": 0,
      "discoveryScope": "app"
    }
  }
}
```

Run `wendy --device vm:go2-sim run` from the application project. Wendy resolves
declared ROS applications onto the profile's guest host network and loopback
ROS bus, checking conflicting domain, middleware and discovery settings before
deployment. Source manifests are not rewritten. The managed runtime installs
guest firewall rules that confine UDP RTPS to loopback, then drops its network
administration capability. Application downloads and ordinary networking work.
Independent VMs have independent robot buses.

Kernels without nftables use verified legacy IPv4/IPv6 filters. That fallback
blocks UDP packets containing the `RTPS` marker anywhere, independent of DDS
domain and port; it can also block unrelated UDP carrying those bytes. The
agent loads fixed filter modules only for a validated managed Go2 runtime on a
WendyOS VM. Both address families must be protected before DDS starts.

Standard applications publish `geometry_msgs/msg/Twist` on `/cmd_vel`.
On a managed Go2 VM, `wendy run` starts a new driving app with control
automatically. Its first fresh command grants control to its DDS publisher,
replacing the previous walking or browser controller. Start a publisher with
zero velocity so handoff stops the previous command before the app moves.
Each publisher gets one automatic grant. Older publishers cannot take control
back by continuing to send commands. Exactly one browser or DDS publisher owns
actuation. Native sport and LowCmd applications still need **Give app control**
in the sandbox. The runtime uses the actual middleware publisher identity;
it does not guess ownership from a node name.
Automatic handoff requires the robot to be standing or walking in sport mode.
It does not switch out of native joint control or a posture transition.

Automatic handoff follows publisher startup, including apps started outside
`wendy run`. Restarting the runtime discovers still-running publishers again,
and the last newly discovered eligible publisher takes control. Standalone
runtimes use manual grants by default; set `GO2_AUTO_APP_CONTROL=1` to enable
the same automatic handoff. The sandbox's source selector and **Give app
control** remain available to grant an eligible publisher control when the
robot has no owner.

The source selector shows ROS node names, including **Patrol**, **Roam**, and
**Teleop** for the samples. If a name is unavailable, it uses a stable numbered
label such as **Velocity app 1**. The full publisher identity is available in
the option's tooltip. Status updates preserve your selection and leave an open
selector alone. An app marked **restart app** needs a new publisher after a
pause, reset or release before it can receive control.

| Observations | Nominal rate |
| --- | --- |
| `/lowstate`, `/lf/lowstate` | 500 / 50 Hz |
| `/sportmodestate`, `/lf/sportmodestate` | 50 Hz in sport mode |
| `/imu/data`, `/utlidar/imu` | 200 Hz |
| `/joint_states`, `/odom`, `/simulation/ground_truth` | 50 Hz |
| `/scan`, `/utlidar/cloud` | 10 Hz |
| `/camera/color/image_raw`, `/camera/color/camera_info` | 15 Hz, 640×360 RGB |

`/tf` supplies `odom → base_link`; `/tf_static` supplies the IMU, lidar,
camera and optical mounting transforms. A localization node can own
`map → odom`. Device mode uses original wall-clock capture timestamps and
does not publish `/clock`; applications use `use_sim_time=false`.

Native interfaces use generated `unitree_go` and `unitree_api` types. The
supported sport subset includes version queries, Move, StopMove, BalanceStand,
physical StandUp/StandDown, and Damp. LowCmd supports twelve active motors in
the twenty-slot SDK layout, CRC checks, stop sentinels and exclusive external
joint control. Unsupported APIs return explicit errors. Native applications
must include their SDK or message dependencies; Wendy's typed ROS inspector
uses the runtime's pinned overlay.

## Sandbox and command lifetime

Choose **Enable controls**, then hold W/S to walk, A/D to strafe, or Q/E to
turn. Space stops the velocity target. The sandbox renders the MuJoCo scene in
the browser using WebGL: scene geometry is loaded once, then pose state updates
move the robot and obstacle. Drag to orbit, right-drag or Shift-drag to pan,
and scroll to zoom. On a touchscreen, use one finger to orbit and two fingers
to pan or pinch to zoom. **Reset view** centers the current robot position and
restores the default viewing angle and distance.
Three.js and its controls ship with the runtime and need no external CDN.

**Follow robot** starts enabled and moves the viewpoint with the robot while
preserving your chosen angle and offset. Turn it off to inspect a fixed part of
the room. **Show lidar** displays the actual sensor returns in the 3D world.
With ROS enabled, these are the same sampled points published to
`/utlidar/cloud`. The standalone sandbox uses the same MuJoCo ray sampler.
Paused, disabled or expired lidar observations disappear from the view; sensor
dropout changes the displayed returns too.

The independent **Robot camera** view displays a JPEG encoding of the actual
MuJoCo front camera exposure. ROS `/camera/color/image_raw` publishes that
exposure's raw RGB pixels and capture timestamp. Moving the sandbox viewpoint
does not change the sensor camera. The browser also offers an adjustable
obstacle and lidar/camera fault controls.
Sensor pauses stop new samples while physics continues; lidar dropout applies
the same seeded mask to scan and cloud. Fault settings persist across world reset.

Velocity commands expire after 200 ms and are acceleration-limited to
0.8 m/s forward, 0.5 m/s lateral and 1 rad/s yaw. External LowCmd expires after
40 ms and enters damping. It never falls back into autonomous walking.
Pause, reset and **Release app control** revoke grants and block existing
publishers. Resume the world before restarting a driving app to get a new
automatic grant. A publisher first seen while the world is paused or otherwise
unable to accept control does not receive an automatic grant later. Resume
alone does not rearm controls. Fallen robots require reset.

HTTP endpoints are `/api/health`, `/api/status`, `/api/profile`, `/api/scene`,
`/api/scene/state`, `/api/scene/lidar` and `/camera.jpg`. The scene endpoint
supplies geometry and materials; scene state supplies the current poses for
local browser rendering. The lidar endpoint supplies the sampled sensor origin
and world-space returns with their capture identity and freshness.
The browser requests state at up to 30 Hz and interpolates poses between updates.
`GO2_VISUAL_DETAIL` selects the same full or balanced meshes for both the browser
and robot camera; physics always retains the original model.
Status reports the simulation identity, source digest,
control owner, world epoch, sensor settings and measured performance. A ready
agent connection alone does not mean the robot is ready.

## Compatibility and validation

The versioned [compatibility manifest](compatibility.json) describes exact
types, nominal rates, command IDs, error codes and fidelity limits. It is also
served at `/api/profile`. [UPSTREAM.md](UPSTREAM.md) records model, policy,
message definitions, derived wire-layout fixes, visual assets and licenses.

Joint, inertial, contact, camera and lidar observations derive from MuJoCo.
Odometry integrates velocity with a declared bias and nonzero covariance;
exact pose is a separate simulation topic. The lidar pattern and camera
mount are virtual attachments. Battery telemetry is a documented constant
synthetic model. Factory gait parity, calibrated sensors, depth, native video
codecs, motion-switcher mutations, and cloud/audio/AI services are unsupported.

Development checks, from this directory:

```sh
python3 tools/fetch_assets.py
python3 tools/fetch_assets.py --check
python3 tools/fetch_unitree.py
python3 tools/prepare_unitree.py
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements-dev.txt
.venv/bin/python -m pytest -q
docker build -t wendy-go2-managed:dev .
```

The production Dockerfile generates and verifies visual assets on Linux ARM64
and builds the derived ROS overlay. Original physics assets remain unchanged.
The sandbox requires a browser with WebGL 2 support. Managed Linux runtimes use
OSMesa for robot camera sensor rendering. Local macOS development can use
MuJoCo's offscreen CGL backend when the process has access to macOS graphics.

With the development dependencies and assets above available, verify the browser
viewer against a disposable local simulator. The test resets the world and
sends robot commands. Start a separate runtime on port 8899 from this directory.
On macOS:

```sh
MUJOCO_GL=cgl GO2_RENDER=1 GO2_PORT=8899 .venv/bin/python -m go2_sim.server
```

Inside the Linux runtime image, use `MUJOCO_GL=osmesa` and the image's `python3`
for this command; expose port 8899 on loopback for the browser test.

In a separate terminal, also from this directory:

```sh
node tests/viewer.browser.mjs http://127.0.0.1:8899
```

The control-page regression runs without a simulator and uses mocked status
responses to check selection, publisher names, and field editing:

```sh
node tests/controls.browser.mjs
```

The test requires Playwright and its Chromium browser. If Playwright is installed
outside this project, set `PLAYWRIGHT_MODULE` to its local `index.mjs` path.
When the verified `assets/visuals` directory is available, add
`GO2_VISUAL_DETAIL=balanced` to test the managed image's lighter visual model.
The check exercises orbit, pan, zoom, reset framing, follow controls, lidar,
state updates, reconnects and the real front camera preview. It requires
`GO2_RENDER=1`. For manual state-only development, `GO2_RENDER=0` leaves the 3D
sandbox available but disables the sensor camera and cannot pass this full test.

To inspect the same disposable runtime, open `http://127.0.0.1:8899`, keep
**Camera active** enabled, and select
**Robot camera**. Pausing the world pauses new camera exposures. The preview
reports whether rendering is disabled, the sensor is disabled or paused, the
renderer failed, or the first exposure is still pending. The optional camera
checks exercise real pixel rendering and the retained exposure's JPEG encoding:

```sh
MUJOCO_GL=cgl GO2_TEST_RENDER=1 .venv/bin/python -m pytest -q tests/test_camera.py
```

Inside the Linux runtime image, use `MUJOCO_GL=osmesa` for the same checks.

Separate applications exercise real DDS delivery and commands:

- [Standard ROS and VM soak](integration/standard/README.md): signed walking,
  sensors, command expiry, reset rejection, and a ten-minute performance gate.
- [Native SDK acceptance](integration/native/README.md): actual SDK requests,
  stock ROS decoding, motor ordering, CRC and low-level watchdog behavior.
- [Navigation acceptance](integration/navigation/README.md): goal arrival,
  obstacle stopping and stale-scan behavior through a small reactive controller.
- [DDS isolation](integration/isolation/README.md): IPv4/IPv6 packet delivery,
  loopback access, firewall ownership and capability removal.

Recorded results live in `validation/`; producer timing reproduction is in
[tools/ros_timing.md](tools/ros_timing.md). Nominal rates and local producer
benchmarks do not by themselves establish the deployed VM performance gate.
The [implementation plan](../../docs/plans/2026-09-12-go2-virtual-robot.md)
tracks remaining acceptance work. No physical robot is used for these tests.
