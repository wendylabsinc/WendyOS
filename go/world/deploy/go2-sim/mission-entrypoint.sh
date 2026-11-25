#!/bin/bash
# ROS setup accesses optional variables, so nounset starts afterward.
set -eo pipefail
source /opt/ros/humble/setup.bash
set -u
# Wendy framework variables must not replace this reviewed simulation topology.
export CYCLONEDDS_URI=file:///opt/world/cyclonedds.xml
export ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0
export ROS_LOG_DIR=/tmp/ros-log
export PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
cd /opt/world
exec python3 /opt/world/boot.py
