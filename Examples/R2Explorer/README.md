# R2 Explorer

A small autonomous exploration app for the Wendy ROSMASTER R2 simulator. It
builds an observed map, chooses destinations it has not visited, and scores
short forward and reverse Ackermann arcs against lidar returns. A depth check
adds a forward clearance constraint. It reads sensors and odometry, never the
simulator's obstacle list.

The sample uses Python's standard library and runs separately from henk's
manual controls. Start explicitly transfers simulator app ownership. Stop,
pause, reset, a different controller, stale sensors, or the run timer halt it.
It starts parked and does not resume automatically after a fault or restart.

## Run on the simulator

With a current source-built Wendy CLI and the ROSMASTER R2 VM named `sim`:

```sh
wendy run --prefix Examples/R2Explorer --device vm:sim --detach
```

Open **http://127.0.0.1:3510** and press **Start exploring**. Use **Stop** or
Space to stop early. The default run lasts two minutes; closing the dashboard
does not stop a run. The simulator viewer also provides Stop and manual takeover.

When running from this sample's own directory, omit `--prefix Examples/R2Explorer`.
The sample requires the updated R2 runtime with the app ownership API, lidar,
raw depth and camera calibration endpoints. It refuses a non-simulator target.

For local development against a running simulator's forwarded HTTP port:

```sh
R2_SIMULATOR_URL=http://127.0.0.1:50459 python3 Examples/R2Explorer/app.py
```

`PORT` defaults to `3510`. Inside the VM, `R2_SIMULATOR_URL` defaults to
`http://127.0.0.1:8890`. The run API is `POST /api/start` with
`{"duration": 120}` and `POST /api/stop`; `GET /api/status` includes the map.

## Extend the planner

`planner.py` contains goal selection, visit memory and the local arc rollout.
`app.py` handles sensor freshness, exclusive control, time limits and the web API.
The car uses approximate R2 dimensions, a 0.30 m clearance radius, up to 0.38 m/s
forward speed, and 0.18 m/s reverse speed. The simulator's 300 ms command watchdog
also stops motion if this process disappears.

This is a reactive exploration example, not SLAM or a complete coverage planner.
It uses ideal simulator odometry, may revisit areas, and can stop in confined
spaces where it cannot find a safe arc. A hardware port needs calibrated sensors,
localization and a hardware motion backend; this app has no hardware fallback.

```sh
cd Examples/R2Explorer
python3 -m unittest discover -s tests -v
python3 tests/simulator_acceptance.py
```

The acceptance check uses the repository's real R2 model as a collision oracle
and advances it offline. It covers a two-minute run and reversing away from a
nearby wall without connecting to your VM.
