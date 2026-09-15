# Go2 command ingress

This standalone ROS 2 Humble package receives the following commands and
forwards attributed samples to a local Unix datagram receiver:

| Topic | ROS type | Envelope kind |
| --- | --- | --- |
| `/cmd_vel` | `geometry_msgs/msg/Twist` | `twist` |
| `/api/sport/request` | `unitree_api/msg/Request` | `sport` |
| `/api/motion_switcher/request` | `unitree_api/msg/Request` | `motion_switcher` |
| `/lowcmd` | `unitree_go/msg/LowCmd` | `lowcmd` |

It does not execute a policy, arm a robot, or publish actuator commands. Runtime
integration and simulator deployment are deliberately separate work.

The subscriber uses best-effort, volatile, keep-last 1 QoS. It can match either
best-effort or reliable writers, drops unsupported/nonfinite Twists, and keeps
no outgoing retry queue. Unix sends are nonblocking, with a 4096-byte maximum.
Missing, full, or failed receivers cause a visible throttled warning and drop
the sample. Logs use a steady clock even if ROS simulation time is paused.

## Socket contract

`GO2_COMMAND_SOCKET` selects an absolute filesystem path; the default is
`/run/wendy-go2/commands.sock`. The receiving runtime creates, owns, restricts
access to, and removes the socket/directory. This process never binds or unlinks
that path. It resolves the destination on every send, allowing a receiver to
replace its socket. Set the DDS domain/interface environment before starting
the executable; the Unix socket does not configure or confine DDS discovery.

Each datagram is UTF-8 JSON:

```json
{
  "kind": "twist",
  "publisher_gid": "<hex of all RMW_GID_STORAGE_SIZE bytes>",
  "node_name": "wendy_go2_patrol",
  "node_namespace": "/",
  "source_timestamp_ns": 123456789,
  "received_ns": 987654321,
  "velocity": [0.35, 0.0, 0.4]
}
```

For native kinds, `payload_hex` replaces `velocity`. Its value is the complete
ROS CDR serialization produced by `rclcpp::Serialization` for the table's
message type, including the CDR encapsulation header. The Python runtime can
decode it with `rclpy.serialization.deserialize_message(bytes.fromhex(value),
Request)` or `LowCmd`, using the same pinned generated packages. Native CDR is
limited to 1920 bytes so its hex plus JSON metadata fits the datagram. Oversized
unbounded Request strings/sequences are rejected before serialization; the
serialized size is checked again before hex encoding. All 20 LowCmd motor
slots, header fields, remote bytes, and CRC survive unchanged.

Native payloads are transported without interpreting API IDs, parameters,
motor targets, or CRC. A decodable message is not authorization to execute it:
native finite values, limits, CRC, API support, and mode/owner requirements
belong to the runtime's admission layer.

`node_name` and `node_namespace` are optional display metadata. They come from
an exact DDS endpoint identity match in the ROS graph. Names never identify a
control owner or grant. Each name is limited to 128 ASCII characters and must
follow ROS name rules. If adding names would exceed the datagram limit, the
ingress omits them and preserves the full native payload.

The velocity contains body-frame forward, lateral, and yaw rates. Nonzero
vertical, roll, or pitch components are rejected. Numeric speed/acceleration
limits belong to runtime admission. `source_timestamp_ns` is the actual RMW
writer timestamp; preserve zero if the middleware cannot supply it.
`received_ns` records C++ callback entry using the Linux steady clock and is
comparable to a Python receiver's `time.monotonic_ns()` on the same kernel. It
does not establish how long the sample waited in DDS. The publisher GID comes
from `rclcpp::MessageInfo`, not ROS graph-name heuristics.

