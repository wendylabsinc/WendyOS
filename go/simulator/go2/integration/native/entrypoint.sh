#!/bin/bash
set -eo pipefail
source /opt/ros/humble/setup.bash
source /opt/wendy-go2/ros_ws/install/setup.bash
exec "$@"
