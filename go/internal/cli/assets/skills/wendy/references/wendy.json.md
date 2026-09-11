# wendy.json Reference

The `wendy.json` file configures your WendyOS application's identity and entitlements (permissions).

## File Structure

```json
{
  "appId": "com.example.myapp",
  "version": "1.0.0",
  "platform": "linux",
  "entitlements": [
    { "type": "network", "mode": "host" }
  ]
}
```

| Field | Description |
|-------|-------------|
| `appId` | Unique identifier (reverse domain notation recommended) |
| `version` | Application version string |
| `platform` | Target platform: `linux`, `wendyos`, `wendy-lite`, or `darwin` |
| `brewfile` | Optional relative Brewfile path for native Darwin deployments; project-root `Brewfile.wendy` is auto-detected |
| `entitlements` | Array of entitlement objects specifying required permissions |

## Platforms

| Value | Description |
|-------|-------------|
| `linux` | Linux edge device; the device architecture is inferred |
| `wendyos` | Compatibility alias for `linux`; apps run in containers |
| `wendy-lite` | ESP32 WASM target |
| `darwin` | Native macOS execution through [Wendy for Mac](/docs/installation/wendy-agent-macos) |

Omit `platform` to target Linux. Existing `"wendyos"` configs are accepted as an alias and resolve to `linux` before Docker or Apple Container builds.

Use `"darwin"` for Apple Silicon Mac targets managed by Wendy for Mac. The CLI builds SwiftPM or Xcode projects on a Mac development machine, syncs the build output to the Mac agent, and starts the app as a native macOS process. Darwin apps run natively and non-containerized, so WendyOS Linux container semantics and hardware entitlements do not apply.

Minimal SwiftPM/Linux container configuration:

```json
{
  "appId": "com.example.hello-linux",
  "version": "1.0.0",
  "language": "swift",
  "platform": "linux"
}
```

Native SwiftPM and Xcode Mac apps can use Homebrew dependencies with Brew Bundle.
Place `Brewfile.wendy` at the project root for auto-detection, or set `"brewfile":
"ops/Brewfile"` to use a relative path. A plain project-root `Brewfile` is left
for developer-machine setup unless explicitly referenced. `wendy run` syncs the
Wendy Brewfile to the target Mac and Wendy Agent runs `brew bundle --file <synced Brewfile>`
before starting the app. Homebrew must already be installed on the target Mac.

## Entitlements Overview

WendyOS uses a security-first approach where applications are sandboxed by default:
- No network access unless explicitly granted
- Hardware devices (cameras, microphones, GPUs) not accessible by default
- Bluetooth and other system interfaces require explicit permission

## Available Entitlements

### Network Entitlement

Controls network access for your application.

```json
{ "type": "network", "mode": "host" }
```

| Mode | Description |
|------|-------------|
| `host` | Shares host's network stack. Required for HTTP servers and services accepting incoming connections. |
| `none` | Isolated network namespace with no network access. For offline data processing tasks. |

**Important**: Web servers and applications accepting incoming connections need `"mode": "host"`.

### GPU Entitlement

Enables GPU or board-telemetry access on supported devices.

```json
{ "type": "gpu" }
```

When enabled:
- **NVIDIA Jetson**: Adds application to video group, injects NVIDIA CDI specs, sets CUDA env vars
- **Raspberry Pi**: Exposes `/dev/vcio` (VideoCore mailbox) for board telemetry (power, voltage, temperature)

**Note**: GPU entitlement behavior is hardware-specific.

### NPU Entitlement

Enables on-SoC neural accelerator access for on-device inference.

```json
{ "type": "npu" }
```

When enabled:
- **Qualcomm (Hexagon DSP)**: Bind-mounts the non-secure `/dev/fastrpc-*` nodes and `/dev/dma_heap/system`, and adds the app to the `fastrpc` and `dmaheap` groups
- **Other hardware**: No grant; the entitlement is inert

**Note**: the `-secure` FastRPC nodes are the signed-PD path and are never granted.

### Camera Entitlement

Camera / V4L2 device access.

```json
{ "type": "camera" }
{ "type": "camera", "allowlist": ["/dev/video0"] }
{ "type": "camera", "user": "admin", "password": "secret" }
```

