# Unitree ROS 2 inspection

The standalone Wendy inspector includes upstream `unitree_go` and `unitree_api`
from commit `668d1ec5a05d1c38d3306bdca7d59f2ba3581a88`. It builds their Python,
Fast DDS and Cyclone DDS typesupport against Humble. It does not apply the Go2
simulator's message changes. The archive hash and license ship with the build.

Both the read-only host inspector and the system CLI source this overlay.
Their version label causes old stock-ROS sidecars to be replaced on the next
inspection, after active commands finish. App inspection still uses the app's
own graph and packages. To inspect hardware while an app is running, use explicit
host scope with an explicit domain through the MCP inspection tools or
`lidar sample`.

## Topic coverage

| Discovered topic types | Support |
| --- | --- |
| `sensor_msgs/msg/PointCloud2`, `LaserScan`, `Imu` | Standard ROS types, including `/utlidar/*` and `/uslam/*` clouds |
| `unitree_go/msg/LidarState`, `HeightMap` | Native inspection, including `/utlidar/lidar_state` and `/utlidar/height_map_array` |
| `unitree_go/msg/AudioData` | Packet inspection and publication, including `/audioreceiver` and `/audiosender` |
| `unitree_api/msg/Request`, `Response` | Native `/api/*` messages, including audiohub and voice |
| `unitree_go/msg/LowState`, `SportModeState`, `WirelessController`, `UwbState`, `UwbSwitch` | Native inspection |

This pinned upstream package does not define `ConfigChangeStatus` or
`VoxelMapCompressed`. It also does not supply `unitree_arm`,
`unitree_interfaces`, or the Nav2 message packages in the supplied robot topic
inventory. Those types still require the corresponding firmware/vendor packages.
Topic discovery alone does not establish wire compatibility with every firmware.

## LiDAR sampling and SLAM

After discovering the robot's topic and domain:

```sh
wendy device ros2 lidar sample /utlidar/cloud_deskewed --scope host --domain 0 --sample-points 64
wendy device ros2 lidar sample /scan --scope host --domain 0 --message-type sensor_msgs/msg/LaserScan
```

The command uses the same bounded probe as MCP `ros2_lidar_summary`. It returns
JSON lines with point counts, sampled XYZ coordinates, range sectors, source
timestamps and frame information. All accepted points contribute to statistics;
`--sample-points` limits only the returned XYZ selection. `--target-frame` applies
TF at the measurement timestamp. It reports unknown when a transform is missing.
Use the actual discovered body frame for robot-relative observations.

These summaries are for inspection. A SLAM node should subscribe to the complete
PointCloud2 stream, plus the appropriate IMU, odometry and TF, on the same DDS
domain. Configure its Wendy app with `discoveryScope: "host"` and host networking.
Do not relabel a cloud's frame or substitute a small XYZ sample for a full scan.
For offline SLAM, record the selected cloud, IMU and transforms with
`wendy device ros2 bag record`, then download the bag. The existing bag recorder
preserves serialized messages rather than YAML previews.

## Audio input and output packets

The inspector can sample either native audio topic and publish native packets.
Check endpoint publishers and subscribers to identify the direction on the
installed firmware. The names alone are not a microphone/speaker contract.

When the standalone system CLI is selected, with no ROS app running:

```sh
wendy device ros2 exec run wendy_ros2_inspection audio sample /audioreceiver --count 3
wendy device ros2 exec run wendy_ros2_inspection audio sample /audiosender --count 1 --include-data
```

Default output contains the native `time_frame`, byte count, SHA-256 and a
32-byte base64 preview. `--include-data` also returns the complete base64 payload.
Packets are limited to 1 MiB. Samples stop at the count or deadline and report
a nonzero exit status when data is missing. Neither raw bytes nor `time_frame`
are interpreted as PCM or a ROS timestamp. Upstream AudioData supplies no codec,
sample rate, channels or timestamp-unit metadata.

For an already encoded firmware-compatible packet, use the `publish` subcommand
with an explicit topic, `--time-frame` and `--data-base64`. It waits for a matching
subscriber and publishes exactly one packet, limited to 64 KiB so the base64
argument fits the Linux process argument limit. DDS acknowledgement does not prove
audible playback. There is no automatic recording-to-speaker connection, codec
conversion, microphone activation, or ALSA device emulation.

When apps are running, the MCP `ros2_topic_sample` tool with `scope: "host"` and
`domain_id: 0` can decode AudioData using the standalone overlay. The executable
above belongs to this inspector image; ordinary app images do not contain it.
Host-scoped inspection remains read-only. Full audio capture or playback through
the agent's audio-device APIs requires a separate firmware-specific bridge.

## Build and validation

From the repository root:

```sh
docker build -f go/ros2/inspector/Dockerfile -t wendy-ros2-inspector:review .
docker run --rm --network none -e ROS_LOCALHOST_ONLY=1 -v "$PWD:/repo:ro" \
  wendy-ros2-inspector:review python3 /repo/go/ros2/inspector/integration_test.py
docker run --rm --network none -e ROS_LOCALHOST_ONLY=1 -v "$PWD:/repo:ro" \
  wendy-ros2-inspector:review python3 /repo/go/internal/shared/ros2inspection/lidar_probe_integration.py
```

Tests use synthetic messages on an isolated bus. They verify native type loading,
lossless audio publication between Cyclone DDS and Fast DDS, missing audio, and
the existing LiDAR probe's geometry/TF handling. Hardware microphone encoding,
speaker playback and firmware schema compatibility require Go2 validation.

Before shipping an agent with this change, publish the amd64/arm64 manifest
`ghcr.io/wendylabsinc/wendy-ros2-inspector:humble-unitree-v1` using the inspector
workflow's manual `publish` option. Make the package publicly pullable. Change
both the image tag and `ros2InspectorVersion` when changing the runtime contract;
do not overwrite a released version with incompatible types.
