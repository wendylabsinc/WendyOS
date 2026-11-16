# Audit 2022 Patrol ROS boundary

Original `499da6870b3e027529aec0586e69c868ec996e68` added explicit scan-gap and capture-age opt-ins, their CLI forwarding and truthful sensor diagnostics. Existing policy/callback tests do not prove the ROS subscriptions or published velocity requests.

`go/ros2/go2-bridge/tests/patrol_transport_test.py` starts the unchanged `ros_app.py` entry point in a separate Python process with actual CLI flags. An independent rclpy participant publishes native `nav_msgs/Odometry` and `sensor_msgs/PointCloud2` on the remapped production topics and receives actual `unitree_api/Request` velocity commands and status messages. Both sensor subscriptions and the start service must be discovered before assertions begin. Each journey has unique topic names.

The five normal journeys require:

- Default mode rejects stale captures and partial clouds without ever publishing motion, then accepts current full coverage and stops after sensor silence under the unchanged 350 ms freshness policy.
- `--allow-scan-gaps` admits a measured front return, rejects rear-only coverage, resumes only through the actual Trigger service, and stops for a measured obstacle.
- `--ignore-capture-age` admits advancing captures from a clock 60 seconds behind, but rejects a repeated capture stamp.
- Both flags complete the real finite waypoint controller route. A small kinematic peer integrates received 0.55 m/s requests into published odometry; the application must reach completion and keep sending zero velocity afterward.
- Both flags still reject zero timestamps, then admit fresh valid data. The test does not rewrite or bypass the production cloud parser or exposure gate.

`test-patrol-ros-runtime.py` adds these checks to the existing Go2 bridge deadline image and private internal network, after the existing Agent/Pion checks. No additional image build or runner is added. Every application child has bounded SIGINT/wait cleanup; the existing outer container owner removes only nonce-labelled resources if a process cannot drain. Container mounts are readonly except its own evidence directory; it has no devices or capabilities. The normal journey has a 140-second outer bound; each of three controls has 35 seconds.

Two controls drop one forwarded CLI flag. A third deliberately admits malformed timestamps in arrival-time mode and must deliver actual nonzero DDS velocity before its named refusal assertion fails. Control acceptance requires the exact assertion, live sensor subscriptions, exit 1 and failed result evidence. Setup, timeout, import and child-cleanup failures cannot satisfy a control.

Run the existing gate unchanged:

```sh
python3 .github/scripts/test-go2-bridge-deadlines.py --output /absolute/new/evidence
```

Portable control-admission checks use `python3 .github/scripts/patrol_ros_runtime_test.py`. They do not establish ROS execution. The real container journey remains pending hosted execution until its normal and three controls pass. The kinematic peer proves transport and controller behavior, not MuJoCo locomotion, a deployed robot's physical velocity, perception accuracy or motor safety. Existing hardware acceptance remains separate.
