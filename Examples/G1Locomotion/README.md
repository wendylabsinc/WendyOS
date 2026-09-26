# G1 locomotion demo

A Python Unitree SDK2 app for the [managed G1 MuJoCo simulator](../../go/simulator/g1/README.md).
It walks forward, follows a left arc, follows a right arc, and stops after
17 seconds. The simulator's learned policy produces the joint motion.

The app uses the official `LocoClient.SetVelocity` API at 20 Hz with a 200 ms
command duration. Forward speed is 0.3 m/s and turning speed is 0.2 rad/s.
Forward arcs suit this simulator's policy better than turning in place.
Distances and heading changes depend on policy tracking and simulation speed.

## Run in the simulator VM

Start and open an existing G1 VM using the CLI built from this checkout:

```sh
../../go/bin/wendy vm start my-g1 --detach
../../go/bin/wendy vm robot start my-g1
../../go/bin/wendy vm robot open my-g1
```

From this directory, deploy the demo into that same VM:

```sh
../../go/bin/wendy run --device vm:my-g1 --dockerfile build.stagefile.yaml --build-type docker --no-restart
```

In the simulator browser, select the new `sport` command publisher and click
**Give app control**. The app sends only zero velocity while waiting, for up to
120 seconds. Once granted, the sequence starts automatically and runs once.
Reset the simulator first if the robot has fallen or is in damping mode.

Ctrl+C or container SIGTERM requests a stop. Any SDK failure or revoked control
ends the sequence and attempts a final zero-velocity command. The simulator's
200 ms watchdog also expires stale motion commands. Reset or pause revokes
control; restart the app and grant its new publisher to run again.

This app targets Wendy's managed G1 profile, which implements the SDK locomotion
service. A stock `unitree_mujoco` low-level DDS bridge needs a locomotion policy
and high-level service before this app can control it. The app uses DDS domain
zero and Linux loopback `lo`; run it inside the simulator VM, not on the Mac host.

## Preview and test locally

These commands need only Python 3.10 or later:

```sh
python3 app.py --dry-run
python3 -m unittest -v
```

To run with Docker on the same Linux host as a managed simulator:

```sh
docker build -t g1-locomotion:dev .
docker run --rm --network host g1-locomotion:dev
```

Both build files use the same SDK revision pinned by the simulator and check
the source archive's SHA-256. The Stagefile builds CycloneDDS 0.10.2 from a
pinned commit and copies the unmodified Python SDK into its import path.
The Dockerfile uses ROS Humble's CycloneDDS library. To select that build,
replace `--dockerfile build.stagefile.yaml` with `--dockerfile Dockerfile`.
See Unitree's
[Python SDK](https://github.com/unitreerobotics/unitree_sdk2_python/tree/65691c8a8bc53b98d3976dba4dbf9d5d20b2e7f5)
and the simulator's [compatibility contract](../../go/simulator/g1/compatibility.json).

Edit `STEPS` in `app.py` to change the movement sequence. Durations use wall time.
The local tests check command sequencing, control denial, timeout, interruption,
and transport failure; they do not validate physical locomotion in MuJoCo.
