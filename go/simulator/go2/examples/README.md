# Go2 examples

These examples use the same native ROS interfaces on a physical Go2 and the
managed Go2 simulator. There is no simulator-specific drive backend.

| App | Inputs | Commands |
| --- | --- | --- |
| Patrol | `/utlidar/cloud_base`, `/utlidar/robot_odom` | `/api/sport/request` |
| Roam | `/utlidar/cloud_base`, `/utlidar/robot_odom` | `/api/sport/request` |
| Teleop | Browser hold-to-drive controls | `/api/sport/request` |
| Sensors | Native cloud, odometry and IMU; optional standard camera/joints | Read only |

From an example directory, run `wendy run --device Woof --build-type docker` for
hardware or `wendy run --device vm:<name> --build-type docker` for a simulator.
Use a CLI built from this checkout. The manifest requests host discovery for
hardware; managed VM deployment converts it to isolated guest loopback discovery
without changing the source manifest. Raw Docker defaults to subnet discovery.
For an isolated simulator container, override both discovery variables or use
its explicit loopback CycloneDDS configuration.

The driving examples build their pinned Unitree Request types into their own
images. Shared Python I/O and message inputs are copied into each directory so
it remains a complete standalone Docker/Wendy build context. After changing
`common/go2_io.py`, run `../tools/sync_examples.py` from this directory. Use
`--check` to verify those copies. Message definitions come from the simulator's
pinned `unitree_ros2` inputs, not a moving upstream branch.

## Sensor contract

Autonomous examples project `/utlidar/cloud_base`, frame `base_link`, into
72 sectors of 5 degrees. Each sector retains the closest return in the body-height
band from -0.2 to +0.4 metres relative to the body origin. For motion, the cloud
is first leveled using roll/pitch from valid odometry within 100 ms of the cloud
capture. XY remains relative to the robot heading. The floor below that
band is excluded. Missing sectors remain unknown. The converter accepts padded,
organized clouds, little/big endian data, extra fields and FLOAT32/FLOAT64 XYZ.
It validates dimensions and field offsets before reading any points.
This obstacle slice does not establish clearance at every height or model drop-offs.
The simulator uses 25 elevation rings to support the horizontal obstacle slice
while walking. The virtual lidar pattern and mounting calibration remain synthetic.
Occluded sectors can stop a patrol even if the nearest visible obstacle is distant.

Odometry uses `/utlidar/robot_odom`, from `odom` to `base_link`. Sensor captures
must advance and be younger than 350 ms for motion. The default assumes clocks
are synchronized. If the sensor processor uses a different wall clock, set
`GO2_SENSOR_CLOCK_OFFSET_SECONDS` to its independently measured offset, defined
as sensor clock minus application clock. Apply it through `wendy run --env`.
The app never estimates this offset from packet receipt, which could conceal
delayed data. Invalid, old or replayed observations continue to block motion.
Woof's inspected sensor headers were offset from its application clock; topic
compatibility alone does not correct that clock setup.

All driving examples send Move, API 1008, with `x`, `y`, `z` velocities and an
initial zero request. The managed simulator grants a new publisher control only
for a valid initial bounded Move, and discards that pre-grant request. Old publishers
cannot take control back. Pause/reset still require a new publisher. Physical
Go2 control arbitration remains the robot firmware's responsibility.

The examples do not change posture or switch the robot's motion mode.
Patrol and Roam stop on observation faults and need a new Start after a route
has begun. Teleop expires browser heartbeats after 250 ms.
