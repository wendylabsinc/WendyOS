# Wendy Agent Swift

Swift sources for the Wendy Agent macOS app and shared agent core.

## Overview

This directory contains:

- `WendyAgentCore/` — the shared Swift package that implements the agent runtime, gRPC services, Bonjour advertising, and local OpenTelemetry ingestion.
- `WendyAgentMac/` — a lightweight macOS menu bar app that launches and manages `WendyAgent`, organized into `Sources/`, `Assets/`, and `Design/`.
- `WendyE2ETests/` — a Swift package for end-to-end tests of the Wendy CLI and agent.
- `WendyAgent.xcworkspace/` — the Xcode workspace for working on the app and package together.
- `Scripts/` — helper scripts, including protobuf generation.

By default, the agent starts:

- gRPC on port `50051`
- local OpenTelemetry ingestion on port `4317`

## Requirements

- macOS 15+
- Xcode with Swift 6.2 support

## Getting started

### Open in Xcode

Open the workspace:

```bash
open WendyAgent.xcworkspace
```

### Build the shared package

```bash
cd WendyAgentCore
swift build
```

### Run tests

```bash
cd WendyAgentCore
swift test
```

### Run E2E tests locally

The E2E harness lives in `WendyE2ETests/` and runs commands over SSH, even
for local runs. The tests expect `wendy-agent` to already be running on the
agent target, whether that target is the local host or a remote device. From
the `swift/` directory, use the Makefile for common workflows or these scripts
when you need lower-level control:

- `Scripts/E2ESetup.sh` dispatches to `Scripts/E2ESetup.macOS.sh` or
  `Scripts/E2ESetup.ubuntu.sh` to check and prepare the host for E2E runs. On
  macOS it asks for sudo access, installs Homebrew if needed, installs required
  tools, installs Swift via swiftly if needed, and configures passwordless SSH
  loopback. On Ubuntu it installs the required packages, Swift if needed, and
  SSH server settings for parallel test bursts.
- `Scripts/E2ETest.sh` and `Scripts/E2ETest.ps1` run the Swift E2E test package,
  build the managed CLI into the CLI run directory, write per-test sandboxes
  under the CLI and agent run directories, and write recordings under
  `<output-dir>/<attempt-id>/observations/`. They accept options such as `--filter`,
  `--agent-address`, `--agent-user`, `--test-timeout`, and `--verbose`.
- `Scripts/E2EAnalyze.sh` and `Scripts/E2EAnalyze.ps1` analyze attempt
  directories in an output directory. By default, they only consider attempts
  matching the current run ID, such as `swift-e2e-tests.local0000`. They support
  `aggregate`, `review`, `report`, and `all` stages; the aggregate stage updates
  matching run directories in place.
- `Scripts/E2EReport.sh` and `Scripts/E2EReport.ps1` render an existing run
  report at `<output-root>/<workflow-name>.<run-id>/index.html`.

Typical local setup and two-step run:

```bash
cd swift
bash Scripts/E2ESetup.sh
make e2e-test
make e2e-analyze
```

Makefile E2E helpers default to the repository-local output root:

- `<repo-root>/Build/e2e/<run-id>`

Set `WENDY_E2E_OUTPUT_DIR` or pass `--output-dir` to the scripts when you need a
custom artifact location. If Swift Testing writes terminal control characters
that are invalid in XML 1.0, the harness sanitizes the xUnit file in place and
preserves the original as `test-results.raw.xml`.

The Makefile includes helpers for the common cases:

- `make e2e-test` runs the E2E suite against the local host and writes attempt
  artifacts only.
- `make e2e-test-wendy DEVICE=<host>` runs an attempt against a WendyOS
  device.
- `make e2e-test-linux DEVICE=<host>` runs an attempt against a Linux device.
- `make e2e-test-macos DEVICE=<host>` runs an attempt against a macOS device.
- `make e2e-test-wendy-<name>`, `make e2e-test-linux-<name>`, and
  `make e2e-test-macos-<name>` run against `<name>.local`.
