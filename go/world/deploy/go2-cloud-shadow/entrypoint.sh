#!/bin/bash
set -eo pipefail
source /opt/ros/humble/setup.bash
exec python3 -B /opt/world/cloud-shadow-launch.py idle
