# Roadmap Status

Status reviewed on 19 October 2026. Available products and unfinished integration
work have separate entries below. Planned work has no committed release date
unless one is explicitly stated. See [The Wendy ecosystem](/docs/concepts/ecosystem)
to choose a product and find its setup guide.

## In Progress

- **MLX Swift support**: MLX Swift integration for unified-memory AI workloads. The containerised MLX LLM demo has landed (see Shipped); the remaining work is first-class MLX Swift integration in the Wendy ecosystem.
- **Companion SDK and Cloud v2 integration**: reusable SDK work and mobile support
  for Cloud v2 continue separately from the released iOS app. Check cameras,
  app controls, logs, and notifications in the intended mobile build before
  migrating an existing deployment. See [Moving to Cloud v2](/docs/cloud-migration).
- **Data, Cloud, and BI integration**: device capture exists; storage, querying,
  and analytics workflows need their own configuration and validation. See
  [Wendy Data](/docs/data) for supported capture and current limits.

## Available Beta

- **Wendy Cloud v2**: remote device access and fleet management. The current CLI
  signs into v2 by default; v1 remains available with `--legacy`. App-port and UDP
  tunnels on v2 need updated Cloud, CLI, and Agent versions. Start with the [Cloud guide](/docs/cloud).
- **Headless Mac** — _beta_: deploy native macOS apps to Apple Silicon Mac targets with [Headless Mac](/docs/installation/wendy-agent-macos). Apple Silicon only. Since the initial beta it has gained remote agent self-update, a LAN registry served over mTLS, app supervision and restart parity across agent restarts, and native gamepad entitlements. Beta limitations are documented in the macOS install guide.

## Available Preview

- **Jetson AGX Thor** — _preview_: WendyOS support for AGX Thor-class physical AI workloads, including the dedicated USB-recovery flash path that writes QSPI and internal NVMe directly. Install, deploy, GPU, and camera all work; see the [AGX Thor install guide](/docs/installation/wendyos-nvidia-jetson-agx-thor).
- **Wendy Lite** — _preview_: ESP32 runtime and deployment layer for C5, C6, C61, P4, and S3 targets. Regular native ESP-IDF projects are the recommended app model and deploy with `wendy run`; Swift and other WASM guests remain available as an optional portable runtime. See [ESP32 installation](/docs/installation/wendy-lite-esp32) and the [Wendy Lite reference](/docs/advanced/wendy-lite).

## Shipped

- **Wendy Companion for iOS**: [available in the App Store](https://apps.apple.com/us/app/wendy-companion/id6761771159).
  Its release does not imply that every v1 workflow has a v2 equivalent.
- **Wendy BI 1.0**: analytics and dashboards are available through
  [Wendy BI](https://wendy.dev/products/wendy-bi), with separate access and
  data-connection setup.
- **TeleCollect**: capture and export pose demonstrations on supported Apple
  devices. See the [product introduction](https://wendy.dev/blog/telecollect-pose-data-collection).
- **Arduino Uno Q OS image**: included in
  [WendyOS 0.19.5](https://github.com/wendylabsinc/WendyOS-Builder/releases/tag/0.19.5).
  Image availability and hardware CI coverage are separate milestones.
- **Jetson AGX Orin support**: stable — install, deploy, GPU, and camera.
- **Jetson Orin Nano support**: stable.
- **Raspberry Pi 3, 4, and 5 support** (8GB Pi 5 recommended).
- **Windows support** for the Wendy CLI and VS Code deployment flow.
- **Zero-trust PKI, deployed to GCP** ([WDY-1226](https://linear.app/wendylabsinc/issue/WDY-1226)): the public-key infrastructure backing device-to-cloud auth is deployed, with CI auto-deploy from main. Every cloud connection is mutually authenticated. See [PKI](/docs/advanced/pki).
- **Stagefile build descriptors**: `build.stagefile.yaml` is now a first-class build input alongside Dockerfiles and compose files, with parallel compilation, CUDA stages resolved from the device's GPU architecture, pinned source installs, and cache scoping. The bundled example apps have been converted to it.
- **ROS 2 support**: standard upstream ROS 2 in stock containers with a selectable middleware (CycloneDDS, Fast DDS, RTI Connext, GurumDDS), plus `wendy device ros2` for remote nodes, topics, services, parameters, actions, lifecycle nodes, and rosbags. See [Robotics](/docs/integrations) and the [Foxglove integration](/docs/integrations/foxglove).
- **Swift on the device**: Swift app templates, tutorials, cross-compilation, and remote Swift debugging, plus native Swift apps on macOS targets.
- **Unitree G1 support**: agent install and reflash guides for the G1.
- **Network (IP) cameras**: a third camera transport alongside USB and CSI, with camera discovery, a credential store, and a picker.
- **Python debugging** from the VS Code extension.
- **Local OS updates** without requiring a full system flash.
- **MLX LLM in a local container** ([WDY-1229](https://linear.app/wendylabsinc/issue/WDY-1229)): a working containerised MLX LLM demo, backed by an upstream MLX segfault fix. The demo container is not size-optimised — it ships Ubuntu 24, the CUDA toolkit, and the Swift toolchain, because MLX compiles CUDA just-in-time.

## Planned

- **RISC-V support**.
- **3D geospatial digital twin**: planned; no current release date is committed here.
- **Logging and telemetry demo** — _planned, not yet started_ ([WDY-1231](https://linear.app/wendylabsinc/issue/WDY-1231)): a demo showcasing remote hardware debugging through logging and telemetry. It was originally sequenced against the PKI release, which has since shipped; the demo is still in the backlog.
- **Wendy Swift initiative** — _separate release track_: the Swift toolchain, templates, and debugging support have shipped (see Shipped). The remaining work is the coordinated launch, which is deliberately kept on its own track so it is not diluted by adjacent releases. The original sequencing behind the Pipecat Agent template refresh no longer applies — that work shipped.
