#!/bin/bash
set -eo pipefail
source /opt/ros/humble/setup.bash
cd /opt/world
exec python3 -B /opt/world/cloud-shadow-launch.py capture "$@"
