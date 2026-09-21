#!/bin/bash
set -eo pipefail
unset AMENT_PREFIX_PATH CMAKE_PREFIX_PATH COLCON_PREFIX_PATH PYTHONPATH LD_LIBRARY_PATH
source /opt/ros/humble/setup.bash
source /app/stock_ros/install/setup.bash
export PYTHONPATH=/opt/wendy-g1/assets/sdk2_python${PYTHONPATH:+:$PYTHONPATH}
exec "$@"
