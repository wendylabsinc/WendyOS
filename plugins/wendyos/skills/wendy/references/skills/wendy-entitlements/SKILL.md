---
name: wendy-entitlements
description: Use when creating, reviewing, or debugging `wendy.json` entitlements for Wendy apps, especially device access, networking, persistence, GPU, audio, camera, Bluetooth, USB, I2C, GPIO, SPI, serial, input, or MCP devices.
---

# Wendy Entitlements Workflow

Use this for an application's `wendy.json` capabilities. Check the installed CLI's
`wendy json --help`, validate the app with `wendy json validate`, and inspect the
selected device's hardware with `wendy-device-ops`. End-users do not need a Wendy
source checkout to configure an app.

## Platform scope

Entitlements apply to containerized platforms (`linux`/`wendyos` and `wendy-lite`). Darwin (native macOS) apps run non-containerized and do not use WendyOS container entitlements.

## Start from app needs

Ask what the app actually touches, then choose the smallest entitlement set:

- Server, WebRTC, callbacks, debugger, or LAN-visible API: `network`.
- NVIDIA/Jetson inference or CUDA: `gpu`.
- Present to a locally-attached monitor as a Wayland client: `display`.
- Microphone, speaker, ALSA, PipeWire, PulseAudio compatibility: `audio`.
- V4L2 cameras or USB webcams: `camera`.
- Model cache, database, user uploads, generated files, or persistent app state: `persist`.
- BlueZ Bluetooth access: `bluetooth`.
- Direct Wi-Fi Aware control: `nan`, plus `network: host` for the NDP data path.
- Raw USB devices: `usb`.
- I2C bus devices: `i2c`.
- GPIO chips: `gpio`.
- SPI devices: `spi`.
- Serial ports (USB-serial adapters, servo buses): `serial`.
- HID/input event devices: `input`.
- MCP tool servers: `mcp`.
- Full local device control (app orchestration, reading all device data): `admin`.

Prefer no entitlement when the app does not need the host resource.

## Supported entitlements

Use this table as the starting point, then verify against the selected device's
capabilities and installed CLI validation:

| Type | Required keys | Common optional keys | Runtime effect |
| --- | --- | --- | --- |
| `network` | none | `mode` as `host` or `none` | Defaults to host networking when `mode` is empty; `host` removes the network namespace and mounts host DNS config. |
| `http` | `port` | none | Declares the app's HTTP port for clients and VM forwarding; the app must listen on that port. It does not start a server or establish readiness. |
| `gpu` | none | none | Jetson: adds NVIDIA device nodes, env vars, CDI wiring. Raspberry Pi: exposes `/dev/vcio` for board telemetry. |
| `display` | none | none | Grants `/dev/dri` (GPU render nodes) and the WendyOS compositor's Wayland socket; allows the container to present to a locally-attached monitor as a Wayland client. Requires a display-enabled WendyOS image; on headless images the socket is absent so nothing renders. On Jetson, GPU graphics userspace is injected from the host via CDI. At most one per app. |
| `audio` | none | none | Adds audio group, mounts `/dev/snd`, allows sound devices, and mounts PipeWire/Pulse sockets when present. |
| `camera` | none | `mode`, `allowlist` | Canonical V4L2/camera entitlement; allows major 81, bind-mounts host `/dev` for live camera hotplug, and bind-mounts `/run/udev` read-only for libcamera CSI enumeration. |
| `video` | none | `mode`, `allowlist` | Deprecated compatibility alias for `camera`; prefer `camera` in new configs. |
| `persist` | `name`, `path` | none | Creates/binds `/var/lib/wendy/volumes/<name>` to the container `path`; volume names are shared across apps. Linux/WendyOS mounts are `noexec`. |
| `bluetooth` | none | `mode` | Uses a filtered `xdg-dbus-proxy` socket for BlueZ. Do not assume unrestricted host D-Bus access. |
| `nan` | none | none | Prepares an app-specific NDI and exposes only the `nan0` wpa_supplicant socket as `WENDY_NAN_SOCKET`; bind the client's local Unix datagram socket in `WENDY_NAN_CLIENT_DIR` for replies. Does not grant host networking. App owns its own NAN service/NDP handles, but shares radio-wide cluster state. |
| `usb` | none | none | Mounts `/dev/bus/usb` and allows USB character devices. |
| `i2c` | `device` | none | Binds `/dev/<device>` such as `/dev/i2c-1` and allows I2C devices. |
| `gpio` | none | `pins` | Mounts existing `/dev/gpiochip0` through `/dev/gpiochip7`; `pins` are documentation/validation, access is chip-level. |
| `spi` | none | none | Mounts existing `/dev/spidev*` nodes and adds the host `spi` group when present. |
| `input` | none | none | Adds input group, mounts `/dev/input`, and allows input event devices. |
| `serial` | `device` | none | Binds `/dev/<device>` (e.g. `/dev/ttyACM0`) and adds the `dialout` group. USB-only; on-board UARTs (`ttyAMA*`, `ttyS*`) are not supported. |
| `mcp` | `port` | none | Exposes an MCP server port for agentic tool access. |
| `admin` | none | none | Grants the container the wendy-agent's local control socket (exposed as `WENDY_AGENT_SOCKET`, currently `/run/wendy/agent/agent.sock` — read the env var, don't hard-code). This is the agent's **full gRPC with no authentication** — an app with `admin` can start, stop, and delete apps and read all device data locally. **Grant it only to fully-trusted first-party apps.** At most one per app. Requires an agent build that serves the local socket. |

