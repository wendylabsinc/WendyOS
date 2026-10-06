---
name: wendy-device-install
description: Install WendyOS on a new Raspberry Pi or Jetson, install the Wendy Agent on a robot or Linux host, or diagnose first boot after flashing. Use for board setup and empty discovery on new hardware; use wendy-install for the developer machine's CLI and wendy-device-debug for an already working installation.
---

# Device installation

Identify the board, carrier, existing OS, host OS, intended storage and network
before selecting an image. Ask only for missing details. For Unitree G1 PC2,
keep vendor Ubuntu and install the Agent; a generic Jetson image does not support
its custom carrier. Distinguish a new board from an installed but unreachable one.

## Plan without writing

Call `wendy_status` and inspect the tools actually exposed by the running MCP
server. Install tools are in the `setup` group, which is not listed until you
call `wendy_tools(groups=["setup"])`. Use `os_install_plan` without a device
connection. It returns the method,
published version/artifact, erase scope, requirements and command argument array.
Jetson planning requires `carrier="developer-kit"`; do not infer this from its SoC.

If the tool is still missing after enabling `setup`, check
`wendy install plan --help`. Update the CLI and
restart the MCP host when authorized, or use the board's installed documentation
through `wendy docs`. Do not invent tools or assume a recent skill means a recent
server binary.

Examples (read-only; JSON output):

```sh
wendy install plan --device-type unitree-g1
wendy install plan --device-type jetson-orin-nano --carrier developer-kit --storage nvme
wendy install plan --device-type raspberry-pi-5
```

For raw-media installation, call `os_list_drives` or
`wendy os list-drives --all --json`. Match path, model and capacity to the intended
SD card/SSD. Request another plan with that exact `drive`. Do not guess a path or
select the first disk. For USB recovery, use the board-specific recovery procedure
and ensure only the intended compatible Jetson is attached before unattended use.

Full Orin recovery writes boot firmware and OS storage. `rootfs_only=true`
writes an attached SD/NVMe drive and leaves QSPI untouched; use it only with
known compatible firmware. AGX Orin requires an explicit NVMe/eMMC choice.

## Execute the selected installation

Show the concrete target, version and erase scope before a disk write. Honor any
authorization already given for that exact target; ask only if destructive scope
or target remains unapproved. Re-list drives immediately before execution if
attachments changed. `--force` skips prompts, not target selection, privileges or
the separate internal-drive protection. Never add `--yes-overwrite-internal` to
work around an unexplained refusal.

Execute the returned argument array through the host terminal using `wendy install`.
Keep its output visible for flash stages and errors. Elevation, recovery buttons,
power cycling and cable changes may need the user. Do not run the interactive
`wendy tour` in an agent pipe, or apply a short tool timeout to an ongoing flash.
If a flash is interrupted, inspect its phase before retrying.

Choose a device name and networking before executing: `--device-name`, WiFi
preseed options, or `--no-wifi` for Ethernet. Use the user's existing credential
input mechanism; keep passwords and enrollment tokens out of reusable commands,
PRs and logs. Cloud pre-enrollment is optional. `provisioning_start` enrolls an
already running agent; it cannot install an OS on a blank board.

For the G1 or another agent-only Linux installation, run the installer **on the
confirmed target host**, using its board guide. Running it on the development
laptop will not install the robot.

## Verify first boot and hand off

Follow the plan's physical steps, then use `device_list(scan=true)` or
`wendy discover --json`. Match the chosen name/address; an unrelated discovered
device is not evidence the new board booted. Call `os_install_verify` with an
explicit address and, for WendyOS, the planned version and device type:

```sh
wendy install verify --address <name-or-IP> \
  --expected-os-version <planned-version> --expected-device-type <board>
```

Retry bounded checks while it boots. A mismatch needs investigation, not removal
of the expected values. Record the public key and supply `expected_public_key`
on later checks. For agent-only installations omit WendyOS version/type checks.
Use `require_enrollment=true` if enrollment was requested. Unknown enrollment is
not success. These checks leave the current MCP session target unchanged.

Connect explicitly, deploy a non-motion smoke app, inspect its container/logs and
test its health endpoint or ROS data. Report separately: image written, expected
agent reachable, OS matched, enrollment checked, and application tested. Use
`wendy-robot-deploy` for robot behavior; a successful flash never proves motion
or sensor correctness.
