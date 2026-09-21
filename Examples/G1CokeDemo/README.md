# G1 Coke scene and policy app

Spawn the supplied MuJoCo scene in a WendyOS VM and run the Coke policy against
its live camera and joint observations. The scene contains three desks, a can,
and the original 43-joint G1 with Dex3 hands. The browser has an orbitable 3D
view, RGB camera, visible-can mask, measured lift and placement distance, and
run/pause/reset controls.

This example runs a separate robot under `/coke/g1` on ROS 2 Humble. The existing
[managed G1 simulator](../../go/simulator/g1/README.md) keeps its 29-joint
locomotion model and port 8890. The Coke app uses port 8892. Its training model
has a fixed pelvis and waist equality constraints, so it performs stationary
manipulation.

## Prepare the supplied assets

The source checkout excludes binary models, checkpoint weights and rollouts.
Prepare them from the two local bundles supplied with the demo:

```sh
cd Examples/G1CokeDemo
python3 prepare-assets.py \
  --expert ~/Downloads/g1-expert-sim-replay-20260915 \
  --runtime ~/Downloads/g1-reference-residual-runtime
python3 prepare-assets.py --check
```

`assets.lock.json` pins the 17 required files by size and SHA-256. The script
validates all inputs before copying about 201 MiB into ignored `expert/` and
`bundle/` directories. It preserves valid existing files and fails if an
existing file differs. It does not download assets or require a GPU.

The runtime bundle's original manifest also describes files absent from the
supplied runtime handoff. This example copies only the pinned subset needed
for inference and its golden trace. The live scene comes from the complete
expert bundle.

## Spawn the scene in WendyOS

Use a CLI built from this checkout and an existing WendyOS VM:

```sh
../../go/bin/wendy vm start g1-sim --detach
../../go/bin/wendy run --device vm:g1-sim --dockerfile Dockerfile --build-type docker
```

Open <http://localhost:8892> and select a controller. The app builds a CPU-only
ROS Humble image with software EGL rendering. No GPU passthrough is required.
If the VM is already running, omit `vm start`.

The app can also run in a generic WendyOS VM. It carries its own scene and
does not require `wendy vm robot start`. Select that VM with `--device vm:NAME`.
For a standalone container on a Linux Docker host, use host networking so the
operator API remains reachable on host loopback:

```sh
docker build -t g1-coke-demo:dev .
docker run --rm --network host g1-coke-demo:dev
```

## Hardware in the loop

The **Jetson policy** controller keeps MuJoCo physics, camera rendering and
the browser in the Mac VM. A separate Jetson or Spark service runs the learned policy
and returns all 43 joint targets. The VM still applies those targets through
its ROS bridge. The Jetson service has no physical robot command interface.

Prepare the assets as above, then run from this directory with a CLI built
from this checkout:

```sh
../../go/bin/wendy run --device vm:g1-sim --hil
```

`--hil` opens a cloud inference device picker, even when only one device is
online. Omit `--device` to choose the main simulator interactively as well. To skip the picker, supply the device directly:

```sh
../../go/bin/wendy run --device vm:g1-sim --hil="YOUR_JETSON"
```

`YOUR_JETSON` is an online Wendy Cloud device name, numeric asset ID, or full
`cloud://HOST/org/ORG_ID/asset/ASSET_ID` selector. You must already be logged
in to that cloud organization. `wendy cloud discover` lists available devices.
Non-interactive runs and `--yes` require `--device vm:NAME` and `--hil=DEVICE`.

Use `--build-host` to pick a device for remote builds, or
`--build-host=DEVICE` to select one directly. Without this flag, Wendy uses
the configured build host default, or builds locally if none is configured.

This is native CLI support. Wendy reads the `hil` section in `wendy.json`,
stages the inference sources with their own configuration, and deploys
`g1-coke-hil` to the selected inference device. It then creates a local cloud tunnel on
an available port, waits for the inference health endpoint, starts the
existing `g1-sim` VM if needed, and deploys the simulation with its policy URL
set automatically. There is no separate script or tunnel command to run.

Keep the command running while using the demo. Ctrl+C closes the managed
tunnel and ends the attached simulator run. The inference app remains on the
remote device. `--detach`, `--deploy`, `--watch`, and multi-service selection are not
supported with `--hil` because the CLI owns the live connection.
The VM must use QEMU user networking. An existing VM running with shared
networking is rejected without restarting it.

