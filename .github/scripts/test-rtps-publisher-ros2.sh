#!/usr/bin/env bash
# Publishes with the agent's ROS 2 client and requires real ROS 2 Humble nodes,
# on both CycloneDDS and Fast DDS, to receive and decode the samples. Two tests
# run the whole SensorLink path: a simulated board with only a temperature
# channel, the agent's supervisor and bridge, and a ROS 2 node subscribed to it.
#
# Needs Docker and Go. The ROS 2 image is the pinned base the other ROS runtime
# checks use, plus the CycloneDDS middleware. The tests run in three
# containers: one with no network beyond loopback, one with a network whose
# ROS 2 nodes discover beyond loopback, and one that may create and enter
# network namespaces, for an app that has a network of its own.
#
#   test-rtps-publisher-ros2.sh                    run everything
#   test-rtps-publisher-ros2.sh PATTERN [PASS]     run matching tests in one pass
#                                                  (loopback-only by default)
#
# WENDY_ROS2_INTEROP_VERBOSE=1 prints the publisher's discovery log.
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
(cd "$root/go" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go test -c -o "$work/ros2sensor.test" ./internal/agent/ros2sensor)
cp "$root/go/internal/rtps/testdata/ros2_interop_subscriber.py" "$work/"

docker build --quiet --tag "$image" - <<EOF
FROM $base
RUN apt-get update && apt-get install -y --no-install-recommends ros-humble-rmw-cyclonedds-cpp && rm -rf /var/lib/apt/lists/*
EOF

# run_pass NAME PATTERN REQUIRED DOCKER_ARGS... runs the tests matching PATTERN
# in one container. Each test named in REQUIRED must have passed.
run_pass() {
  local name=$1 pattern=$2 required=$3 log="$work/$1.log" status=0 test
  shift 3
  echo "=== $name ==="
  docker run --rm --cap-drop=ALL --security-opt=no-new-privileges --read-only \
    --tmpfs /tmp:rw,nosuid,nodev,size=64m -e HOME=/tmp -e ROS_LOG_DIR=/tmp/ros-logs \
    -e WENDY_ROS2_INTEROP_SUBSCRIBER=/work/ros2_interop_subscriber.py \
    -e WENDY_ROS2_INTEROP_VERBOSE -e PATTERN="$pattern" "$@" \
    -v "$work:/work:ro" "$image" /bin/bash -c \
    'source /opt/ros/humble/setup.bash
     for binary in /work/rtps.test /work/ros2sensor.test; do
       "$binary" -test.run "$PATTERN" -test.v -test.count=1 -test.timeout 900s || exit 1
     done' \
    2>&1 | tee "$log" || status=$?

  # A skipped test is not a passing one: it means the container was not set
  # up for it.
  if [[ $status -ne 0 ]] || grep -q -- '--- SKIP' "$log" || ! grep -q '^--- PASS: Test' "$log"; then
    echo "$name: ROS 2 nodes did not receive the published samples" >&2
    exit 1
  fi
  for test in $required; do
    if ! grep -q "^--- PASS: $test " "$log"; then
      echo "$name: $test did not run" >&2
      exit 1
    fi
  done
}

# Loopback only: what an app with ROS 2 scope "app" or "device" has.
loopback_only() {
  run_pass loopback-only "$1" "$2" --network none
}

# Wider discovery: what an app with scope "peripherals" or "network" has. The
# agent publishes on loopback either way, so these nodes must still find it.
# The CycloneDDS configuration is the one cycloneDDSBeyondDeviceConfig in
# go/internal/agent/containerd/client.go gives such an app; Fast DDS is only
# told not to stay on loopback.
wider_discovery() {
  run_pass wider-discovery "$1" "$2" -e WENDY_ROS2_INTEROP_LOCALHOST_ONLY=0 \
    -e CYCLONEDDS_URI='<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo" multicast="true"/><NetworkInterface autodetermine="true"/></Interfaces></General><Discovery><ParticipantIndex>auto</ParticipantIndex><Peers><Peer address="127.0.0.1"/></Peers></Discovery><SharedMemory><Enable>false</Enable></SharedMemory></Domain></CycloneDDS>'
}

# An app in a network namespace of its own, which is what an app without the
# host network entitlement has. The ROS 2 node runs in a new namespace and the
# agent's side has to enter it, which takes these two capabilities.
isolated_app() {
  run_pass isolated-app "$1" "$2" --network none --cap-add SYS_ADMIN --cap-add NET_ADMIN \
    -e WENDY_ROS2_INTEROP_NETNS=1
}

if [[ $# -gt 0 ]]; then
  case "${2:-loopback-only}" in
    loopback-only) loopback_only "$1" '' ;;
    wider-discovery) wider_discovery "$1" '' ;;
    isolated-app) isolated_app "$1" '' ;;
    *)
      echo "unknown pass $2: use loopback-only, wider-discovery or isolated-app" >&2
      exit 2
      ;;
  esac
  exit 0
fi

shared='TestROS2NodesReceiveTemperatureSamples TestROS2NodesReceiveRawByteSamples TestROS2NodeReceivesAPairedTemperatureSensor'
loopback_only '^TestROS2Node' "$shared"
wider_discovery '^TestROS2Node' "$shared"
isolated_app '^TestIsolatedROS2App' 'TestIsolatedROS2AppReceivesAPairedTemperatureSensor'
