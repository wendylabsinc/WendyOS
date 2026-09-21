#!/bin/bash
set -eo pipefail
# The dependency image contains the old simulator overlay. Exclude it from
# all import/library paths and source the separately built stock messages.
unset AMENT_PREFIX_PATH CMAKE_PREFIX_PATH COLCON_PREFIX_PATH PYTHONPATH LD_LIBRARY_PATH
source /opt/ros/humble/setup.bash
source /app/stock_ros/install/setup.bash
export PYTHONPATH="/opt/wendy-go2/assets/sdk2_python${PYTHONPATH:+:${PYTHONPATH}}"
# Host-networked VM apps use the managed VM bus, with explicit loopback peers.
export ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0
export CYCLONEDDS_URI=file:///app/native/cyclonedds.vm.xml
exec "$@"
