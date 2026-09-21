#!/bin/bash
set -eo pipefail
# The managed profile has guest host-admin networking solely for this bootstrap.
# Keep rules across app restarts; reapply after each VM boot before DDS starts.
if [ -n "${GO2_VM_NAME:-}" ] && [ "${1:-}" != "--isolated" ]; then
  python3 -m go2_sim.isolation
  export GO2_ISOLATION_STATUS=udp-rtps-loopback
  exec setpriv --bounding-set=-net_admin --inh-caps=-net_admin --ambient-caps=-net_admin \
    /bin/bash /opt/wendy-go2/entrypoint.sh --isolated
fi
source /opt/ros/humble/setup.bash
source /opt/wendy-go2/ros_ws/install/setup.bash
# Humble's localhost-only RMW option injects another loopback interface and
# conflicts with the explicit interface below. The Cyclone file binds to lo.
export ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp
export CYCLONEDDS_URI=file:///opt/wendy-go2/cyclonedds.xml GO2_ROS=1
mkdir -p /run/wendy-go2
session_dir=$(mktemp -d /run/wendy-go2/session.XXXXXX)
export GO2_COMMAND_SOCKET="$session_dir/commands.sock"

ros2 run go2_command_ingress go2_command_ingress &
ingress_pid=$!
python3 -m go2_sim.server &
runtime_pid=$!
cleanup() {
  kill -TERM "$runtime_pid" "$ingress_pid" 2>/dev/null || true
  wait "$runtime_pid" "$ingress_pid" 2>/dev/null || true
  # This launch owns this exact directory; a crashed previous launch cannot
  # prevent startup and no active endpoint is removed.
  rmdir "$session_dir" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 0' TERM INT
# Either worker disappearing makes the managed runtime unhealthy and exits the
# container. There is no surviving robot with a silently dead command bridge.
wait -n "$runtime_pid" "$ingress_pid"
exit 1
