# Mac Support for Wendy

This project adds macOS as a supported Wendy target. Apple Silicon's unified memory architecture and MLX/CoreML stack make it a compelling platform for edge AI and embedded workloads — the same class of applications Wendy already supports on Jetson and Raspberry Pi.

## Background

Wendy is made up of two components — a **CLI** (`wendy`) and an **agent** (`wendy-agent`) — that communicate over gRPC.

**The CLI** runs on the developer's machine. Its core workflow is `wendy run`: read the project's `wendy.json`, build a container image, discover a target device, upload it, and stream output back. It also handles device discovery (mDNS, USB, Bluetooth), WiFi and Bluetooth configuration, hardware capability queries, app lifecycle, OS updates, and cloud authentication.

**The agent** runs on every target device (Raspberry Pi, NVIDIA Jetson, or any Linux system). It manages containers, networking, Bluetooth peripherals, audio devices, telemetry collection, and device provisioning.

## Swift vs. Go

The Wendy CLI and agent were rewritten in Go because the Swift implementation had a **heavy dependency graph** that slowed compile times, while Go offers **mature container SDKs** (containerd, go-containerregistry) and a strong ecosystem for **networking and orchestration**. That rationale holds for Linux.

The Mac agent needs deep integration with **Apple platform frameworks** — Bluetooth, Bonjoir, AWDL, Keychain, etc. — which are **only exposed as Swift/ObjC APIs**. Using Go would require writing these pieces in Swift anyway, exposing C interfaces, and bridging them back — the bridging surface area would rival the **agent logic itself**.

Since the CLI and agent already communicate over **gRPC**, the cleanest approach is to write the Mac agent entirely in **Swift** while keeping the Go CLI unchanged. The stubs are **generated from shared `.proto` definitions**, so the contract is enforced at build time regardless of language.

## The Easy Bits

The legacy Swift-based Wendy implementation for Linux already has modules that are platform-agnostic and can be reused directly:

- **WendySDK** — certificate generation, CSR signing, chain validation (pure `swift-certificates` + `swift-crypto`)
- **Analytics** — anonymous usage telemetry
- **OpenTelemetry** — log/metrics/traces receivers and broadcaster
- **Provisioning** — enrollment flow and config persistence
- **AppConfig** — `wendy.json` parsing

The following need macOS-native backends to replace their Linux (D-Bus/BlueZ/PipeWire) implementations:

- **Bluetooth** → CoreBluetooth
- **Audio** → CoreAudio
- **WiFi** → CoreWLAN
- **Discovery** → Bonjour

The cleanest starting point is to take the legacy Swift codebase, strip the Linux-specific code, and fill in the macOS implementations.

## Mitigating Lack of Native Containers

On Linux, containers solve three problems at once: **packaging** (ship the app with all its dependencies), **transfer** (push a self-contained image to the device), and **isolation** (sandbox the running process with namespaces and cgroups).

Docker on macOS runs containers inside a Linux VM. Apps deployed that way cannot access the hardware and frameworks that make Apple Silicon compelling in the first place — Unified Memory, GPU, Neural Engine, Secure Enclave, CoreML, Metal, and so on. Mac apps need to run **natively**, not inside a Linux container.

macOS has no native equivalent that bundles all three concerns. Instead, we need to address each one separately: how do we **package** an app and its dependencies, how do we **transfer** it to the device, and how do we **isolate** it once it's running. The subsections below explore options for each.

### Docker vs. Container Images

The Wendy CLI has currently two data-transfer mechanisms:

1. **App deployment** — container images uploaded via `WendyContainerService`
2. **Agent updates** — binary blobs streamed via `WendyAgentService`

That said, Docker (OCI) container images are just layered tar archives with a manifest — there is nothing Linux-specific about the format itself. The same packaging can carry a macOS app bundle, and the existing layer-dedup and transfer logic in `WendyContainerService` would work unchanged.

This leaves the following options:

- **Reuse `WendyContainerService`** — package the Mac app as an OCI image, upload it through the existing interface, and have the Mac agent unpack and run it natively instead of handing it to containerd.
- **Introduce a new transfer interface** — a dedicated gRPC service for pushing arbitrary artifacts to the device.
- **(Ab-)use git** — git is already optimized for content-addressable storage and efficient delta syncing. The simplest version would use `git push` over SSH, which every macOS installation has built-in. (Or something more advanced over
gRPC, etc.)

### Homebrew & Friends

On Linux, a container image ships the entire userland — libraries, tools, runtimes — so every app sees a self-contained, reproducible environment. macOS has no equivalent, but we can approximate it in layers of increasing sophistication:

1. **Brewfile support** — the simplest option. The agent reads a `Brewfile` (or similar manifest) from the app bundle and ensures the listed dependencies are installed on the device via Homebrew. Lightweight, familiar to macOS developers, but all apps share a single Homebrew prefix so version conflicts are possible.

2. **Isolated per-app Homebrew prefixes** — each app gets its own Homebrew installation under a dedicated directory. The agent manages the Homebrew lifecycle (install, update, etc.). This eliminates conflicts between apps at the cost of more disk space and build time (due to non-standard prefix path, most unix tools needs to be build from source rather than download prebuilt binaries).

3. **Pre-built dependency layers** — take option 2 further by building the Homebrew tools with a custom prefix once per app (on a CI machine or the developer's Mac), packaging it as one or more OCI layers, and pushing it to devices through the existing `WendyContainerService` transfer path. Dependencies are built once, potentially centrally cached, and shared across a fleet — only the app layer itself differs per deployment.

### Sandboxing

On Linux, containers isolate apps from the host and from each other via kernel namespaces and cgroups. macOS has no direct equivalent, but there are several options ranging from "no isolation" to "full App Sandbox":

1. **No isolation** — run the app as-is under the agent's user. Simplest to implement, good enough for trusted first-party workloads on a developer's own machine. This is what the prototype does today.

2. **Dedicated user per app** — create a macOS user account for each deployed app and launch the process under that UID. Provides basic filesystem and process isolation through standard Unix permissions without requiring any Apple-specific APIs.

3. **App Sandbox** — package the app as a proper `.app` bundle with sandbox entitlements. This is Apple's recommended isolation mechanism and gives fine-grained control over file access, networking, and hardware. It requires code signing, but local signing with a Developer ID certificate should suffice — no notarization needed.

4. **`sandbox-exec`** — launch the app under a custom `.sb` sandbox profile. Apple has deprecated this API and encourages App Sandbox instead. The sandbox profile rules are also notoriously tricky to get right.

## Marketing: Killer Mac (-mini) Demos

The demos need to show things that only a Mac can do well — and that clearly benefit from Wendy's deploy-and-manage workflow rather than just running locally.

## Plan

**TODO: flesh out.**

Random thoughts:

- Mac to Mac only
- Agentinc engineering instead of vibe coding: use extensive unit/integration tests as spec, let AI flesh that out so it's runnable, then let AI use that as a feedback loop while writing the actual code
- Iterative, start with the simplest, then expand progressively (i.e. no isolation > Signing & App Sandbox > sandbox-exec support)
- Provide hooks as basic building blocks as an escape hatch where possible such that users can build their own mechanisms