Open the URL printed by Wendy, normally <http://localhost:8892>. **Jetson
policy · hardware in the loop** is selected automatically. Choose the
one-second run and click **Run demo**. The page reports Jetson compute time,
the full request/reply duration, and the selected inference devices.

Wendy selects the inference build from the remote GPU architecture. Spark
`sm_121` uses `hil/spark.stagefile.yaml`, Python 3.12 and PyTorch 2.9.1 with
CUDA 13.0 wheels. The `hil.buildFilesByGPUArch` map in `wendy.json` configures
this override; other GPUs use `hil.buildFile`.

The default Stagefile uses Wendy's device-aware CUDA support for Orin `sm_87`, including
Orin on WendyOS with JetPack 7.2. The current CLI selects a CUDA 12.6 runtime and
Orin-compatible PyTorch 2.8 wheel. The service defaults to CUDA vision and CPU
recurrent control, and fails at startup if CUDA is unavailable. On-device
CUDA and cloud execution still need validation; local parity tests use
PyTorch 2.7.1 on CPU.

### Connecting to an existing inference service

You can also connect the VM to an inference service that is already running:

```sh
../../go/bin/wendy run --device vm:g1-sim --dockerfile Dockerfile \
  --env COKE_POLICY_URL=http://JETSON_IP:8098
```

For a manually managed Wendy Cloud tunnel on the Mac:

```sh
../../go/bin/wendy cloud tunnel 18098:8098 --device "YOUR_JETSON"
```

The VM's URL is then `http://10.0.2.2:18098` under QEMU user networking. A
simulation running directly on the Mac uses `http://127.0.0.1:18098` instead.
The native `--hil` command manages this addressing automatically.

### Timing, resets and failures

Each request binds the joint observation, camera exposure and original
attempt-000001 reference step together. New camera tensors are sent at 20 Hz
of simulation time, losslessly compressed; the intervening control step reuses
the Jetson's cached embedding. The uncompressed tensor is 1.54 MB per exposure,
so bandwidth can also limit cloud runs. Actual compression depends on the image.

The VM advances one 25 ms simulation step only after receiving its matching
target. Network latency slows wall-clock execution. This mode tests policy
behavior on the real inference hardware; it does not establish that the
complete robot control loop meets a real-time deadline.

Pause/resume preserves recurrent state. Reset and a new run create a new
episode, clearing the GRU, residual controller, acceleration history and
camera cache before the next inference step. The service caches the last
reply, so a transport retry does not run inference twice. Stale steps,
changed retries, mismatched checkpoints and invalid targets stop the run.
The client retries a failed connection once; `COKE_POLICY_TIMEOUT` controls
each attempt. After a service restart or persistent connection failure,
reset the scene and start a new run. Use one simulation per inference service.

### Local transport check

You can test the same connection without a Jetson. In separate terminals:

```sh
./run.sh hil-serve --device cpu --port 18098
./run.sh serve --local --policy-url http://127.0.0.1:18098
```

Then exercise a short HIL run:

```sh
uv run python tests/smoke_live.py --mode hil --steps 40
```

## Controllers and source provenance

- The learned controller uses the supplied update-2,525 CNN/GRU/actor and
  residual controller. Camera inputs contain native RGB, optical depth and a
  visible-can segmentation mask. Joint inputs contain all 43 ordered joints.
- Expert replay applies the supplied recorded targets with the original PD
  gains and native contact physics.
- External ROS mode accepts named joint targets from another application.
- Jetson policy runs the same scene policy through the simulation inference
  service, over a LAN connection or cloud tunnel.

The learned controller keeps the original 7,518-step reference timing, or
187.95 simulation seconds. Expert replay uses the supplied smooth 60-second
retiming. CPU rendering may run slower than real time.

Both built-in controllers send targets through the ROS trajectory topic
before MuJoCo applies them. Their neural inputs use copied scene observations;
the same observations are also published for other ROS applications.

The visible scene is the exact **attempt-000001** model. The checkpoint and
sealed golden trace use **attempt-000159**. Running the checkpoint in this
scene is a separate evaluation. The UI reports measured lift and distance;
the source recording's success does not establish success of a new policy run.
The stage-champion source adds a phase input to continued training, but no
later checkpoint accompanies it. This app preserves the supplied checkpoint's
original 316-input architecture.

## ROS interface

Wendy connects the app to CycloneDDS domain 0 with loopback discovery inside
the VM. Other applications can subscribe under `/coke/g1`. Sensor timestamps
use simulation time; consumers should set `use_sim_time=true`.

