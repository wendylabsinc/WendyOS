# Native example acceptance

Build the Patrol Dockerfile, start an isolated managed Go2 image with
`GO2_AUTO_APP_CONTROL=1`, and mount the current simulator implementation into that
runtime when testing unbuilt source changes. Share only the simulator container's
network namespace with the test container. Mount `patrol_smoke.py` at
`/test/patrol_smoke.py` in the Patrol image and run:

```sh
python3 /test/patrol_smoke.py --simulator-url http://127.0.0.1:8890
```

The test verifies simulator identity before publishing, drives a 0.5 m square
away from the default box, checks all four waypoints, native command reception,
and two seconds of zero commands after completion. It uses the application from
`/app`, including its own generated Unitree Request types.

The [recorded result](../../validation/native-examples.json) passed with real ROS
and MuJoCo. A separate run of the default 1 m route stopped for a sector occluded
by the box. That remains unknown space; the test does not fill missing returns or
relax the application's freshness and obstacle limits.
