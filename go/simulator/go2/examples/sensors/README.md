# Go2 sensor dashboard

This app uses the [shared native Go2 interfaces](../README.md) on hardware and
in the simulator. The source manifest uses host discovery; managed VM deployment
normalizes it to guest loopback. Sensor clocks must satisfy the shared freshness
requirements. Use a CLI built from this checkout.


A read-only browser app showing the robot's front camera, planar lidar scan,
odometry trail, IMU readings and joint angles. Each sensor reports capture age
and its delivery rate over the last two seconds. Run it alongside roaming,
patrol, teleoperation or manual sandbox controls; it needs no control grant.

## Deploy to a Go2 VM

Create and start a [Go2 simulator](../../README.md#create-or-configure-a-simulator),
then run from this directory:

```sh
wendy run --device vm:<simulator-name> --build-type docker --no-restart
```

Wendy opens the dashboard after the app is ready. You can also open
`http://127.0.0.1:8904`. With the default VM user networking, Wendy forwards
the app's HTTP port automatically. A VM using shared networking is reached at
`http://<vm-ip>:8904` instead. The app listens on all interfaces and is intended
for a trusted local simulator network.

The container subscribes to the Go2 VM's native Go2 ROS bus using Humble,
CycloneDDS and domain 0. It publishes no commands and includes no simulator
implementation. No Python packages beyond the ROS image are needed; camera
pixels are encoded as PNG with the standard library.

In a ROS-enabled shell on the same bus, you can also run:

```sh
python3 dashboard.py --host 127.0.0.1 --port 8904
```

This requires the simulator's ROS publishers; a standalone browser preview
without ROS cannot supply this dashboard.

## Try it

1. Drive the robot in the sandbox and watch the estimated path grow. The view
   keeps about two minutes of odometry, with a one-metre grid. Large pose jumps
   or gaps clear the stored trail. **Clear trail view** hides earlier points in
   this browser only.
2. Move the sandbox obstacle and watch the lidar returns change. Forward is up
   and left is left. Unknown, infinite and out-of-range returns are omitted;
   the coverage percentage shows how much of the scan is valid.
3. Disable the camera, pause lidar, or pause the world in the sandbox. After
   600 ms without a fresh capture, the affected sensor turns stale. Stale camera
   images and lidar returns disappear. Retained motion readings stay visible
   with their sensor's stale indicator.
4. Restore sensors to resume observation automatically. This app never moves
   or rearms the robot.

| Topic | Display |
| --- | --- |
| `/camera/color/image_raw` | Actual `rgb8` robot camera exposures, browser refresh up to 4 Hz |
| `/utlidar/cloud_base` | Projected obstacle returns in `base_link`, nearest return and valid coverage |
| `/utlidar/robot_odom` | Position, speed, heading and trail in `odom` |
| `/utlidar/imu` | Acceleration and angular velocity in `imu_link` |
| `/joint_states` | Named joint positions in radians |

Odometry includes drift and is not a map. The dashboard measures received
messages, not the simulator's internal production rate. Wrong frames, malformed
payloads, repeated captures, captures older than 600 ms, and timestamps more than
50 ms in the future are rejected. Delayed messages retain their capture age.

`GET /api/status` returns a JSON snapshot; `GET /camera.png` returns the latest
fresh exposure or HTTP 503 while no fresh camera frame is available. The page
uses locally served assets and no CDN.

## Check without ROS

```sh
../../.venv/bin/python -m pytest -q test_dashboard.py
```

The raw RGB camera and JointState panels are optional standard ROS extensions.
They remain waiting on a physical Go2 without those bridge publishers; native
cloud, odometry and IMU panels do not depend on them.
