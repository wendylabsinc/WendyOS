#!/usr/bin/env bash
# Publishes with the agent's ROS 2 client and requires real ROS 2 Humble nodes,
# on both CycloneDDS and Fast DDS, to receive and decode the samples.
#
# Needs Docker and Go. The ROS 2 image is the pinned base the other ROS runtime
# checks use, plus the CycloneDDS middleware. The tests run twice: in a
# container with no network beyond loopback, and in one with a network whose
# ROS 2 nodes discover beyond loopback.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
base='ros:humble-ros-base@sha256:d9f14e84747bea639a65e00cf871d2c68ae55385d2acf6239614de0cadb41dcc'
image='wendy-rtps-publisher-interop:local'
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

case "$(docker info --format '{{.Architecture}}')" in
  aarch64 | arm64) goarch=arm64 ;;
  x86_64 | amd64) goarch=amd64 ;;
  *)
    echo 'unsupported Docker architecture' >&2
    exit 2
    ;;
esac

(cd "$root/go" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go test -c -o "$work/rtps.test" ./internal/rtps)
cp "$root/go/internal/rtps/testdata/ros2_interop_subscriber.py" "$work/"

docker build --quiet --tag "$image" - <<EOF
FROM $base
RUN apt-get update && apt-get install -y --no-install-recommends ros-humble-rmw-cyclonedds-cpp && rm -rf /var/lib/apt/lists/*
EOF

# An optional first argument narrows the run to matching tests, for debugging.
# WENDY_ROS2_INTEROP_VERBOSE=1 prints the publisher's discovery log.
selection=${1:-^TestROS2Node}

# run_pass NAME DOCKER_ARGS... runs the selected tests in one container and
# requires them to pass.
run_pass() {
  local name=$1 log="$work/$1.log" status=0
  shift
  echo "=== $name ==="
  docker run --rm --cap-drop=ALL --security-opt=no-new-privileges --read-only \
    --tmpfs /tmp:rw,nosuid,nodev,size=64m -e HOME=/tmp -e ROS_LOG_DIR=/tmp/ros-logs \
    -e WENDY_ROS2_INTEROP_SUBSCRIBER=/work/ros2_interop_subscriber.py \
    -e WENDY_ROS2_INTEROP_VERBOSE -e SELECTION="$selection" "$@" \
    -v "$work:/work:ro" "$image" /bin/bash -c \
    'source /opt/ros/humble/setup.bash
     for binary in /work/rtps.test; do
       "$binary" -test.run "$SELECTION" -test.v -test.count=1 -test.timeout 900s || exit 1
     done' \
    2>&1 | tee "$log" || status=$?

  # A skipped test is not a passing one: the subscriber path must have been set.
  if [[ $status -ne 0 ]] || grep -q -- '--- SKIP' "$log" || ! grep -q '^--- PASS: TestROS2Node' "$log"; then
    echo "$name: ROS 2 nodes did not receive the published samples" >&2
    exit 1
  fi
  if [[ $selection == '^TestROS2Node' ]] && ! { grep -q '^--- PASS: TestROS2NodesReceiveTemperatureSamples ' "$log" &&
    grep -q '^--- PASS: TestROS2NodesReceiveRawByteSamples ' "$log"; }; then
    echo "$name: not every ROS 2 interop test ran" >&2
    exit 1
  fi
}

# Loopback only: what an app with ROS 2 scope "app" or "device" has.
run_pass loopback-only --network none

# Wider discovery: what an app with scope "peripherals" or "network" has. A
# writer on loopback must still be found by these nodes. The CycloneDDS
# configuration is the one cycloneDDSBeyondDeviceConfig in
# go/internal/agent/containerd/client.go gives such an app; Fast DDS is only
# told not to stay on loopback.
run_pass wider-discovery -e WENDY_ROS2_INTEROP_LOCALHOST_ONLY=0 \
  -e CYCLONEDDS_URI='<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo" multicast="true"/><NetworkInterface autodetermine="true"/></Interfaces></General><Discovery><ParticipantIndex>auto</ParticipantIndex><Peers><Peer address="127.0.0.1"/></Peers></Discovery><SharedMemory><Enable>false</Enable></SharedMemory></Domain></CycloneDDS>'
