# Go2 waypoint patrol

Run a finite route through an open part of the Go2 simulator using lidar and
odometry. The default route is a 1 m square: forward, left, back, then back to
the starting position. The deployed app starts one route automatically when
fresh lidar and odometry show clear space. It captures the current position
and heading, then transforms the route's `[forward, left]` offsets into `odom`.
Completion stops the app's velocity requests.

The app follows waypoints directly and stops for obstacles; it has no map,
path planning or obstacle recovery. Choose an open starting area. This sample
is intended for the virtual Go2, and has not been validated on a physical robot.

## Deploy to the simulator VM

From this directory:

```sh
wendy run --device vm:<simulator-name> --build-type docker --no-restart
```

The manifest uses ROS 2 Humble, CycloneDDS, domain 0 and the simulator VM's
loopback ROS bus. The Dockerfile runs `ros_app.py --autostart`. The app first
publishes zero velocity, waits for fresh sensors and clear space, then begins
requesting the route. You do not need to call `/patrol/start` for this first run.

The managed Go2 simulator automatically gives the new Patrol publisher
control, replacing the previous driving app or browser controller. Run the
command above with the world running to walk the route. Open the sandbox to
watch it; no **Give app control** click is needed.

If another app takes control during the route, Patrol's waypoint deadline
continues to run. Restart Patrol to return control to it and start a new route
from the current pose.
The terminal logs report waiting, waypoint progress and the reason for a stop.

## Stop, inspect or run another route

Use these commands from your development machine:

```sh
wendy --device vm:<simulator-name> device ros2 call /patrol/stop std_srvs/srv/Trigger '{}'
wendy --device vm:<simulator-name> device ros2 echo /patrol/status
wendy --device vm:<simulator-name> device ros2 call /patrol/start std_srvs/srv/Trigger '{}'
```

The equivalent commands in a ROS-enabled shell on the same VM bus are:

```sh
ros2 service call /patrol/start std_srvs/srv/Trigger '{}'
ros2 topic echo /patrol/status std_msgs/msg/String
ros2 service call /patrol/stop std_srvs/srv/Trigger '{}'
```

Start needs current, valid `/scan` and `/odom` observations and clear space
around the robot. If Start fails, its response contains the stop reason. Repeating
Start while a patrol is running leaves the current route in progress. Starting
after Stop, completion or a fault creates a new route from the current pose.
`/patrol/stop` immediately publishes zero. **Release app control** in the
sandbox also removes this publisher's command ownership.

Automatic startup happens only once per process. After the route starts, Stop,
completion or a fault leaves it stopped until an explicit Start. Calling Stop
before sensors are ready also cancels the pending automatic start. An explicit
Start cancels the pending automatic start even if that request fails.

## Configure a route

In a ROS-enabled shell sharing the simulator bus, run the adapter directly:

```sh
python3 ros_app.py --autostart --waypoints '[[1.5,0],[1.5,1],[0,1],[0,0]]' --laps 2
```

Omit `--autostart` to keep the app stopped until `/patrol/start` succeeds.
For a deployed container, put the same arguments after `/app/ros_app.py` in the
Dockerfile's `CMD` before running `wendy run`. Use one patrol instance at a
time, since the examples use fixed service and topic names.

Routes allow 1 to 16 finite waypoints, each within 3 m of the start, 1 to 5 laps,
and at most 30 m of total travel. A lap visits the configured waypoints in
order; include `[0,0]` as the last waypoint to return to the start. Transformed
targets and live odometry must stay within ±5 m on both `odom` axes. A waypoint
times out after 45 seconds; the whole route times out after 5 minutes. These
deadlines include any time without a simulator control grant.

## Behavior and status

- Subscribes to `/scan` (`sensor_msgs/msg/LaserScan`, frame `lidar_link`) and
  `/odom` (`nav_msgs/msg/Odometry`, from `odom` to `base_link`).
- Publishes `/cmd_vel` (`geometry_msgs/msg/Twist`) at 20 Hz, bounded to
  0.35 m/s forward and ±0.4 rad/s yaw, with no reverse or lateral motion.
  It turns toward each waypoint before walking and arrives within 25 cm.
- Requires a complete horizontal lidar circle with finite in-range returns.
  Missing returns, including infinity, are unknown space and stop the route.
  It stops at 85 cm forward clearance or 55 cm clearance elsewhere, including
  while turning. This conservative rule can reject scans near room corners.
- Requires advancing wall-clock capture stamps younger than 350 ms. It
  rejects malformed frames, poses, timestamps and scan geometry. Delayed
  observations retain their capture age; receipt does not make them new.
- Once a route starts, stale observations, invalid data, obstacles, bounds or
  deadline violations stop and disarm it. Fresh data or a cleared obstacle
  never restarts it; issue `/patrol/start` when ready to start a new route.

`/patrol/status` publishes JSON with the current state and reason, waypoint
number/count, target in `odom`, distance, requested velocities, clearances and
sensor ages in seconds. `autostart_pending` is true while automatic startup is
waiting for observations and clear space. The status describes the app's
requests; it does not report ownership of the simulator's command grant.

After a simulator pause, world reset or **Release app control**, resume the
world and then restart the ROS app to get a new automatic grant. Starting the
app while paused does not defer its grant until resume. Existing publishers
cannot automatically take control back from a newer app.

## ROS-free checks

From this directory:

```sh
../../.venv/bin/python -m pytest -q test_patrol.py test_admission.py
```

The checks exercise a complete square through a simple kinematic model,
relative route transforms, bounded commands, fault latching, deadlines and
ROS message admission. They also cover automatic startup, cancellation before
startup, and holding zero after Stop, faults and completion. These unit tests
do not validate the MuJoCo gait or DDS deployment.
The container uses the ROS packages installed in the Dockerfile; there are
no pip dependencies.

A separate live smoke test on 2026-09-15 built this image and ran its default
Docker command beside an isolated `wendy-go2-managed:dev` runtime with real ROS
and MuJoCo. The route started automatically without a `/patrol/start` call.
The simulator held its applied command at zero before the control grant.
After the grant, the default square completed in 25.43 seconds with 24.0 cm
return error. All 360 lidar rays were admitted, and requests stayed within
0.35 m/s and ±0.4 rad/s. The app held zero for two seconds after completion
while fresh observations continued. The test revoked the grant and removed
both containers afterward. It did not exercise a simulator VM deployment or
physical hardware.
