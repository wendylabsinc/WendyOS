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
  `<output-root>/<run-id>/tests`. They accept options such as `--filter`,
  `--agent-address`, `--agent-user`, and `--verbose`.
- `Scripts/E2EReview.sh` and `Scripts/E2EReview.ps1` review tests that include
  `// AI:` comments and write `review.md` files into the run directory.
- `Scripts/E2EReport.sh` and `Scripts/E2EReport.ps1` render
  `<output-root>/<run-id>/report.html`.

Typical local setup and full run:

```bash
cd swift
bash Scripts/E2ESetup.sh
make e2e-run
```

Makefile E2E helpers default to global temporary output roots:

- Unix/macOS: `/tmp/wendy/e2e/<run-id>`
- Windows: `C:\Windows\Temp\wendy\e2e\<run-id>`

Set `WENDY_E2E_OUTPUT_DIR` or pass `--output-dir` to the scripts when you need a
custom artifact location. If Swift Testing writes terminal control characters
that are invalid in XML 1.0, the harness sanitizes the xUnit file in place and
preserves the original as `test-results-swift-testing.raw.xml`.

The Makefile includes helpers for the common cases:

- `make e2e-test` runs the E2E suite against the local host and writes raw
  artifacts only.
- `make e2e-run` runs local tests, reviews results, renders the HTML report, and
  opens it in the browser on macOS.
- `make e2e-run-mac-mini` runs the full local pipeline against
  `mac-mini.local`.
- `make e2e-run-jetson-orin-nano` runs the full local pipeline against
  `wendyos-jetson-orin-nano.local`.
- `make e2e-run-raspberry-pi-5` runs the full local pipeline against
  `wendyos-raspberry-pi-5.local`.

The device-targeted `e2e-test-*` helpers are also available when you only need
raw test artifacts. Device-targeted helpers accept a `DEVICE` override:

```bash
make e2e-run-mac-mini DEVICE=my-mac.local
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
