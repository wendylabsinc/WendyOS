#!/bin/bash
set -eo pipefail
source /opt/ros/humble/setup.bash
source /opt/wendy-go2/ros_ws/install/setup.bash
# Use the same explicit loopback Cyclone interface as the virtual robot.
# Humble's localhost flag must be off to avoid injecting the interface twice.
export ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp
export CYCLONEDDS_URI=file:///app/cyclonedds.xml
exec "$@"
