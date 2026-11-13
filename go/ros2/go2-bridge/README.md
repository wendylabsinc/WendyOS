# Go2 ROS 2 bridge (experimental)

Publishes a paired Unitree Go2 (tested design target: Go2 Air over Wi-Fi) as the
same ROS 2 contract as the [Go2 simulator](../../simulator/go2/README.md):
Humble, CycloneDDS, domain 0, `unitree_go`/`unitree_api` types built from the
simulator's pinned and patched sources, the same topic names and frames. Apps
written against the simulator run against the real robot unchanged.

```
Go2 ──WebRTC──▶ Wendy agent ──Stream/Command (agent socket)──▶ go2-bridge ──▶ ROS 2 domain 0
```

The agent keeps the robot connection and key. The bridge needs the `admin`
entitlement for now, to reach the agent socket. Later the agent is meant to run
this bridge itself.

The robot bus stays on the device. The bridge uses ROS 2 scope `device`
(loopback only) on domain 0. Apps that use the robot declare scope
`peripherals`, like the simulator examples, on domain 0 with host networking:

```json
"frameworks": { "ros2": { "distro": "humble", "rmw": "rmw_cyclonedds_cpp",
                          "domainId": 0, "scope": "peripherals" } },
"entitlements": [ { "type": "network", "mode": "host" } ]
```

On a device without a robot cable, `peripherals` is loopback only, which is
where the bridge is; the same manifest reaches a Go2 EDU on its own cable.
Nothing on the LAN sees the robot topics or can publish commands. Wendy's ROS
tools (`wendy device ros2 …`, Foxglove) join an app's network and discovery
settings, so they keep working. Agents without ROS 2 `scope` support ignore the
field and use loopback discovery, which reaches the bridge as well.

## Run

Pair the robot (`wendy device pair --unitree`), then from this directory:

```sh
wendy run --detach
```

With one paired robot nothing else is needed; with several set
`--env GO2_SERIAL=<serial>`. `--env GO2_EXTRA_TOPICS=rt/a,rt/b` requests more
robot topics, published as `/go2/raw/<topic>`. Every 10 s the log shows how many
messages per second arrive per robot topic, and how long turning a voxel map into clouds takes. Check with `wendy device ros2 topics`,
`wendy device ros2 hz /lowstate` or `wendy device foxglove serve`.

## Topics

| Topic | Type | Source |
| --- | --- | --- |
| `/lowstate`, `/lf/lowstate` | `unitree_go/LowState` | `rt/lf/lowstate` (IMU, motors, battery, foot force) |
| `/sportmodestate`, `/lf/sportmodestate` | `unitree_go/SportModeState` | `rt/sportmodestate`, `rt/lf/sportmodestate` |
| `/imu/data`, `/utlidar/imu` | `sensor_msgs/Imu` (`imu_link`, `utlidar_imu`) | lowstate IMU |
| `/joint_states` | `sensor_msgs/JointState` | lowstate motors, SDK order `FR/FL/RR/RL × hip/thigh/calf` |
| `/odom`, `/utlidar/robot_odom`, `/tf` `odom→base_link` | `nav_msgs/Odometry` | sport state position, IMU orientation, velocity |
| `/tf_static` | mounts | same values as the simulator |
| `/utlidar/cloud_base`, `/utlidar/cloud` | `sensor_msgs/PointCloud2` (`base_link`, `utlidar_lidar`) | LiDAR voxel map, one point per occupied voxel, moved into the robot frame with the latest pose (stamped with that pose) |
| `/go2/voxel_map` | `sensor_msgs/PointCloud2` (`odom`) | the same voxel map as the robot sends it |
| `/camera/color/image_raw` | `sensor_msgs/Image` `rgb8` | front camera, decoded only while subscribed |
| `/camera/color/h264` | `foxglove_msgs/CompressedVideo` | front camera H.264, unchanged |
| `/joy` | `sensor_msgs/Joy` | Unitree remote: axes `lx ly rx ry`, 16 key bits |
| `/go2/raw/<topic>` | `std_msgs/String` or `UInt8MultiArray` | every robot message unchanged, only while subscribed |
| `/api/sport/response` | `unitree_api/Response` | replies to `/api/sport/request` |

Commands: `/cmd_vel` (`linear.x`, `linear.y`, `angular.z`) and `/api/sport/request` with
Move (1008), StopMove (1003), RecoveryStand (1006), StandDown (1005) and Hello
(1016). Other APIs get status 3203. Speeds are capped like the simulator's:
0.8 m/s forward, 0.5 m/s sideways (`linear.y` / Move `y`), 1.0 rad/s turning.
The physical Go2 ignores forward requests below about 0.5 m/s. The agent stops a
move after 500 ms, so publish at least ~3 Hz while moving.

## Differences from the simulator

- LiDAR is the robot's ~5 cm accumulated voxel map, not per-scan points, so
  clouds show what the robot has mapped around it, not only the latest sweep.
  No `/scan`.
- Rates are what the robot sends over Wi-Fi (state about 50 Hz), not 500 Hz.
- No `camera_info` (the Air's camera is not calibrated here); the image is the
  robot's native resolution.
- `/odom` twist assumes the sport state velocity is in the world frame; to be
  verified on hardware.
- No control handoff between apps: the latest command wins.

## Simulator examples

The Go2 simulator's example apps (`go/simulator/go2/examples`: Sensors, Teleop,
Patrol, Roam, Hallway) run against this bridge with no code changes, with their default
strict capture-age and scan-coverage checks. `tests/simulator_examples_test.py`
checks that against a fake robot in a walled room that walks according to the
commands it receives: every example becomes ready and drives at 0.55 m/s; Patrol
completes its square and Roam stops before the wall. Not yet verified on the
physical robot (real LiDAR rate and coverage decide whether the strict checks
pass there).

## Tests

```sh
python -m pytest tests/test_convert.py            # conversions, no ROS needed
docker build -t go2-bridge . && docker run --rm -v "$PWD/tests:/opt/go2-bridge/tests:ro" \
  -e PYTHONPATH=/opt/go2-bridge go2-bridge /bin/bash -c \
  'source ros_ws/install/setup.bash && python3 tests/integration_test.py'   # fake agent, real ROS 2
docker run --rm -v "$PWD/tests:/opt/go2-bridge/tests:ro" -v "$PWD/../../simulator/go2/examples:/examples:ro" \
  -e PYTHONPATH=/opt/go2-bridge go2-bridge /bin/bash -c \
  'source ros_ws/install/setup.bash && python3 tests/simulator_examples_test.py'   # the simulator's examples
```

The LiDAR decoder is adapted from
[unitree_webrtc_connect](https://github.com/legion1581/unitree_webrtc_connect)
(`LICENSE-unitree_webrtc_connect`).
