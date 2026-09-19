# ROS goal-navigation acceptance

This finite Wendy app validates a real MuJoCo walking loop through DDS. Its
separate controller process reads `/goal_pose` (`PoseStamped` in `odom`),
`/odom`, `/imu/data`, TF and a laser scan, and publishes `/cmd_vel`. It has no
HTTP client or simulator imports. The controller is a bounded reactive goal
follower; it does not claim to validate Nav2 planning, mapping or recovery.

The acceptance driver verifies the virtual robot's HTTP identity before each
lifecycle operation. It resets the world, discovers exactly one new Twist
publisher, grants that specific source, and moves the actual sandbox box.
Every goal and motion command is sent through ROS. The app exits after its
checks and leaves ROS control disarmed.

The scenarios cover:

- Positive odometry covariance, coherent required TF/IMU frames, zero startup
  commands, physical joint articulation and goal arrival within 25 cm.
- A box placed in the walking path. The controller stops below 65 cm lidar
  clearance and holds zero commands, while the body stays clear of the box.
  Moving the box aside allows the same ROS goal to finish.
- Loss of the controller's scan input. The driver stops a ROS scan relay while
  the simulator continues advancing and publishing its original scan/odometry.
  The controller cancels its goal after 300 ms without a fresh scan and emits
  zero commands. Fresh scans alone cannot restart the cancelled goal; a new
  explicit ROS goal can.
- Actual 640×360 RGB frames, optical-frame camera calibration and changed
  pixels after physical walking, plus finite 3D lidar points. The scan must
  match the same capture's horizontal point-cloud ring.

The app validates source timestamps as well as receipt freshness, treats a
mostly missing forward scan as unknown space, and refuses goals outside the
bounded room or in an unsupported frame. It subscribes to the real `/scan`
through `/navigation/test_scan` only so the harness can interrupt delivery.
Standalone `python3 controller.py` reads `/scan` directly.

From this directory, deploy to the designated virtual robot:

```sh
wendy run --device vm:<simulator-name> --build-type docker --no-restart
```

For a local container test from the repository's `go/` directory:

```sh
docker build -f simulator/go2/Dockerfile.ros -t wendy-go2-ros:dev simulator/go2
docker build -t wendy-go2-navigation-test:dev simulator/go2/integration/navigation
docker run -d --rm --name wendy-go2-navigation-validation --network none -e GO2_NATIVE=1 -e GO2_RENDER=1 wendy-go2-ros:dev
docker run --rm --network container:wendy-go2-navigation-validation wendy-go2-navigation-test:dev
docker stop wendy-go2-navigation-validation
```

The runtime and test share an isolated network namespace with no external
interface or published ports. The default run requires camera/cloud topics.
`--without-perception` runs the motion scenarios during sensor development and
reports `full_sensor_acceptance: false`; it is not the full sensor gate.
The tests print JSON assertions and a final result, and fail with a nonzero
exit status. `--output /results/acceptance.json` writes the complete report,
including source hashes, when an output directory is mounted at `/results`.

The isolated motion run on 2026-09-12 passed in 42.26 seconds, with rendering
and native Go2 messages disabled. The robot moved 0.987 m to the first goal;
the obstacle hold drift was 0.028 m with at least 0.852 m between the body
center and the box surface. Scan-relay loss produced zero commands in 296 ms
while physics advanced 2.27 seconds. The generated report is
`results/navigation/motion.json` in the simulator directory.

The full sensor/navigation run then passed in 42.07 seconds with native Go2
state and rendering enabled. RGB/camera calibration, changed camera pixels,
1,183 finite 3D lidar points, and all 360 horizontal returns matching the same
scan capture passed. The first goal moved the body 0.962 m, obstacle hold
drift stayed below 0.018 m, and scan loss emitted zero commands after 342 ms
(the 300 ms freshness limit plus controller/delivery scheduling). The generated
report is `results/navigation/acceptance.json`; `camera.jpg` contains an actual
front-camera exposure. Tests ran with fixed source and visual-mesh snapshots
mounted in the isolated runtime. This establishes functional behavior. The
separate sensor-rate and VM performance gates remain independent requirements.

The controller's failure behavior can also be tested without ROS:

```sh
simulator/go2/.venv/bin/python -m pytest simulator/go2/integration/navigation/test_controller.py -q
```
