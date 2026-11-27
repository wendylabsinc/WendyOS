#!/bin/bash
set -eo pipefail
source /opt/ros/humble/setup.bash
set -u
export ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0
export CYCLONEDDS_URI=file:///opt/world/cyclonedds.xml
python3 -m sim.go2_bridge probe --runtime http://127.0.0.1:8890 --output /tmp/zero-probe.json --delay-ms 250 || true
exec python3 /opt/world/probe-report.py
