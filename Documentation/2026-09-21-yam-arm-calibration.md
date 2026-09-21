# Guided YAM arm calibration

`wendy calibrate arms <dof>` launches the standalone
[`yam-arm-calibrator`](https://github.com/wendylabsinc/yam-arm-calibrator)
wizard. Supported YAM variants have six arm joints; the gripper is separate.

```sh
wendy calibrate profiles
wendy calibrate arms 6 --demo
wendy calibrate arms 6 --demo --auto --json
wendy calibrate arms 6 --device <thor>
wendy calibrate arms 6 --device <thor> --model big_yam --channel can3 --id right-arm
```

The guided flow identifies the model and physical arm, captures the known
reference pose, records each joint's working range with live position feedback,
captures gripper endpoints, checks return-to-reference repeatability, and saves
a new version after review. Omitted options are selected in the wizard.

By default hardware calibration uses the existing device selector and
authenticated HostShell transport. `--local` uses SocketCAN on the current Linux
host. `--demo` is always local, never opens hardware, and marks outputs as demo
data. `--auto` is accepted only with `--demo`. Unsupported DOF/model/gripper
requests and noninteractive hardware requests fail before device connection.

## Runtime and storage

The CLI bundles a deterministic standard-library Python zipapp under
`go/internal/cli/armcalibrator/`. `UPSTREAM.json` pins its source commit and
SHA-256. The archive is decoded and verified into a temporary directory, then
run under Python 3.10+ with the caller's terminal attached. No host package
installation, pip download, robot container, or public release is needed.
Temporary runtime files are removed on completion or interruption.

Results are saved on the machine running the wizard. For a remote run that is
the Thor, typically under the shell user's `~/.cache/wendy/calibration/arms`.
`--output` overrides that directory on the same machine. Each run contains
`calibration.json`, raw `measurements.json`, and a `COMPLETE.json` checksum marker.
Existing calibrations are retained; no motor settings are overwritten.

## Hardware scope

The backend sends only DM register reads, verifies identity/configuration, and
tracks continuous output-shaft position. It never constructs the I2RT motor
chain, enables or disables motors, commands motion, configures CAN, or writes
motor RAM/Flash. The operator stops controllers, disables and supports the arm,
and moves it manually. A bus lock, foreign-command detection, bounded exchanges,
sample-gap checks and explicit save prompt bound the collection flow.

This is LeRobot-inspired calibration with a YAM-specific radian schema, not
the SO-101's Feetech calibration routine. Camera intrinsics, hand-eye calibration,
motorized homing, torque calibration, and physical qualification are outside
this command. Motor-control integrations must consume and validate the saved
offsets explicitly; saving does not automatically alter an existing controller.

## Development checks

```sh
go test ./go/internal/cli/armcalibrator ./go/internal/cli/commands -run '^TestCalibrate' -count=1
go build -o bin/wendy ./go/cmd/wendy
bin/wendy calibrate arms 6 --demo --auto --json
```

The standalone repo also tests profile math, CAN framing/filtering, timeouts,
wrap tracking, identity mismatch, interrupted/cancelled collection, immutable
storage, and deterministic archive generation. Hardware calibration remains
unverified until an operator completes a run on the actual arm.
