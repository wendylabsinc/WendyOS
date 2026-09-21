# Go2 hallway explorer

A separate office-hallway demo using the existing Wendy Go2 model, pinned ONNX
walking policy, and MuJoCo sensors. The same Python navigation controller runs
in the physics tests and the ROS image deployed to a physical Go2.

It follows the corridor centre, turns into an observed side branch when forward
travel is blocked, and stops at a dead end. At a choice of branches it prefers
the less-visited odometry cell, then the configured left/right preference.
Exploration ends after 20 m or three minutes by default. A stop or fault requires
an explicit new Start. It does not automatically return to the starting point.

## Test in MuJoCo

From this directory, using the existing simulator development environment:

```sh
../../.venv/bin/python -m pytest -q -p no:cacheprovider
../../.venv/bin/python simulate.py --layout corner --output /tmp/hallway-corner.json
../../.venv/bin/python preview.py --layout corner --width 1.4
```

Recorded test trajectories are in [validation/paths.svg](validation/paths.svg),
with source hashes and the ROS result in [validation/summary.json](validation/summary.json).
To refresh them, run tests with `HALLWAY_RESULTS=validation`, then `python3 report.py`.

The preview uses the existing browser sandbox at `http://127.0.0.1:8895`.
Its controls operate only that disposable simulation. It does not contact a robot.
Layouts are `straight`, `corner`, `junction`, and `blocked`. `--width` sets the
clear distance between wall faces. A 1 m straight corridor is included in the
tests; turning requires more observed space and can stop in a narrow corridor.

The headless runner advances the floating-base robot through actuator torques
and `mj_step`, including the existing walking policy. It does not move the body
by changing its pose. A biased, integrated odometry estimate and real raycast
clouds feed the production admission code. Commands pass through native Move
serialization. Ground truth is used only to check outcomes and wall contacts.
The local test does not exercise DDS; the following test does.

### Test the ROS deployment image

Build the existing simulator image as described in [the simulator guide](../../README.md),
then run these commands from this directory:

```sh
docker build -t wendy-go2-hallway:demo .
docker run -d --rm --name hallway-test --network none \
  -e GO2_NATIVE=1 -e GO2_VISUAL_DETAIL=full \
  -v "$PWD:/hallway:ro" \
  wendy-go2-managed:dev /bin/bash /hallway/sim_entrypoint.sh
mkdir -p validation
docker run --rm --network container:hallway-test \
  -e RMW_IMPLEMENTATION=rmw_cyclonedds_cpp \
  -e CYCLONEDDS_URI=file:///test-cyclonedds.xml \
  -v "$PWD/../../cyclonedds.xml:/test-cyclonedds.xml:ro" \
  -v "$PWD/validation:/results" \
  wendy-go2-hallway:demo python3 /app/ros_smoke.py --output /results/ros-corner.json
docker stop hallway-test
```

The simulator and controller share a network namespace with no external network.
The test verifies the simulator identity before creating a command publisher.
It requires a completed corner, nonzero native command admission, zero final
commands, and no contacts with hallway walls. The test app includes no simulator
runtime. The robot model and walking policy stay in the simulator container.

## Deploy to Woof

The image starts idle. The initial and idle requests are zero velocity.
Keep only one motion application running when using the robot.

```sh
# Install without starting a container:
wendy run --device Woof --build-type docker --deploy

# When ready to use the hallway demo, stop the other motion application first.
wendy --device Woof cloud device apps start --detach sh.wendy.simulator.go2.examples.hallway
wendy --device Woof cloud device ros2 call /hallway/start std_srvs/srv/Trigger '{}'
wendy --device Woof cloud device ros2 echo /hallway/status
wendy --device Woof cloud device ros2 call /hallway/stop std_srvs/srv/Trigger '{}'
```

The Docker command uses `--ignore-capture-age` for Woof's unsynchronized sensor
clock. It substitutes local arrival time for the 350 ms timeout. Positive,
advancing source stamps and cloud/odometry pairing within 100 ms remain required.
A bounded odometry buffer pairs delayed clouds with the closest capture.
The latest pose still determines odometry freshness.
Delayed captures cannot be distinguished from current captures in that mode.
Remove the flag to restore capture-age checks with synchronized clocks.

To change exploration limits, update the Dockerfile command and rebuild:

```dockerfile
CMD ["python3", "/app/ros_app.py", "--ignore-capture-age", "--max-distance", "5", "--max-seconds", "60", "--prefer", "right"]
```

Use `--autostart` only when automatic motion after sensor readiness is intended.
Hardware deployment and simulated acceptance do not validate real gait,
calibration, stopping distance, or obstacle visibility on Woof.

## Interfaces and movement limits

| Interface | Purpose |
| --- | --- |
| `/utlidar/cloud_base` | Body-frame cloud, projected into 72 levelled sectors |
| `/utlidar/robot_odom` | Pose and orientation, `odom` to `base_link` |
| `/api/sport/request` | Native Unitree Move, API 1008 |
| `/hallway/start`, `/hallway/stop` | Trigger services |
| `/hallway/status` | State, reason, distance, branches, clearances, sensor errors |

Forward speed is 0.55 m/s to satisfy Woof's reported minimum-speed requirement.
Turning is limited to 0.35 rad/s. Forward travel
stops at 85 cm clearance in a 64 cm-wide corridor ahead of the body origin.
Near-body side returns at 36 cm or closer stop the app. Turning requires a
48 cm observed radius around the body and an observed side exit beyond 1.25 m.
An odometry jump, missing sensor, empty required sector, stuck robot, or turn
timeout stops exploration. Measurements and commands use metres and radians.

The cloud converter preserves missing returns as unknown. The navigation layer
requires at least 60% measured returns in a selected angular sector and side
measurements near the body. Sparse clouds can hide obstacles. The app reads
only `/utlidar/cloud_base`; it does not fuse separate lidars or assume their
mounting transforms.

This is a bounded reactive explorer for level hallways, not a building mapping
or route-planning system. It does not detect stairs or drop-offs, identify people,
open doors, use lifts, or guarantee complete coverage of all branches.

## Provenance

`go2_io.py` and `unitree_api/` are copied from the existing standalone Go2
examples. The Unitree message license and pinned upstream reference are included.
`world.py` derives scenes without changing the pinned robot assets or shared
simulator. Validation reports in `validation/` distinguish direct physics from
real DDS tests. No physical motion result is claimed by those reports.
