# Wendy Agent Swift

Swift sources for the Wendy Agent macOS app and shared agent core.

## Overview

This directory contains:

- `WendyAgentCore/` — the shared Swift package that implements the agent runtime, gRPC services, Bonjour advertising, and local OpenTelemetry ingestion.
- `WendyAgentMac/` — a lightweight macOS menu bar app that launches and manages `WendyAgent`, organized into `Sources/`, `Assets/`, and `Design/`.
- `WendyE2ETests/` — a small standalone Swift package for script-like end-to-end tests built around a `Machine` helper.
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
the `swift/` directory, use these scripts directly when you need lower-level
control:

- `Scripts/SetupE2E.sh` checks and prepares the host for E2E runs. On macOS it
  verifies the required tools and configures passwordless SSH loopback. On
  Ubuntu it also installs the required packages, Swift if needed, and SSH server
  settings for parallel test bursts.
- `Scripts/TestE2E.sh` runs the Swift E2E test package, writes command records
  into `Build/e2e-report.<run-id>/recording`, and writes the HTML report to
  `Build/e2e-report.<run-id>/index.html`. It accepts options such as `--filter`,
  `--agent-address`, `--agent-user`, and `--verbose`.

Typical local setup and run:

```bash
cd swift
bash Scripts/SetupE2E.sh
make test-e2e
```

The Makefile includes helpers for the common cases:

- `make test-e2e` runs the E2E suite against the local host.
- `make test-e2e-mac-mini` runs against `mac-mini.local`.
- `make test-e2e-jetson-orin-nano` runs against
  `wendyos-jetson-orin-nano.local`.
- `make test-e2e-raspberry-pi-5` runs against
  `wendyos-raspberry-pi-5.local`.

Device-targeted helpers accept a `DEVICE` override:

```bash
make test-e2e-mac-mini DEVICE=my-mac.local
```

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
