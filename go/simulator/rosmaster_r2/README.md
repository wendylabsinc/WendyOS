# ROSMASTER R2 simulator

Simulate a Yahboom ROSMASTER R2 with the Jetson Nano B01 board. The car uses
Ackermann steering, rear-wheel drive, and a rectangular collision footprint.
The browser renders a 3D indoor world with obstacles, front-wheel steering,
wheel rotation, a driven path and the same lidar returns published over ROS 2.
The detailed car has treaded tires, red rims, black chassis plates, exposed Nano
electronics, a lidar housing and depth-camera lenses. Physically based materials,
studio reflections, tone mapping and soft shadows light the model on a textured
concrete floor. Orbit view starts close to the car; top view shows the course.
All geometry, textures and reflections are generated locally in the browser.

This models the car and its ROS interfaces. It does not emulate the Nano CPU,
GPU, CUDA, JetPack, or the motor controller's firmware. The runtime runs on
the host CPU or a WendyOS ARM64 VM. Geometry and sensor mounts are approximate;
see [compatibility.json](compatibility.json) and [UPSTREAM.md](UPSTREAM.md).

## Run locally

The simulator needs Python 3.10 or newer, NumPy and Pillow for the RGB/depth sensor.
From this directory:

```sh
python3 -m pip install numpy pillow
python3 -m r2_sim.server
```

Open <http://127.0.0.1:8890>. Set `R2_PORT` to use another port. This local
mode has no ROS bridge. WebGL 2 is required; all viewer assets ship locally.

Choose **Enable keyboard controls**. Hold W/S or up/down to drive forward or
reverse, and A/D or left/right to steer. The arrow buttons also support touch.
Space stops and revokes control. Moving the steering at rest changes the wheel
angles but cannot rotate the chassis. Losing browser focus stops browser control.

Orbit, top and driver views show the same world. The RGB and depth panels show the server-rendered camera. The driver view uses
the same approximate mounting position, with browser lighting. Drag to orbit, right-drag to pan and scroll
to zoom. Pause and reset revoke commands; resume then enable controls again.

## Run with ROS 2

Build and start a disposable local container:

```sh
docker build -t wendy-rosmaster-r2:dev .
docker run --rm --name r2-sim -p 127.0.0.1:8890:8890 wendy-rosmaster-r2:dev
```

The image includes ROS 2 Humble, rclpy, CycloneDDS, tf2 and a small rclcpp command
receiver. The receiver supplies the DDS publisher identity and capture timestamp
that Humble's Python callbacks omit. It starts in ROS
control mode. The first fresh `/cmd_vel` publisher owns control until you stop,
reset, pause, or choose another controller. To drive from a second terminal:

```sh
docker exec r2-sim bash -lc 'source /opt/ros/humble/setup.bash; export RMW_IMPLEMENTATION=rmw_cyclonedds_cpp CYCLONEDDS_URI=file:///opt/wendy-r2/cyclonedds.xml; ros2 topic pub --rate 10 /cmd_vel geometry_msgs/msg/Twist "{linear: {x: 0.4}, angular: {z: 0.2}}"'
```

Commands expire after 300 ms. `/cmd_vel` uses forward velocity in `linear.x`
and yaw rate in `angular.z`. The model converts yaw rate to steering and bounds
it to 30 degrees. `linear.y` must be zero. A zero-speed yaw request stops;
the R2 cannot turn in place. Applications should publish at 10 Hz or faster.
After a browser stop, reset or pause, explicitly choose **Use ROS 2** to rearm.
DDS publisher identity prevents another publisher from mixing in commands.
Old, reordered and future-dated messages are rejected using their DDS timestamps.

| Topic | Type | Nominal rate |
| --- | --- | --- |
| `/cmd_vel` | `geometry_msgs/msg/Twist` | Input |
| `/odom` | `nav_msgs/msg/Odometry` | 50 Hz |
| `/scan` | `sensor_msgs/msg/LaserScan` | 10 Hz |
| `/imu/data` | `sensor_msgs/msg/Imu` | 50 Hz |
| `/joint_states` | `sensor_msgs/msg/JointState` | 50 Hz |
| `/tf` | `tf2_msgs/msg/TFMessage` | 50 Hz |
| `/camera/color/image_raw` | `sensor_msgs/msg/Image`, rgb8 | 10 Hz |
| `/camera/depth/image_raw` | `sensor_msgs/msg/Image`, 16UC1 millimetres | 10 Hz |
| `/camera/color/camera_info`, `/camera/depth/camera_info` | `sensor_msgs/msg/CameraInfo` | 10 Hz |
| `/tf_static` | `tf2_msgs/msg/TFMessage` | Transient local |