| Field | Description |
|-------|-------------|
| `allowlist` | Restrict access to specific device paths. Omit to allow all cameras. |
| `user` | Username for a registered IP camera. Ignored for local cameras. |
| `password` | Password for a registered IP camera. Ignored for local cameras. |

> **Security:** `user`/`password` set here are stored in **plaintext** in `wendy.json` and deployed as-is. Prefer the interactive `wendy device camera login` command, which keeps credentials out of the app config.

### Display Entitlement

Present to a locally-attached monitor as a Wayland client (GPU-accelerated).

```json
{ "type": "display" }
```

When enabled:
- `/dev/dri` (GPU render nodes); cgroup access is `rw`, no `mknod`
- Membership in the `video` and `render` groups
- The WendyOS compositor's Wayland socket, exposed via `WAYLAND_DISPLAY` / `XDG_RUNTIME_DIR`

On NVIDIA Jetson the GL/EGL userspace is injected from the host through CDI; on Raspberry Pi the app's own mesa works against the vc4 kernel driver.

| Constraint | |
|------------|--|
| At most one `display` per app | enforced at validation |
| Display-enabled image | the Wayland socket is present only on display-enabled WendyOS images |

> **Security:** apps **without** `display` never receive `/dev/dri` — the default GPU/display sandbox is unchanged.

### Video Entitlement

**Deprecated:** Use `camera` instead. Provides access to video capture devices (USB cameras, CSI cameras).

```json
{ "type": "video" }
```

When enabled:
- Mounts `/dev` to expose all video capture devices
- Configures device permissions for video capture
- Enables V4L2 (Video4Linux2) and libcamera interfaces

### Audio Entitlement

Enables access to audio input and output devices.

```json
{ "type": "audio" }
```

When enabled:
- Mounts `/dev/snd` directory into container
- Configures ALSA device permissions
- Enables recording and playback capabilities

### Bluetooth Entitlement

Allows communication with Bluetooth devices.

```json
{ "type": "bluetooth", "mode": "kernel" }
```

| Mode | Description |
|------|-------------|
| `kernel` | Direct kernel-level Bluetooth via HCI sockets. For low-level control and custom protocol implementations. |
| `bluez` | Uses BlueZ daemon's D-Bus API. Recommended for standard Bluetooth profiles (A2DP, HFP, GATT). |

**kernel mode** adds:
- Network administration capabilities (`CAP_NET_ADMIN`, `CAP_NET_RAW`)
- Seccomp filters for Bluetooth socket operations
- Direct HCI socket communication

**bluez mode** provides:
- BlueZ D-Bus interface access
- Interaction with paired devices and Bluetooth profiles

### Display Entitlement

Present to a locally-attached monitor as a Wayland client (GPU-accelerated).

```json
{ "type": "display" }
```

The container receives:
- `/dev/dri` (GPU render nodes); cgroup access is `rw`, no `mknod`.
- Membership in the `video` group, plus the `render` group when the host has one.
- The WendyOS compositor's Wayland socket, exposed via `WAYLAND_DISPLAY` / `XDG_RUNTIME_DIR`.

On NVIDIA Jetson the GL/EGL userspace is injected from the host through the same CDI path as `gpu`; on Raspberry Pi the app's own mesa works against the vc4 kernel driver.

| Constraint | |
|------------|--|
| At most one `display` per app | enforced at validation |
| Display-enabled image | the Wayland socket is present only on display-enabled WendyOS images; on a headless image the entitlement is accepted but nothing renders |

> **Security:** apps **without** `display` never receive `/dev/dri` — the default GPU/display sandbox is unchanged.

### Notifications Entitlement

Sends operator-facing Wendy Notifications through the app's private app
connection.

```json
{ "type": "notifications" }
```

The agent/daemon mounts `/run/wendy/system` read-only and injects
`WENDY_SYSTEM_SOCKET=/run/wendy/system/system.sock`. There is one socket per
app, shared by that app's entitled services and future app-facing API
capabilities. WendyKit's public Swift operation is
`WendyNotification.send(_:)`; apps do not need to use gRPC.

