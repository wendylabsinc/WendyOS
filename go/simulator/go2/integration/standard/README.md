# Standard ROS integration harness

This is a separate Wendy application for the Go2 simulator VM. It subscribes
to real ROS observations and commands `/cmd_vel`; it does not import the
simulator or call a motion HTTP endpoint. Run only after the parent simulator's
placement/performance gate and ROS integration are ready. No deployment has
been performed merely by adding this harness.

The app requires guest host networking, explicit domain 0, CycloneDDS, and
loopback-only ROS discovery. `wendy.json` supplies those choices. The simulator
HTTP endpoint defaults to `http://127.0.0.1:8890`; only loopback HTTP endpoints
are accepted. Before any mutation it requires `simulation: true`, `robot: go2`,
and `profile_version: 1` in `/api/status`.

The run takes approximately 35 seconds, resets the virtual world, and:

- creates a zero-command publisher at 20 Hz, selects its unique newly observed
  ingress identity, and explicitly calls `/api/arm_ros`;
- verifies odometry, IMU, joint, scan, ground-truth and coherent TF data;
- measures source-stamp rates, requiring at least 50 Hz IMU, 25 Hz odometry,
  joints/ground truth, and 5 Hz lidar scan;
- proves signed forward/backward, lateral, and yaw motion from ROS odometry,
  with changing joint states and range observations;
- stops publishing and verifies command expiry and physical settling;
- keeps the old publisher sending through world reset, checks rejection of its
  old identity, recreates the publisher and explicitly grants the fresh source.

The harness prints JSON assertions and a final result. Cleanup invokes
`/api/disarm_ros` to leave the simulator without ROS command ownership. Another
new publisher appearing during source discovery makes the run fail instead of
guessing which source to grant.

After deployment is authorized, from this directory:

```sh
wendy run --device vm:<simulator-name> --build-type docker --no-restart
```

This is a finite test application; disable automatic application restart. The
HTTP API must implement `/api/arm_ros`, `/api/disarm_ros`, and `/api/reset` as
documented by the simulator. The sensor contract uses `odom → base_link`,
`base_link → imu_link`, and `base_link → lidar_link`. Odometry and its TF must
match at an identical source timestamp. IMU acceleration includes specific
force; `/simulation/ground_truth` is separate from estimated odometry.

The full ten-minute gate uses `Dockerfile.soak`, which adds real readers for
`/camera/color/image_raw`, `/camera/color/camera_info`, `/utlidar/cloud`, and
`/lowstate`. The original lightweight Dockerfile still needs only standard ROS
packages. The soak image contains generated native interfaces, their licenses,
and the SDK's original 8.6 KB CRC source; it contains no MuJoCo, policy/model
assets, SDK transport package, or simulator Python modules.

Prepare the native overlay from the locally built managed simulator image. It
must match the target VM architecture. Preparation creates an inactive temporary
container, copies its compiled ROS install tree, removes the temporary
container, and records the exact image digest in `build/overlay/source.json`.
The generated `build/` directory is ignored. This ordinary build context works
with Wendy's separate buildx builder without publishing the local managed image
to a registry:

```sh
# Run from the repository's go/ directory, after building the managed image.
python3 simulator/go2/integration/standard/prepare_soak.py --image wendy-go2-managed:dev
wendy --device vm:go2-sim run \
  --prefix simulator/go2/integration/standard \
  --build-type docker --builder docker --dockerfile Dockerfile.soak \
  --no-restart --user-args=--soak-seconds,600 --yes
```

`Dockerfile.soak` enables `--full-sensors` in its command and explicitly binds
Cyclone to the copied loopback-only profile configuration. Its entrypoint sets
`ROS_LOCALHOST_ONLY=0` to avoid Humble adding a second loopback interface. The
harness verifies that the XML contains only `lo`, loopback peers, and disabled
multicast. The ordinary standard image continues using `ROS_LOCALHOST_ONLY=1`.

The additional readers check actual payload lengths/frames, camera calibration
and matching exposure stamps, image hashes, finite 3D returns, and a same-stamp
cloud horizontal ring matching the scan. Every LowState is received; every tenth
receipt (about 50 Hz) is independently checked using the pinned SDK's unaltered
native-record packer and bitwise CRC. The source hash is verified during image
build and startup. This oracle does not use the simulator's faster CRC. LowState
has no wall-clock ROS header: freshness instead compares its millisecond tick
with current physics time and checks tick advancement. Other source stamps and
all receipt timestamps must remain fresh.

The sustained report retains all original movement, joint/torque, contact,
command-watchdog, ownership, and 600-second checks. It additionally requires
received LowState ≥475 Hz, Image/CameraInfo ≥14 Hz, and PointCloud2 ≥9.5 Hz,
alongside IMU ≥190 Hz, odometry/joints ≥47.5 Hz, lidar ≥9.5 Hz, policy ≥47.5 Hz,
real-time factor ≥0.95, and command latency p95 <100 ms. Motion
continues through `/cmd_vel` throughout; all streams have real DDS readers in
the separately deployed app. A shorter `--soak-seconds` run provides diagnostics
but cannot pass the 600-second acceptance gate.

The report derives `camera_fps` from the runtime's `camera_frames` counter.
Sandbox rendering runs in the browser and has no observer-frame production gate;
the sensor image receipt gate above still applies. Verify the browser separately
with `tests/viewer.browser.mjs`, as described in the [simulator README](../../README.md).