`network.ports` being accepted by validation does not establish port forwarding.
For host networking, verify the app's listening address and port directly.

## Persistent volumes and executable code

On Linux/WendyOS, a file under a `persist` mount can have mode `0755` and still
fail with `Permission denied` when executed directly. Check file ownership and
permissions as well as the mount policy; `chmod` cannot override `noexec`.

Keep bundled executable workers in the image outside the volume, for example
`/usr/local/bin/worker`, and use `/data` for their persistent state. If startup
copies a bundled worker into `/data` and executes it there, change the launch path
to the image's copy. Preserve existing volume names, data and mount protections
during the repair, then verify the app's output and existing data after redeploy.

An interpreter installed in the image can read a persisted script, for example
`sh /data/worker`, when stored scripts are part of the app's intended design.
That works for scripts; it does not make a compiled executable runnable on a
`noexec` mount.

## Example patterns

Web server:

```json
{
  "appId": "api-server",
  "platform": "linux",
  "entitlements": [
    { "type": "network", "mode": "host" },
    { "type": "http", "port": 8000 }
  ],
  "readiness": {
    "tcpSocket": { "port": 8000 },
    "timeoutSeconds": 30
  }
}
```

Camera + audio app:

```json
{
  "appId": "camera-assistant",
  "platform": "linux",
  "entitlements": [
    { "type": "network", "mode": "host" },
    { "type": "camera" },
    { "type": "audio" }
  ]
}
```

Jetson GPU app with persistent model cache:

```json
{
  "appId": "vision-inference",
  "platform": "linux",
  "entitlements": [
    { "type": "network", "mode": "host" },
    { "type": "gpu" },
    { "type": "camera" },
    { "type": "persist", "name": "vision-models", "path": "/models" }
  ]
}
```

I2C sensor app:

```json
{
  "appId": "sensor-reader",
  "platform": "linux",
  "entitlements": [
    { "type": "i2c", "device": "i2c-1" },
    { "type": "persist", "name": "sensor-data", "path": "/data" }
  ]
}
```

Serial device app (USB-serial servo bus):

```json
{
  "appId": "servo-controller",
  "platform": "linux",
  "entitlements": [
    { "type": "network", "mode": "host" },
    { "type": "serial", "device": "ttyACM0" }
  ]
}
```

## Review checklist

- `appId` is present.
- Entitlement names are current; prefer `camera` over `video`.
- `persist` entries include both `name` and an absolute container `path`.
- Linux/WendyOS startup failures under `persist` paths are checked for `noexec` before changing file modes.
- `i2c` entries include a device name without a leading `/dev/`.
- `serial` entries include a bare USB tty node name (e.g. `ttyACM0`, `ttyUSB0`) matching `^(ttyACM|ttyUSB)[0-9]+$`; on-board UARTs (`ttyAMA*`, `ttyS*`) are not supported.
- Device-heavy apps include only the devices they actually use.
- GPU issues are checked across app image, device type, CDI, agent logs, and app fallback behavior before blaming the entitlement alone.
- JSON examples remain valid JSON; do not put comments inside `wendy.json`.
