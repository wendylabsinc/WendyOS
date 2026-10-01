---
name: wendy
description: 'Guidance on building and deploying apps to robots and edge devices using WendyOS, including NVIDIA Jetson, Raspberry Pi, Qualcomm Dragonwing, and MuJoCo simulation.'
---

## Bundled workflows

This package includes the complete Wendy end-user skill group. Read the
relevant workflow below before acting, and resolve its references relative to
that file. These supporting skills need no separate installation.

For first-time setup, read
[wendy-onboarding](references/skills/wendy-onboarding/SKILL.md). It guides the user
from their goal through CLI installation when needed, a local or hosted
connection, and their first physical device or simulator. It also works before
any Wendy MCP tools are connected. Do not assume the user knows Wendy or has
installed its CLI.

Inspect the available tools first. The CLI server uses `wendy_status` and
`device_list`; the ChatGPT gateway uses `list_robots` and `inspect_robot`.
Use the connected server's tools and explicit target identifiers. Use CLI
commands only when a local terminal is available. A skill cannot grant access
or add tools missing from the server.

- [wendy](references/skills/wendy/SKILL.md)
- [wendy-app-lifecycle](references/skills/wendy-app-lifecycle/SKILL.md)
- [wendy-device-debug](references/skills/wendy-device-debug/SKILL.md)
- [wendy-device-install](references/skills/wendy-device-install/SKILL.md)
- [wendy-device-ops](references/skills/wendy-device-ops/SKILL.md)
- [wendy-entitlements](references/skills/wendy-entitlements/SKILL.md)
- [wendy-install](references/skills/wendy-install/SKILL.md)
- [wendy-mcp-setup](references/skills/wendy-mcp-setup/SKILL.md)
- [wendy-onboarding](references/skills/wendy-onboarding/SKILL.md)
- [wendy-project-setup](references/skills/wendy-project-setup/SKILL.md)
- [wendy-robot-deploy](references/skills/wendy-robot-deploy/SKILL.md)
- [wendy-template-app](references/skills/wendy-template-app/SKILL.md)


# WendyOS

WendyOS is an Embedded Linux operating system for edge computing. It supports:
- NVIDIA Jetson devices (production with OTA updates); target JetPack 7.2 or later
- Raspberry Pi 4/5 (edge devices)
- ARM64/AMD64 VMs (development)

## Learning About Wendy

Before helping with Wendy commands, explore the command tree with the built-in help:

```bash
wendy --help
wendy <command> --help
```

Use `--json` for list/status commands and agent-backed `run --detach`. Detached
run returns a JSON result with the target, app and available HTTP URLs; progress
goes to stderr. Build, attached run and flash still emit progress text. Supply
explicit targets and choices to avoid interactive pickers. (There is no `-j` shorthand.)

## Common Tasks

For first-time setup, use `wendy-onboarding` to install and verify a physical
device or start a local simulator while waiting for hardware.

- Run an app: `wendy run`
- Create a new project: `wendy init`
- Discover devices: `wendy discover`
- Update agent: `wendy device update`
- Configure WiFi: `wendy device wifi connect`
- Install WendyOS on an external drive: `wendy os install`
- Set a device as default using `wendy device set-default`
- Check the default device with `wendy device get-default`

### Create and deploy an app

`wendy init` creates a WendyOS container project (Python, Swift, Rust, Node or C++).
Use explicit template variables and `--assistant skip --git-init no` for unattended
scaffolding. When already inside the requested destination, include `--here` so
`--app-id` does not create another directory. See `wendy-template-app`.

`wendy run --device <selector> --detach --yes` builds, transfers and starts the app.
The MCP `run` tool uses its explicit `device` or the connected session's target;
legacy `device_name` selects cloud deployment. Verify the returned target, container
state, logs and application health separately: detached run skips readiness waits.
For a local VM, `--device vm:dev` selects the agent; it is not a DNS hostname or
HTTP URL. With default user networking, fetch the detached result's `url` (for
example `http://127.0.0.1:18880`), rather than `vm:dev`, `dev`, or the guest's
`10.0.2.15` address. Declare the app's HTTP port in an `http` entitlement so Wendy
can forward and report it. `readiness: "not_checked"` means start was acknowledged,
not that the HTTP endpoint is healthy; verify the expected response separately.
Wendy Lite uses a separate MCU/WASM workflow; see `wendy-lite` for ESP32.

### `wendy device wifi connect` — Set Up WiFi

Configures WiFi credentials on a connected device:

```bash
wendy device wifi connect
```

This sends WiFi SSID and password to the device so it can connect to the local network. The device must be reachable over USB or an existing connection first.

## Setup and Configuration

