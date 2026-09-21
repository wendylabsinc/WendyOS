`ros_timing.py` measures actual physics and producer counters after a warmup. It
does not replace the separate ROS/SDK/navigation apps or their ten-minute VM
acceptance gate. Run inside the runtime image after sourcing both ROS overlays:

```sh
source /opt/ros/humble/setup.bash
source /opt/wendy-go2/ros_ws/install/setup.bash
GO2_NATIVE=1 LP_NUM_THREADS=2 python3 tools/ros_timing.py \
  --warmup 5 --seconds 30 --render --profile --output results/full.json
```

Use the profile's loopback-only Cyclone configuration. Run one benchmark runtime
at a time and stop other simulator containers/VM apps before comparing renderer
settings. Full mode renders the 640×360 robot sensor camera, publishes actual
camera frames, and emits the five-ring lidar cloud. The sandbox receives scene
state and renders in the browser. Producer counters include `camera_frames`;
this benchmark does not measure browser rendering performance.

The clean local ARM64 Docker comparison on 2026-09-12 used the pinned MuJoCo,
policy, and generated ROS overlays with a fixed Python source snapshot. That
historical snapshot rendered both camera and observer images on the CPU. Its
original measurements are retained below and predate the browser 3D renderer:

| Mesa workers | Physics Hz | Policy Hz | LowState Hz | IMU Hz | Lidar Hz | Observer FPS | ROS camera FPS | CPU cores |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 499.95 | 49.99 | 482.27 | 196.97 | 10.02 | 8.82 | 8.82 | 1.65 |
| 2 | 499.99 | 50.02 | 500.02 | 200.03 | 9.99 | 14.59 | 14.59 | 1.56 |
| 4 | 499.97 | 50.01 | 500.00 | 199.99 | 9.99 | 14.45 | 14.42 | 1.56 |

Two workers are the image default. The two- and four-worker runs had no queue
overflows or expired snapshots during their measured 30-second intervals. Small
counter differences at the interval edges reflect samples already pending when
the initial status snapshot was read; physics states are never duplicated.

Local reports are `results/timing/full-lp1.json`, `full-lp2.json`, and
`full-lp4.json`; `results/timing/matrix-source.json` records the image, environment,
and exact source hashes. Results are ignored build artifacts. This comparison
has no external DDS subscribers, so subsequent VM acceptance must include real
readers for native state, images, point clouds, and standard observations.
