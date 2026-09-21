# G1 standard ROS acceptance

Run this standalone process inside a separate container or deployed application
on the same guest-local DDS bus as an already running G1 runtime. Use the built
G1 image so its pinned `unitree_hg` types are available. Source ROS and that
image's overlay before invoking the script:

```sh
. /opt/ros/humble/setup.sh
. /opt/wendy-g1/ros_ws/install/setup.sh
export ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0
export CYCLONEDDS_URI=file:///opt/wendy-g1/cyclonedds.xml
python3 /path/to/exercise.py --seconds 60 --expected-vm g1-sim --expected-source sha256:ACTUAL_SOURCE_DIGEST --output /tmp/g1-standard.json
```

Omit the optional expected identity flags for an isolated local container run.
The script always requires a loopback HTTP endpoint identifying a G1 simulation.
It resets the virtual world, grants its newly discovered DDS Twist publisher,
and uses only DDS for velocity commands. Do not run another controller alongside
this test. The simulator runtime and its command ingress must already be running;
this script neither starts a VM nor imports simulator implementation modules.

Checks cover 29 physical joints in 35 HG motor slots, fresh native ticks,
standard IMU/odometry/TF, the declared pelvis-relative lidar and camera mounts,
same-stamp camera/info and scan/cloud pairs, signed physical forward/backward/turn
motion with joint, scan and camera changes, and pause/reset rejection of the
old publisher followed by a fresh explicit grant. Cleanup revokes control.
Forward/backward checks request +/-0.3 m/s for three seconds. Turning requests
0.3 m/s forward and 0.2 rad/s yaw for six seconds, within the pinned policy's
deployment limits; physical and observed yaw must both advance by more than
0.25 rad. The Cyclone DDS configuration explicitly selects loopback; keep
`ROS_LOCALHOST_ONLY=0` consistent with the runtime's explicit interface setup.

The 12-second uninterrupted rate window requires at least 40 Hz odometry/joints/
ground truth, 160 Hz IMU, 8 Hz scan/cloud, 12 Hz image/info and 400 Hz HG LowState,
measured from this application's callbacks. The JSON records actual receipt
rates and runtime physics/policy counter deltas. These finite checks do not
establish the ten-minute performance gate, native API/CRC correctness, Nav2
support or physical G1 fidelity. Total duration defaults to at least 60 seconds
and may grow by bounded discovery/readiness waits.
