# Passive Go2 patrol navigation

This private simulation adapter runs ROS 2 Humble, AMCL, Smac State Lattice and
Regulated Pure Pursuit in one isolated Linux container. The native viewer does
not own this process. The HTTP bridge receives only physics `/sensors` and the
known static `/map`. It never reads `/state` or uses body ground truth for control.

The launcher can reuse an installed private dependency image:

```sh
docker build --build-arg NAV_BASE=go2-home-navigation:validation \
  -f deploy/patrol-nav/Dockerfile -t wendy-world-patrol-nav:local .
```

Without `NAV_BASE`, the Dockerfile installs Nav2 into `ros:humble-ros-base`. Run
on the launcher's private Docker network with `--physics http://physics:8080`.
The adapter listens on port 8092. ROS DDS stays inside this container through
`ROS_LOCALHOST_ONLY=1`, domain 72. The generated map image and parameters remain
in `/tmp/patrol-nav`; no generated images are Git assets.

The initial estimate defaults to `-5.8,0.15,0.08` in map X/Y/yaw coordinates.
MuJoCo and ROS use metres with Z up. World clients convert to their explicit
Y-up frame. AMCL receives this estimate exactly once, after activation, with
0.5 metre position and 0.262 radian heading standard deviations. Later updates
come from the simulated mounted LiDAR and independent contact/IMU odometry.

The nominal forward command is 0.55 m/s. The controller's minimum approach and
regulated speeds also use the verified 0.55 m/s command. The command guard converts positive requests below
0.5 m/s to a stationary response while preserving the bounded yaw command.
It never increases a request into the supported range. The planner checks a
rectangular footprint, and the controller checks projected collisions. The one-metre-radius differential lattice fits the 0.55 m/s forward and
0.7 rad/s yaw limits. A requested yaw above that limit stops forward motion
while turning. The policy retains its original internal slew limits.
The route stays fixed until the costmap invalidates it or the goal changes;
Nav2 checks validity every second. No backup, following or investigation behavior is installed.

HTTP endpoints:

- `GET /status` reports readiness, AMCL pose/covariance, planned route, measured
  odometry speed, current goal and sensor age. Goal states are `idle`, `accepted`,
  `navigating`, `succeeded`, `canceled`, `failed`, `rejected`, `executor_lost` and
  `sensor_hold`.
- `GET /candidates` uses the office's declared room geometry and current global
  costmap. Candidates require a 0.75 metre free circle for turning and arrival
  margin around the padded rectangular footprint. This is a fixture configuration,
  not a measured worst-case stopping bound. Every room remains listed, including
  rooms with no qualifying position, with the reason reported separately from
  physical room reachability.
- `POST /route` with `x`, `y`, `yaw` asks Smac for a route and returns `reachable`,
  `length` and `route`. Candidate acceptance also checks a return path, so a
  position with no navigable exit is rejected. Allow a 12-second HTTP timeout.
- `POST /goal` takes the same coordinates plus `id`. It accepts an internal
  waypoint under the existing mission; it does not authorize a new mission.
- `POST /heartbeat` renews the executor's one-second lease for the current goal.
  Motion requires both a goal and a lease. An optional `expires_at_unix_ms`
  further caps each lease at the mission expiry. An expired lease latches the goal off;
  a delayed heartbeat cannot restart it.
- `POST /cancel` cancels the active goal and sends zero velocity.

Missing, malformed or stale scans/odometry, stale localization, or position/yaw
standard deviations above 0.15 metre/radian inhibit velocity and cancel the goal.
Passive observations use a 0.15 metre arrival tolerance with no required final yaw. The physics process
also owns its independent command watchdog. These are simulation fixture
controls, not a hardware safety certification or production authentication API.

Run `python3 -m unittest sim.test_patrol_nav -v` for focused contract tests.
Runtime and localization acceptance results belong in the main patrol verification
report, with the actual image IDs and physics/policy identities.