The tested `rmw_cyclonedds_cpp` 1.3.4
returns an opaque publication handle in message-info GID bytes, while ROS graph
endpoint GIDs contain DDS GUIDs. They do not compare equal; this is
[upstream issue 377](https://github.com/ros2/rmw_cyclonedds/issues/377). The ingress
keeps the received GID unchanged for runtime grants. To obtain a display name,
it traverses at most 128 DDS entities in its own process and ROS domain, then
calls `dds_get_matched_publication_data` with the received publication handle.
The returned writer GUID must exactly match a ROS graph endpoint. This uses
public CycloneDDS APIs and no private RMW structures or node-name guesses.
[CycloneDDS matched endpoint implementation](https://github.com/eclipse-cyclonedds/cyclonedds/blob/0.10.5/src/core/ddsc/src/dds_matched.c)
resolves handles through the reader's shared domain entity index.

Metadata refresh runs once per second in a separate callback group and executor
thread. Command callbacks only read the cache. The cache is limited to 4096
sources, and unresolved or conflicting names are omitted. FastDDS uses the
direct exact GID match. Runtime grants always use the IDs received with
commands, and an ingress restart remains an ownership boundary.

## Admission, reset, and stale data

The receiver must validate the complete envelope, its byte limit, native CDR
type for each kind, finite numbers and bounds, source identity, and monotonic
receipt age before commanding the simulator. A Unix sender must not cause
implicit arming.

A publisher GID identifies an endpoint, **not** a control grant or world epoch.
The envelope intentionally makes no claim to carry a trustworthy current
epoch: an unstamped `Twist` does not contain one. Runtime integration must:

1. Explicitly grant control to a selected publisher GID for the current epoch,
   retaining the corresponding runtime token privately.
2. On pause/reset, revoke that grant and token before changing the world;
   reject queued samples by their source identity and timestamps.
3. Reject the revoked GID across the reset boundary. Require a fresh publisher
   endpoint before a new grant, or an explicit producer reset/flush handshake
   that establishes a new command generation. Never grant control to the next
   incoming command automatically.
4. Reject callback timestamps older than the grant and enforce the independent
   monotonic watchdog. Where the selected middleware supplies valid source
   timestamps, check DDS source age as well; callback freshness alone cannot
   rule out delayed samples.
5. Reject duplicate or reordered source/receipt timestamps for each publisher,
   so older traffic cannot replace a newer command or renew its watchdog lease.

Replacing the Unix socket alone does not fence queued DDS samples or a sender
that keeps publishing. Tests for the future receiver must keep the old ROS
publisher active through pause/reset and verify it cannot reactivate motion.

## Build and validate

From a ROS 2 Humble environment, with this package and the pinned
`unitree_api`/`unitree_go` packages in a colcon workspace:

```sh
colcon build --packages-up-to go2_command_ingress
colcon test --packages-select go2_command_ingress --event-handlers console_direct+
colcon test-result --verbose
```

The CTest integration test creates a temporary Unix socket and actual Python
ROS publishers, verifies stable and distinct source IDs (plus graph equality on
FastDDS), and checks exact names with simultaneous nodes and multiple publishers
on one node. It checks timestamps and finite planar values and exercises receiver
backpressure and disappearance/recreation. Native tests deserialize actual CDR
using generated Python types, compare all fields and fixed arrays, check source
identity stability, and verify oversize rejection followed by recovery. It
needs no simulator VM, physical device, model, or renderer.

`Dockerfile.test` installs the build/test dependencies, including
`ros-humble-rosidl-generator-dds-idl`, which the pinned upstream CMake files
require without declaring in their package manifests. From the repository root:

```sh
docker build -t wendy-go2-ingress-test:dev \
  -f simulator/go2/ros_ws/src/go2_command_ingress/Dockerfile.test \
  simulator/go2/ros_ws/src/go2_command_ingress
docker run --rm --network none \
  -v "$PWD/simulator/go2/ros_ws/src:/src:ro" \
  wendy-go2-ingress-test:dev bash -c \
  'colcon build --base-paths /src --packages-up-to go2_command_ingress &&
   colcon test --base-paths /src --packages-select go2_command_ingress --event-handlers console_direct+ &&
   colcon test-result --verbose'
```

The source mount is read-only; generated build/install files stay in the
ephemeral container. Tests use domain 73 inside its isolated loopback network.

Humble's Python subscription callback does not expose publisher metadata, which
is why command ingress uses C++. References:
[Humble rclpy executor](https://github.com/ros2/rclpy/blob/humble/rclpy/rclpy/executors.py),
[rclcpp MessageInfo](https://github.com/ros2/rclcpp/blob/humble/rclcpp/include/rclcpp/message_info.hpp).