Wendy CLI connects to a device over gRPC (TCP) port 50051. If Wendy CLI is not installed yet, run `curl -fsSL https://install.wendy.dev/cli.sh | bash`.

Devices are discovered over USB or LAN. On Linux, USB tethering may need a
one-time host setup: run `wendy discover` in a terminal and accept its USB-C
setup prompt (it needs sudo). An empty scan can also mean a blank board.
Use `wendy-device-install` and `os_install_plan` for initial installation and
`os_install_verify` for first boot. Full Jetson recovery updates boot firmware;
rootfs-only media writes do not. Unitree G1 PC2 keeps vendor Ubuntu and receives
the Agent, not a generic Jetson image.

For robotics, use `wendy-robot-deploy`: inspect actual ROS topics, QoS, frames,
clocks and DDS scope, test in simulation, deploy without automatic motion, and
verify the app's output before any authorized physical test. Check the live MCP
tool list; newer skill text cannot add tools to an older running server.

## Development

WendyOS is a Linux-based containerized operating system. It uses Linux containers to run your apps.

WendyOS uses Swift.org as its flagship language. This uses Swift Package Manager and the Swift Container Plugin to build and run your app. Wendy CLI will cross compile Swift for you.

Other programming languages are supported, but require the use of a Dockerfile or Containerfile to build your app.

### Entitlements

WendyOS uses an entitlement system, managed through `wendy.json`, to manage permissions for your app. This reflects how your container will be set up on the device.

See `references/wendy.json.md` for detailed entitlement configuration.

### Quick Start

1. Create a new Swift project or navigate to an existing one
2. Initialize wendy.json: `wendy init`
3. Add required entitlements (e.g., for a web server): `wendy project entitlements add network` (the mode is chosen at the interactive prompt; there is no `--mode` flag)
4. Run on device: `wendy run`

### Common Entitlements

| Entitlement | Use Case |
|-------------|----------|
| `network` (host mode) | Web servers, HTTP APIs, incoming connections |
| `gpu` | ML inference/computer vision (Jetson), board telemetry (Raspberry Pi) |
| `display` | Present to local monitor as Wayland client |
| `camera` | Camera access, video capture |
| `audio` | Microphone, speakers |
| `bluetooth` | BLE devices, Bluetooth communication |

## Remote Debugging

`wendy run --debug` starts the app under a debugger.

For **Python** apps the agent rewrites the entrypoint to run under `debugpy`.
The CLI does **not** inject debugpy into the image, so the image must already
bundle it — for Stagefile projects the CLI checks the pip requirements up front
and fails fast rather than letting the app crash-loop with
`No module named debugpy`.

For **Swift** apps, attach with the WendyOS VS Code extension, which generates a
debug configuration per executable target. See the
[VS Code extension guide](https://docs.wendy.dev/latest/remote-debugging/vscode-extension) for
the ports and prerequisites — the debugger wiring lives in that extension, not
in this CLI.

## Observability

WendyOS runs a local OpenTelemetry collector on each device. Apps should report telemetry (logs, metrics, traces) to this local collector.

### Configuration

Use HTTP protocol (not gRPC) for OTel exports:

```swift
import OTel

var config = OTel.Configuration.default
config.traces.otlpExporter.protocol = .httpProtobuf
config.traces.otlpExporter.endpoint = "http://localhost:4318"
config.metrics.otlpExporter.protocol = .httpProtobuf
config.metrics.otlpExporter.endpoint = "http://localhost:4318"
config.logs.otlpExporter.protocol = .httpProtobuf
config.logs.otlpExporter.endpoint = "http://localhost:4318"

let observability = try OTel.bootstrap(configuration: config)
```

Or via environment variables:

```bash
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
```

The local collector handles forwarding telemetry to your backend infrastructure.

## Troubleshooting

| Problem | Solution |
|---------|----------|
| Device not found | Check USB/LAN connection, run `wendy discover`. On Linux with USB-C, the CLI offers to run its USB setup step automatically — accept that prompt. |
| Network access denied | Add network entitlement with host mode |
| GPU not detected | Add gpu entitlement (Jetson for CUDA, Raspberry Pi for board telemetry) |
| Camera not found | Add camera entitlement, verify camera at `/dev/video0` (for CSI cameras also check `/run/udev` is present on host) |
| Build fails | Check Swift version compatibility, try `wendy run --verbose` |

## Reference Files

Load these files as needed for specific topics:

- **`references/wendy.json.md`** - App configuration, entitlements (network, gpu, display, camera, audio, bluetooth), common configurations, CLI commands

## Further Reading

WendyOS documentation at https://docs.wendy.dev/latest/
