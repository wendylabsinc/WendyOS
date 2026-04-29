# Wendy Agent Swift

Swift sources for the Wendy Agent macOS app and shared agent core.

## Overview

This directory contains:

- `WendyAgentCore/` — the shared Swift package that implements the agent runtime, gRPC services, Bonjour advertising, and local OpenTelemetry ingestion.
- `WendyAgentMac/` — a lightweight macOS menu bar app that launches and manages `WendyAgent`, organized into `Sources/`, `Assets/`, and `Design/`.
- `WendyAgentE2ETests/` — a small standalone Swift package for script-like end-to-end tests built around a `Machine` helper.
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

## Project structure

```text
swift/
├── README.md
├── Scripts/
├── WendyAgent.xcworkspace/
├── WendyAgentCore/
├── WendyAgentE2ETests/
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
