#!/bin/bash
set -eo pipefail
if [ -n "${R2_VM_NAME:-}" ] && [ "${1:-}" != "--isolated" ]; then
  python3 -m r2_sim.isolation
  export R2_ISOLATION_STATUS=udp-rtps-loopback
  exec setpriv --bounding-set=-net_admin --inh-caps=-net_admin --ambient-caps=-net_admin \
    /bin/bash /opt/wendy-r2/entrypoint.sh --isolated
fi
source /opt/ros/humble/setup.bash
export ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp
export CYCLONEDDS_URI=file:///opt/wendy-r2/cyclonedds.xml R2_ROS=1
exec python3 -m r2_sim.server
