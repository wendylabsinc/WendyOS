# Go2 waypoint patrol

Patrol follows a finite route using the native Go2 interfaces shared by physical
robots and the simulator. The default route is a 1 m square, relative to the
position and heading at startup. It stops at completion or on an observation,
obstacle or timeout fault. It has no map or obstacle recovery.

## Run

From this directory:

```sh
wendy run --device Woof --build-type docker --no-restart
# Or, with a CLI built from this checkout:
wendy run --device vm:<simulator-name> --build-type docker --no-restart
```

The Docker command starts one route automatically after current observations
show clear space. It sends an initial zero Unitree Move request. Managed Go2
simulators grant that new publisher control automatically. Standalone simulators
using manual control require a grant in the sandbox.

See the [shared interface and clock requirements](../README.md). In particular,
Woof publishes body-frame point clouds and native odometry, not `/scan` or `/odom`.
The application requires synchronized capture clocks or an independently measured
`GO2_SENSOR_CLOCK_OFFSET_SECONDS`. Waiting logs identify the readiness blocker
and the underlying sensor rejection. Repeated errors are suppressed; changing
waiting conditions print at most once every five seconds. Old or future captures
include their age relative to the application clock. `/patrol/status` includes
`sensor_errors` and `capture_age_seconds` for the latest checked captures.
Packet receipt alone does not prove that a capture is fresh.

## Stop, inspect or restart a route

```sh
wendy --device Woof cloud device ros2 call /patrol/stop std_srvs/srv/Trigger '{}'
wendy --device Woof cloud device ros2 echo /patrol/status
wendy --device Woof cloud device ros2 call /patrol/start std_srvs/srv/Trigger '{}'
```

For a simulator or directly reachable device, use `device ros2` instead of
`cloud device ros2` and select its device name. An explicit Start or Stop cancels
pending autostart. Start while running preserves the route. Completion or a fault
never automatically restarts it. After a simulator pause/reset, resume the world
and restart the app to create a new command publisher.

## Route and limits

In the image's ROS shell:

```sh
python3 /app/ros_app.py --autostart --waypoints '[[1.5,0],[1.5,1],[0,1],[0,0]]' --laps 2
```

Omit `--autostart` to wait for `/patrol/start`. Use one Patrol instance at a time.
Routes allow 1 to 16 waypoints within 3 m of the start, 1 to 5 laps, and at most
30 m of total travel. Include `[0,0]` to return to the start. Odometry and targets
must remain within ±5 m on both odom axes. Each waypoint has a 45-second deadline;
the route has a five-minute deadline.

Commands are bounded to 0.35 m/s forward and ±0.4 rad/s yaw. Patrol turns in place
before walking and accepts arrival within 25 cm. It requires all 72 projected
cloud sectors, with more than 85 cm forward clearance and 55 cm elsewhere.
Unknown sectors, including missing returns, prevent startup and stop a route.
`/patrol/status` reports controller state, sensor ages, clearances, target and
`readiness_error`. Command requests are not confirmation of physical motion.

## Checks

```sh
../../.venv/bin/python -m pytest -q
```

Tests cover route completion, bounds, fault latching, admission, autostart and
native request serialization. The simulator integration check exercises actual
ROS delivery and native control. It does not validate physical gait or stopping
distance on Woof.
