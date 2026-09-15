# Go2 browser teleoperation

This app uses the [shared native Go2 interfaces](../README.md) on hardware and
in the simulator. The source manifest uses host discovery; managed VM deployment
normalizes it to guest loopback. Use a CLI built from this checkout.


Drive a Go2 robot or its simulator from a small browser control panel. Hold **W / S** to move
forward or backward, **A / D** to strafe left or right, and **Q / E** to turn.
The six buttons also support mouse and touch. Release a direction to stop that
motion; **Space**, **Escape**, or **Stop / disable** ends the control session.

The Python application uses ROS 2 and the standard-library HTTP server, with no
pip or frontend dependencies. It publishes `unitree_api/msg/Request` on
`/api/sport/request` at 20 Hz, starting with zero velocity while the simulator gives its
new publisher control. This is manual driving; it has no obstacle avoidance.

## Deploy to a Go2 VM

From this directory, with a Wendy Go2 VM available:

```sh
wendy run --device vm:<simulator-name> --build-type docker --no-restart
```

1. Wendy opens the control panel after the app is ready. You can also open
   `http://127.0.0.1:8903`. The manifest lets Wendy forward port 8903 for a VM using the default
   user network. The server binds `0.0.0.0` inside the guest so that forwarding
   can reach it. With a shared-network VM, use `http://<guest-ip>:8903` instead.
2. Find the sandbox URL with `wendy vm robot status <simulator-name>` and open it
   alongside the control panel.
3. Choose **Enable controls** in the control panel. Hold a direction
   to drive. Keep the control panel focused; use two visible windows if you want
   to watch the sandbox while driving.

The managed Go2 simulator automatically gives the new Teleop publisher control,
replacing the previous driving app or browser controller. Its initial command
is zero; it waits for you to enable controls and hold a direction before moving.
The manifest selects ROS 2 Humble, CycloneDDS and domain 0. Managed VM
deployment normalizes its hardware discovery setting to the loopback bus.

After a simulator pause, world reset or **Release app control**, resume the
world and then restart this application. Its new publisher receives control
automatically. Starting it while paused does not defer its grant until resume.
**Release app control** in the sandbox removes the simulator grant independently
of this page's enable button. Existing publishers cannot automatically take
control back from a newer app; restart Teleop to return control to it.

## Run in an existing ROS environment

In a ROS 2 Humble shell on the same bus as the simulator:

```sh
python3 teleop.py --host 127.0.0.1 --port 8903
```

The host needs `rclpy`, `geometry_msgs` and the selected ROS middleware; the
Dockerfile installs them for deployment. This sample connects to ROS, so the
standalone simulator browser preview's HTTP API is not a transport for it.

For deployment on a different port, change `--port` in the Dockerfile's
command, `readiness.tcpSocket.port`, and the HTTP entitlement's `port` in
`wendy.json`. This development control
panel has no login; use it on your local simulator network.

## Stop behavior

- The browser sends a heartbeat every 100 ms. The server invalidates the session
  after 250 ms without one and publishes zero on its next 50 ms timer tick.
  Expired sessions need a new explicit **Enable controls** action.
- Releasing every direction requests zero immediately. Stopping, hiding the
  page, losing window focus, reloading, or a failed control request also clears
  the tab's held inputs and releases its session. The server timeout covers
  disconnects where the browser's release cannot arrive.
- Only one tab can own a session. Ordered request numbers and unique session
  tokens reject delayed input after a release, expiry, or a new tab taking over.
  A page reload never restores its old controls.
- The server rejects malformed requests and nonfinite speeds. Translation is
  capped at 0.6 m/s overall, lateral velocity at 0.4 m/s, and yaw at 1 rad/s.
  Opposite keys cancel each other. The panel shows requested velocities;
  simulator permission and constraints determine actual movement.

## Check without ROS

From this directory, using the simulator's development environment:

```sh
../../.venv/bin/python -m pytest -q test_teleop.py
```

The checks cover deadman expiry, immediate stop, concurrent tabs, reordered
requests, a timer racing with release, malformed HTTP payloads, velocity limits,
and the ROS adapter's startup command and 20 Hz timer using a minimal test double.

With Playwright and Chromium installed, the optional browser regression runs
against a disposable local instance **without a simulator control grant**:

```sh
node teleop.browser.mjs http://127.0.0.1:8903
```

Set `PLAYWRIGHT_MODULE` to an existing Playwright module path if needed. This
checks keyboard and pointer driving, release, focus loss, reload, disconnects,
and mobile layout, and writes desktop/mobile screenshots to `/tmp`.

## Sensor compatibility

Teleop uses browser controls and does not check sensor capture clocks or lidar
coverage. The temporary clock and scan-gap options in Patrol, Roam and Sensors
do not apply here. Its 250 ms browser heartbeat timeout remains active.
