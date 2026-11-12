# Go2 ROS bridge command deadlines

`python3 .github/scripts/test-go2-bridge-deadlines.py --output "$RUNNER_TEMP/go2-bridge-deadlines"`

Run this in the existing `simulator-python310` **go2 row only**, after its Python
requirements check and conditional `./.github/actions/setup-go-ci`. No new job,
runner, Docker builder, device, privileged container, or external robot is
needed. The existing Docker context must have a `docker` buildx driver. Upload
`go2-bridge-deadlines/*.json` and `*.log` with `if: always()`; the script removes
its compiled test binaries, containers, internal network and unique image tag.

The script builds the shipped bridge Dockerfile with its official ROS Humble
base pinned to the recorded multiarchitecture index. The existing pinned
Unitree message source and Python requirements remain unchanged. It builds one
normal services test executable and one Go overlay control. Native amd64 and
arm64 Docker hosts are supported. A cold native arm64 image build took about
two minutes locally; the image is approximately 1.05 GB. The normal and control test bodies take about 11 seconds combined once
compiled; container startup and owned-process cleanup add overhead. Allow space for the ordinary Go
module/build caches and Docker layers; no shared cache is pruned.

`TestGo2BridgeROSDeadlines` starts the production `UnitreeRobotService` with its
normal 200 ms command deadline and 500 ms move lease. The paired connection is
production `unitree.Connect`, including local HTTP signaling, validation and a
real Pion WebRTC/SCTP data channel to a protocol robot. Pairing ownership is
seeded privately for this fixture. The Python check runs the unchanged bridge
as another process, connects through a real Unix gRPC socket, and publishes and
subscribes through ROS Humble/CycloneDDS. Runtime networking is a fresh internal
Docker network; ROS discovery is loopback only. Containers run without Linux
capabilities, as the calling UID, with a read-only root filesystem.

The normal journey requires:

- A 1.2-second posture acknowledgement leaves the ROS executor responsive to
  an unsupported sport request, velocity input, fresh robot odometry and Stop.
- Stop completes while the posture acknowledgement is still pending.
- A non-refreshed Move produces an actual StopMove packet after the production
  500 ms lease; a successful gRPC return alone cannot pass this assertion.
- A robot Stop acknowledgement delayed 900 ms produces a failed ROS command
  response near the Agent's 200 ms deadline. The connection remains usable.
- The late Stop acknowledgement is actually transmitted while a different
  posture command remains pending, and cannot satisfy that command.

The synchronous-posture control changes only the bridge's posture callback to
run on the ROS executor. It must fail the concurrent callback assertion. The
no-lease Go overlay removes only the Agent's move expiry arming. It must fail
because the protocol robot receives no lease StopMove packet. Both controls
run in fresh containers and must reach their specific regression assertions.

This covers audit #2242's device-free ROS/Agent/robot transport seam. It does
not run MuJoCo, validate modern encrypted robot pairing, measure Wi-Fi loss,
prove physical posture completion, or establish hardware stopping distance.
Those hardware claims remain separate.
