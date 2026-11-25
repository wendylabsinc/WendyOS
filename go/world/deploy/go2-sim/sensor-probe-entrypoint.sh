#!/bin/bash
set -eo pipefail
source /opt/ros/humble/setup.bash
set -u
export ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0
export CYCLONEDDS_URI=file:///opt/world/cyclonedds.xml ROS_LOG_DIR=/tmp/ros-log
python3 -m sim.go2_bridge probe-sensors --output /tmp/sensor-probe.json || true
exec python3 /opt/world/probe-report.py sensors
