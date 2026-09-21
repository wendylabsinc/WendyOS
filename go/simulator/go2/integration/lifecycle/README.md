# Managed VM lifecycle checks

These helpers target explicitly identified virtual robots. The separate
`observer/` application only reads status and subscribes to `/odom`; it never
publishes commands or calls a control endpoint. Run it with `--no-restart`:

```sh
wendy --device vm:go2-sim run \
  --prefix simulator/go2/integration/lifecycle/observer \
  --build-type docker --builder docker --no-restart \
  --user-args=--seconds,45,--expected-vm,go2-sim --yes
```

While it runs, `wendy --device vm:go2-sim device ros2 echo /lowstate --count 1`
checks native inspection alongside another ROS app. Deploy the observer again
to check that app replacement preserves the world's epoch. An optional
`--expected-source` argument also pins its runtime identity.

`two_vm_isolation.py` requires two running, initially disarmed Go2 profiles
with the same expected runtime source. Obtain each verified URL from
`wendy vm robot status <name>`. From the repository's `go/` directory:

```sh
python3 simulator/go2/integration/lifecycle/two_vm_isolation.py \
  --cli /absolute/path/to/wendy \
  --primary go2-sim --primary-url http://127.0.0.1:PRIMARY_PORT \
  --peer go2-peer --peer-url http://127.0.0.1:PEER_PORT \
  --source-digest sha256:EXPECTED_DIGEST \
  --output /tmp/go2-vm-isolation.json
```

The test checks distinct native publishers, explicitly grants one peer ROS
publisher, walks the peer for three seconds at a 0.3 m/s target, and resets
only that peer. The primary must retain its epoch and ownership, continue
physics, remain within 3 cm of its starting pose, and never observe the peer
command identity. Reset must revoke the peer publisher even while it keeps
sending. Cleanup disarms both robots. This is a bounded functional check;
it does not test packet escape or sustained sensor throughput.

Recorded runs and manual pause/reconnect, restart, stopped-runtime and worker
crash checks are described in [validation](../../validation/README.md).
