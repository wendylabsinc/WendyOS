# Go2 sample apps

Small, standalone applications for the [Wendy Go2 simulator](../README.md).
Each directory is its own deployable project with a Dockerfile and `wendy.json`.
They use standard ROS 2 Humble messages and the simulator VM's loopback bus.

| App | Try it for | Interface |
| --- | --- | --- |
| [Roam](roam/README.md) | Autonomous exploration with reactive obstacle avoidance | Local browser controls or ROS start/stop services |
| [Patrol](patrol/README.md) | A finite square or configurable route, using odometry and lidar | ROS start/stop services and status topic |
| [Teleop](teleop/README.md) | Manual driving with hold-to-drive keys or touch buttons | Browser at port **8903** |
| [Sensors](sensors/README.md) | Camera, lidar, odometry trail, IMU, joints and capture freshness | Read-only browser at port **8904** |

## Run a sample

Create and start a Go2 simulator using a CLI built from this checkout:

```sh
wendy vm create go2-sim --profile go2
wendy vm robot start go2-sim
wendy vm robot open go2-sim
```

From the sample's directory, deploy it:

```sh
wendy run --device vm:go2-sim --build-type docker --no-restart
```

For example, run that command from `teleop/`. Wendy opens
`http://127.0.0.1:8903` when the app is ready. Wendy forwards the declared HTTP port for a VM using
the default user networking. Shared-network VMs use their guest IP instead.
The sensor dashboard runs on a separate port so you can deploy it alongside any
driving sample. See each app's README for controls and configuration.

Driving samples publish zero on startup so the sandbox can discover their ROS
publisher. The managed Go2 simulator automatically gives the new publisher
control, replacing the previous driving app or browser controller. Running
Patrol walks its route once observations are ready; Roam starts exploring.
Teleop waits for **Enable controls** in its page.
Patrol prints its progress and stop reasons in the `wendy run` logs.
Only one publisher can drive at a time. Older publishers cannot automatically
take control back; restart an earlier driving app to return control to it.
The sandbox still offers **Give app control** when the robot has no owner.
The sensor dashboard needs no grant and does not take control.

After a simulator pause, world reset or **Release app control**, resume the
world and then restart the driving app. A resumed world does not restore old
grants. Starting an app while paused does not defer its automatic grant until
resume. See [command ownership](../README.md#sandbox-and-command-lifetime).
Sensor recovery alone does not restart a stopped patrol or roam controller.

These examples target the virtual robot. Patrol follows direct waypoint legs;
teleop is manual driving. Read their documented limits before adapting them.
Only [Roam's local runner](roam/README.md#run-beside-the-local-browser-preview)
supports the standalone HTTP simulator preview; the deployed apps require ROS.

## Development checks

With the simulator's [development environment](../README.md#compatibility-and-validation)
installed, run all sample tests from this directory:

```sh
../.venv/bin/python -m pytest -q roam patrol teleop sensors
```

The tests cover controller behavior, source timestamp admission, HTTP endpoints,
camera encoding and command lifetime without importing ROS. The Dockerfiles
provide the ROS dependencies for deployment. Browser pages use no build step
or external assets.

Validated on 2026-09-15: all 196 sample tests passed and all three new Docker
images built. In a disposable managed Go2 Docker runtime, patrol completed its
default square with automatic startup in 25.43 seconds, returning within
24.0 cm; the sensor dashboard
received all five ROS streams and passed camera dropout/recovery checks.
Teleop passed an isolated ROS command test for startup zero, 20 Hz publishing,
driving, release and heartbeat expiry. Both browser pages passed desktop/mobile
checks, including disconnected states, with no JavaScript errors.

The updated runtime and Patrol were also deployed to `go2-sim`. The live page
displayed named publishers and preserved selection through status polling.
After an initial stale-scan stop while awaiting control, an explicit Start
completed the square in about 30 seconds and held zero commands. Physical
robot operation has not been tested.

Automatic control handoff was then verified on `go2-sim`: launching Patrol
with `wendy run --detach` took control from a still-publishing Roam app.
Roam did not reclaim control. After resetting the world and launching Patrol
again, it completed all four waypoints in 28.6 seconds and held zero commands,
without a manual grant or a `/patrol/start` call.
