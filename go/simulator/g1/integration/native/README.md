# G1 native acceptance

This finite app uses the pinned Unitree Python SDK and independently built
stock ROS message packages. It imports no simulator runtime modules. Validate
the G1 simulation identity before opening domain0/loopback DDS; by default the
network namespace must contain only `lo`. An explicit `--managed-vm NAME`
additionally pins that VM's source digest, robot kind, policy and DDS isolation.

From the repository's `go` directory, after building the G1 runtime image:

```sh
docker build -t wendy-g1-native-test:dev \
  --build-arg SIMULATOR_IMAGE=wendy-g1-managed:dev \
  -f simulator/g1/integration/native/Dockerfile simulator/g1
docker run --rm --network container:wendy-g1-ros-validation \
  wendy-g1-native-test:dev python3 /app/native/exercise.py
```

The separate runtime must itself use `--network none`, native ROS enabled and
the standing G1 controller. The test performs bounded native Loco motion, stop,
Damp, ZeroTorque, resets, and a short29-joint PR low-level command with wrist
motion. It always disarms during cleanup. A pass establishes these functional
checks only, not sustained stream rates, factory controller fidelity or full
G1 API compatibility.

The [recorded isolated SDK run](../../validation/native-sdk.json) passed in
9.46 seconds: 0.652 m forward displacement from the explicit 0.3 m/s Loco command,
valid 35-slot HG state/CRC, Damp/ZeroTorque and unsupported-API responses, and
0.099 rad wrist motion through native PR LowCmd with both watchdogs working.
The report records exact runtime/SDK image IDs and source hashes. Rendering
was disabled for this native check; this is not VM or sustained-rate evidence.

`tests/test_native_interfaces.py` checks actual HG ROS CDR round trips against
the SDK and its independent CRC packer without DDS or a physics world. The
G1 ingress package has a separate loopback-only C++ datagram integration test.