The app supplies one or more user, organization team, or role selectors, plus
title/body, severity, deep link, a caller-chosen `notification_id` UUID v4
resource identity, and optional metadata. After successful creation, every
canonical UUID reuse returns `ALREADY_EXISTS` rather than replaying success; a
local validation or rate-limit rejection leaves the UUID valid for retry.
Selectors have union semantics and are limited to 100 entries before
normalization and deduplication; Cloud resolves at most 10,000
recipients. Trusted local state supplies Cloud `app_id`, stored as
`created_by_app_id`, while provisioned device mTLS supplies device and
organization identity. This entitlement never exposes the administrative
`WENDY_AGENT_SOCKET`.

### Admin Entitlement

Grants the container the wendy-agent's full gRPC over a local unix socket, exposed as `WENDY_AGENT_SOCKET` (`/run/wendy/agent/agent.sock`) — with no authentication.

```json
{ "type": "admin" }
```

An app with `admin` can start, stop, and delete apps and read all device data locally. The socket is bind-mounted only into containers that declare `admin` — that mount is the entire trust boundary — and it is never reachable off-device (a unix socket, not TCP). At most one `admin` per app.

> **Security:** `admin` is a privileged, deliberate grant equivalent to local device control. Grant it only to fully-trusted first-party apps (e.g. the WendyOS shell). Requires an agent build that serves the local socket.

### Build Entitlement

Runs a container image builder (BuildKit) inside the app container.

```json
{ "type": "build" }
```

Grants `CAP_SYS_ADMIN` and un-denies the `unshare` / `clone(CLONE_NEWUSER)` syscalls a nested builder needs (the kernel-module and `kexec` denials are kept). **Privileged-equivalent: a container→host escape surface.** Used so a device can build apps for itself (see the `claude-on-device` example). Grant only to fully-trusted, first-party apps. At most one per app; takes no parameters (`{"type":"build"}`).

## Common Configurations

### Web Server with Camera
```json
{
  "appId": "com.example.video-streamer",
  "platform": "linux",
  "version": "1.0.0",
  "entitlements": [
    { "type": "network", "mode": "host" },
    { "type": "video" }
  ]
}
```

### Machine Learning Inference Server
```json
{
  "appId": "com.example.ml-server",
  "platform": "linux",
  "version": "1.0.0",
  "entitlements": [
    { "type": "network", "mode": "host" },
    { "type": "gpu" }
  ]
}
```

### Computer Vision with GPU
```json
{
  "appId": "com.example.vision-app",
  "platform": "linux",
  "version": "1.0.0",
  "entitlements": [
    { "type": "gpu" },
    { "type": "video" }
  ]
}
```

### Voice Assistant
```json
{
  "appId": "com.example.voice-assistant",
  "platform": "linux",
  "version": "1.0.0",
  "entitlements": [
    { "type": "network", "mode": "host" },
    { "type": "audio" },
    { "type": "bluetooth", "mode": "kernel" }
  ]
}
```

### Minimal (No Hardware Access)
```json
{
  "appId": "com.example.hello-world",
  "platform": "linux",
  "version": "1.0.0",
  "entitlements": []
}
```

## CLI Commands

### Add Entitlements
```bash
wendy project entitlements add network --mode host
wendy project entitlements add network --mode none
wendy project entitlements add gpu
wendy project entitlements add video
wendy project entitlements add audio
wendy project entitlements add notifications
wendy project entitlements add bluetooth --mode kernel
wendy project entitlements add bluetooth --mode bluez
```

### Remove Entitlements
```bash
wendy project entitlements remove network
wendy project entitlements remove gpu
```

### List Entitlements
```bash
wendy project entitlements list
```

## Troubleshooting

| Problem | Solution |
|---------|----------|
| Can't access network | Add `{ "type": "network", "mode": "host" }` |
| GPU not detected | Add `{ "type": "gpu" }` (Jetson devices only) |
| Camera not found | Add `{ "type": "camera" }`, verify camera at `/dev/video0` |
| Audio permission denied | Add `{ "type": "audio" }` |
| Bluetooth operations failing | Add `{ "type": "bluetooth", "mode": "kernel" }` or `"mode": "bluez"` |

## Best Practices

1. **Least privilege**: Only request entitlements your app actually needs
2. **Start minimal**: Begin with empty entitlements, add as needed when encountering access errors
3. **Use host networking for servers**: Any app accepting incoming connections needs network entitlement with `mode: host`
4. **Document entitlements**: Explain in README why each entitlement is required
5. **Watch for port conflicts**: With host mode, app ports are exposed directly on device
