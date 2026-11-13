# Host sensor loopback check

This test runs on the device host through authenticated `wendy device shell`.
It checks the installed files and vermagic for the running kernel, loads the
drivers, and verifies their character devices. It does not deploy a container.

The image contracts are:

- Dragonwing IQ8, IQ9 and Arduino Uno Q require `v4l2loopback` and `snd-aloop`.
- Jetson images require `v4l2loopback`.

Known boards without a loopback image contract and non-WendyOS platforms are
explicitly skipped. An unknown WendyOS board or missing OS identity fails.
Missing required modules, an incompatible video control ABI, failed module
loads and missing endpoints fail. No physical camera or microphone is needed.
The test leaves loaded modules available for the agent and does not unload them.

Run against a device:

```sh
go/scripts/test-ci.sh -h DEVICE -t host-sensor-loopback
```

Run local regressions without a device or module loads:

```sh
python3 .github/ci-tests/host-sensor-loopback/check_test.py
```

The hardware integration workflow runs the registered test by default.
The Go tests workflow runs the local shell and harness regressions.
