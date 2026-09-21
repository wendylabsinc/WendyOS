#!/bin/bash
set -eo pipefail
source /opt/ros/humble/setup.bash
source /opt/wendy-go2/ros_ws/install/setup.bash
export ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp
export CYCLONEDDS_URI=file:///opt/wendy-go2/cyclonedds.xml
export GO2_COMMAND_SOCKET=/tmp/hallway-commands.sock GO2_AUTO_APP_CONTROL=1 GO2_VISUAL_DETAIL=full
ros2 run go2_command_ingress go2_command_ingress &
ingress_pid=$!
python3 /hallway/preview.py --sim-root /opt/wendy-go2 --ros --port 8895 --layout "${HALLWAY_LAYOUT:-corner}" --width "${HALLWAY_WIDTH:-1.4}" &
runtime_pid=$!
cleanup() {
  kill -TERM "$runtime_pid" "$ingress_pid" 2>/dev/null || true
  wait "$runtime_pid" "$ingress_pid" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 0' TERM INT
wait -n "$runtime_pid" "$ingress_pid"
exit 1