- `make e2e-test-mac-mini` runs an attempt against `mac-mini.local`.
- `make e2e-test-jetson-orin-nano` runs an attempt against
  `wendyos-jetson-orin-nano.local`.
- `make e2e-test-raspberry-pi-5` runs an attempt against
  `wendyos-raspberry-pi-5.local`.
- `make e2e-aggregate` integrates attempts for the current run ID, updating run
  directories in place.
- `make e2e-review` reviews existing run results.
- `make e2e-report` renders run HTML reports and opens the newest report on
  macOS.
- `make e2e-reference` generates Swift E2E HTML reference documentation and
  opens it.
- `make e2e-analyze` runs `e2e-aggregate`, `e2e-review`, and `e2e-report` in
  order for the current run ID.

Generic device-targeted helpers require `DEVICE`; named device helpers accept a
`DEVICE` override:

```bash
make e2e-test-macos DEVICE=my-mac.local
make e2e-test-macos-my-mac
make e2e-test-mac-mini DEVICE=my-mac.local
make e2e-analyze
```

## USB camera controls on macOS

The existing `camera controls` and `camera set-control` commands (including
`--reset` and `--reset-all`) use the same RPCs and control names as Linux. Get the numeric camera ID from `camera list`; IDs are discovery positions,
not durable USB identities. Built-in/network cameras and ambiguous USB identities
are rejected. Only standard scalar controls advertised by the device's UVC
camera-terminal/processing-unit descriptor are exposed; vendor extension units
and compound pan/tilt/white-balance controls are not inferred.

Use a CLI containing the matching preflight update: older CLIs refuse Mac
controls before sending an RPC. New CLIs still let an older Agent return its
honest unsupported-operation error. Like Linux, the camera does not require
enrollment; older CLIs still ask for it on an unenrolled Mac.

The backend uses public, non-seizing IOKit USB control requests. It never resets,
detaches or reconfigures the camera, and does not open an AVFoundation capture
session to enumerate controls. Custom AVFoundation exposure-duration/ISO setters
are unavailable on macOS. USB requests are serialized on a dedicated executor,
with one-second transfer timeouts and range/step/inactive/readback validation.

`exposure_time_absolute` is in UVC/V4L2 units of **100 microseconds**, not ISO.
`auto_exposure` uses V4L2 values: 0 automatic, 1 manual, 2 shutter priority,
3 aperture priority. A camera may support only some of those modes; unsupported
menu values are rejected even when inside the reported minimum/maximum.
Auto-mode changes are applied before dependent controls in a batch.

Successful `persist=true` changes are stored by exact AVFoundation USB UID in
`~/Library/Application Support/sh.wendy.WendyAgentMac/camera-controls/controls.json`
and reapplied after Agent startup and once its capture session produces a first
frame on every reopen. `persist=false` applies once and does not remove a prior
persisted setting, matching Linux; use Reset to forget it. Reset with no names
forgets every persisted control for that UID. Forgetting precedes restoring
camera-reported defaults, even if a now-inactive control cannot be restored;
per-control results state the outcome. Persistence failure is reported rather
than silently claiming a durable change.

USB topology changes produce a different UID and do not inherit old settings.
Restoration for capture sessions opened by **other applications** is not owned
by the Agent: another application/firmware may reset controls independently.
The first Agent frame can precede restoration; subsequent frames use the
reapplied settings. Hardware behavior, reboot and visible exposure changes must
be qualified per camera; backend support does not establish detection accuracy.

## Project structure

```text
swift/
├── README.md
├── Scripts/
├── WendyAgent.xcworkspace/
├── WendyAgentCore/
├── WendyE2ETests/
└── WendyAgentMac/
    ├── Assets/
    ├── Design/
    ├── Sources/
    └── WendyAgentMac.xcodeproj/
```

## Notes

- The macOS app is implemented as an accessory app with a status menu, with source files under `WendyAgentMac/Sources/` and resources separated into `Assets/` and `Design/`.
- The core package exposes `WendyAgent`, which manages startup, shutdown, service lifecycle, and status observation.
- `Scripts/GenerateProto.sh` is intended for regenerating protobuf/gRPC sources when the service definitions change.