| Topic | Type and data |
| --- | --- |
| `/coke/g1/joint_states` | `sensor_msgs/JointState`, 43 named positions, velocities and efforts |
| `/coke/g1/joint_trajectory` | `trajectory_msgs/JointTrajectory`, command input |
| `/coke/g1/camera/color/image_raw` | `sensor_msgs/Image`, RGB8 |
| `/coke/g1/camera/depth/image_rect_raw` | `sensor_msgs/Image`, 32FC1 depth in metres |
| `/coke/g1/camera/color/camera_info` | `sensor_msgs/CameraInfo` |
| `/coke/g1/camera/depth/camera_info` | `sensor_msgs/CameraInfo` |
| `/coke/g1/perception/can_mask` | `sensor_msgs/Image`, visible can pixels |
| `/coke/g1/perception/can_visible` | `std_msgs/Bool` |
| `/coke/g1/imu/data` | `sensor_msgs/Imu`, fixed-base orientation and gravity-specific force |
| `/coke/g1/odom` | `nav_msgs/Odometry`, fixed-base pose |
| `/coke/g1/simulation/epoch` | `std_msgs/UInt64`, reset generation |
| `/coke/g1/simulation/status` | `std_msgs/String`, JSON status |
| `/tf` | `tf2_msgs/TFMessage`, links and camera frames prefixed `coke/g1` |
| `/clock` | `rosgraph_msgs/Clock` |

`/coke/g1/simulation/reset`, `/pause`, and `/resume` are
`std_srvs/Trigger` services. They acknowledge queuing; read status to confirm
completion.

Each command contains one trajectory point naming all 43 joints exactly
once. The bridge maps by name and rejects non-finite, out-of-range or stale
targets. This interface applies position setpoints; it does not interpolate
multi-point trajectories. The original controller holds the legs and left
hand at their initial positions while upper-body and right-hand targets
drive the manipulation.

## Local development

After preparing the assets:

```sh
./run.sh                       # Local browser preview without ROS
./run.sh verify-policy         # All 100 recorded policy parity checks
uv run python -m pytest -q     # Python regressions; ROS checks need ROS packages
node --test tests/test_ui.mjs  # Browser command recovery regressions
uv run python tests/run_ros_vm.py --cli ../../go/bin/wendy
uv run python tests/smoke_live.py --mode policy --steps 40
```

The VM transport check uses temporary files and an isolated ROS domain. It
checks real DDS messages without changing the running scene. The HTTP smoke
check starts a short simulation run in the deployed app.

The importer tests need only Python and no assets:

```sh
python3 -m unittest discover -s tests -p test_prepare_assets.py -v
```

`coke_demo/scene.py` loads the exact model and exposes target-driven physics,
camera observations and browser geometry. `coke_demo/ros_bridge.py` supplies
the ROS interface. `coke_demo/service.py` runs inference and the operator API.
Original policy runtime sources are kept in `runtime/`; prepared bundle
sources retain their recorded hashes. The local Three.js and OrbitControls
files include their MIT license in `ui/vendor/three.LICENSE`.

See [validation results](VALIDATION.md) for measured outcomes and test scope.

### Control access

The operator UI and physical control services listen on literal loopback
addresses. Access them through an authenticated Wendy tunnel. Their local HTTP
protocol trusts processes on the device; run them only on a trusted device,
without untrusted tenants. Operator confirmation expresses intent and does not
authenticate a caller.

A HIL inference listener on a non-loopback address requires `COKE_HIL_TOKEN`.
Supply the same secret to the inference server and simulator. Use Wendy's
authenticated tunnel for remote transport. Raw camera tensors and joint states
are sent to the selected inference device.

Camera producers must issue a new stream ID after a restart that resets frame
IDs. The vision buffer invalidates cached and in-flight encodes when it sees a
new stream ID.

Use `wendy run --hil` to deploy the inference companion. Wendy generates a fresh
per-run token and supplies it to both the inference server and simulator.

The HIL base image is digest-pinned and the Orin runtime uses the validated
PyTorch 2.7.1 version. A different GPU/runtime still needs the documented policy
parity checks before physical qualification.

For `wendy run --hil`, `hil.tokenEnv` names the environment variable that
receives a fresh secret on both deployments. The CLI uses that secret for its
health request. `hil.healthSchema` requires the JSON health response to identify
the expected inference protocol before starting the simulator. The selected
cloud organization and asset remain the trusted inference destination.