`base_link` is the rear axle center projected onto the ground, with X forward,
Y left and Z up. TF supplies `odom` to `base_link` and the fixed `laser_frame`
and `imu_link` mounts. Joint states contain four wheel positions followed by
the two front steering angles. Odometry and IMU are ideal model observations,
not calibrated hardware estimates. The IMU includes gravity. Lidar samples
360 simultaneous rays against the collision world. Use `use_sim_time=false`;
timestamps use wall-clock capture time and there is no `/clock` topic.

The container's ROS bus uses loopback. Run standalone test applications in its
network namespace, or use a managed Wendy VM for normal app deployment.

## Use a managed Wendy simulator

Build the CLI and VM agent from this checkout. Choose **Yahboom ROSMASTER R2**
in the simulator picker, or use:

```sh
wendy vm create my-r2 --profile rosmaster-r2
wendy vm robot start my-r2
wendy vm robot open my-r2
```

The agent must advertise `rosmaster-r2-virtual-robot`. For an older VM agent,
build and install this checkout's agent before starting the robot:

```sh
make -C go build-cli build-agent-linux-arm64
./go/bin/wendy --device vm:my-r2 device update --binary go/bin/wendy-agent-linux-arm64
./go/bin/wendy vm robot start my-r2
```

Run the build commands above from the repository root. For an ordinary existing
VM, use `wendy vm robot configure NAME --profile rosmaster-r2`. Profiles pin
their source digest; `wendy vm robot update NAME` applies a rebuilt CLI's runtime.
`status`, `reset`, and `restart` use the same commands as the Go2/G1 profiles.
`status --json` reports the verified sandbox URL; each VM gets its own host port.

Managed mode isolates UDP DDS to VM loopback, then drops NET_ADMIN before
starting ROS. Declared ROS applications deployed with `wendy run --device
vm:my-r2` use that bus. Use ROS 2 Humble, CycloneDDS, domain 0 and host discovery
in the application's `wendy.json`. The reserved runtime app ID is
`sh.wendy.simulator.rosmaster-r2`. No Unitree message overlay is required.

## Apps with hardware drivers

For a complete autonomous app, try [R2 Explorer](../../../Examples/R2Explorer).
It builds an observed map and drives using lidar, depth and odometry, with a
dashboard for bounded runs and explicit stop control.

The simulator does not emulate USB camera firmware or the STM32 serial device.
Apps can declare a `simulation` backend in each service of `wendy.json`. Wendy
selects it only on the matching managed robot VM, before building or deploying.
Physical devices keep the normal environment and entitlements. A different robot
profile is an error. The service image must implement the declared backend.

```json
"simulation": {
  "profile": "rosmaster-r2",
  "entitlements": [{"type": "network", "mode": "host"}],
  "env": {"R2_SIMULATOR_URL": "http://127.0.0.1:8890"}
}
```

The simulator grants replace the service's hardware grants. Environment entries
overlay its normal environment. CLI `--env` overrides still take precedence.
The CLI resolves this configuration in memory and does not rewrite the manifest.

An app claims control with `POST /api/app/claim`, then posts `token`, an increasing
integer `sequence`, `speed` in m/s and `steering` in radians to
`/api/app/command`. Positive steering turns left. Commands expire after 300 ms.
`stop: true` stops immediately and retains the app session. The simulator's Stop,
Reset, Pause, and controller selection revoke it. Select **Use app** to rearm.
Only one app, browser, or ROS publisher controls the robot at a time.

`/stream/color` and `/stream/depth` serve MJPEG from the obstacle world at 320×240
and up to 10 Hz. The depth preview uses a 0.2–4 m color scale. The raw depth
endpoint `/api/camera/depth.raw` returns little-endian uint16 millimetres, with
zero for invalid/out-of-range pixels. `/api/camera/info` describes approximate
intrinsics and mounting. RGB, raw depth, previews and ROS camera messages share
one capture. Rendering runs on the server, including when no browser is open.

## Validation

```sh
python3 -m unittest discover -s tests -v
```

The tests cover signed straight and curved motion, Ackermann wheel geometry,
limits, collisions and reverse recovery, ray distances, command expiry,
publisher ownership and reset revocation.

Run browser checks against a disposable simulator, with Playwright and Chromium
installed. `PLAYWRIGHT_MODULE` can point to another installation's `index.mjs`;
`PLAYWRIGHT_CHROMIUM_EXECUTABLE` can select an installed Chromium binary.

```sh
node tests/viewer.browser.mjs http://127.0.0.1:8890
```

For real DDS validation, add `-v "$PWD/tests:/opt/wendy-r2/tests:ro"` to the
disposable container's `docker run` command. Run `tests/ros_acceptance.py` there
after sourcing ROS and exporting the same CycloneDDS environment used by the
drive command. It subscribes to every
dynamic observation topic, drives forward and backward, and verifies that command
expiry, reset and stop revoke motion. These tests reset the simulated world.

This is a kinematic simulator with separate acceleration and braking limits.
Tire slip, suspension, uneven terrain, `Rosmaster_Lib` serial commands, voice,
and factory firmware are unsupported. Camera optics and depth noise are idealized.
