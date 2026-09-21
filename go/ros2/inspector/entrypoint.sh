#!/bin/sh
set -e
. /opt/ros/humble/setup.sh
. /opt/wendy-ros2/install/setup.sh
exec "$@"
