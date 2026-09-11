# App Entitlements

Entitlements grant a containerized app access to host resources — networking, GPU, cameras, serial ports, and more. An app gets **only** what it declares; anything not listed is unavailable. Entitlements apply to WendyOS container targets (`linux`/`wendyos`) and `wendy-lite`; native macOS (`darwin`) apps run non-containerized and don't use them.

Declare them in the `entitlements` array of your `wendy.json`, or add one with `wendy project entitlements add <type>`:

```json
{
    "appId": "com.example.app",
    "platform": "linux",
    "entitlements": [
        { "type": "network", "mode": "host" }
    ]
}
```

> **Complete reference:** for every entitlement type, its options, and security notes, see [wendy.json → Entitlements](../apps/wendy.json.md#entitlements-1). This page is a guide to the common ones and how to choose between similar options.

## Notifications

Use `{ "type": "notifications" }` when an app needs to alert operators through
Wendy Cloud and Companion. WendyKit exposes this as
`WendyNotification.send(_:)`.

| Boundary | Value |
|---|---|
| Read-only mount | `/run/wendy/system` (one private app-facing socket per app) |
| Injected environment | `WENDY_SYSTEM_SOCKET=/run/wendy/system/system.sock` |
| Supplementary group | GID `2000`, so non-root apps can connect without world access |
| Stable host directory | Per-app subdirectory under `/var/lib/wendy/app-system` |
| Cloud deadline | 15 seconds per `Send` call |
| Socket restoration | Recreated from persisted container labels after agent/daemon restart |

The app supplies one or more user, organization team, or role selectors plus
content, severity, deep link, a caller-chosen Notification UUID v4 resource
identity, and optional metadata. After successful creation, any canonical UUID
reuse returns `ALREADY_EXISTS` rather than replaying success. A local validation
or rate-limit rejection does not claim the UUID, so that UUID remains valid for
retry. All selector categories are unioned, normalized, and deduplicated. The
app-facing API accepts at most 100 selector entries before deduplication; Cloud
resolves at most 10,000 recipients. The agent/daemon stamps trusted `app_id`;
Wendy Cloud stores it as `created_by_app_id` and derives device and organization
identity from device mTLS. Apps without the entitlement receive no private app
connection mount, environment variable, or socket group. The full administrative Agent socket
remains separate and requires `admin`.

All entitled services in a multi-service app share the app's stable socket
directory and app identity. Running containers reconnect on their next call
after socket restoration; no redeploy is required. Stopped containers retain
their mount and can reconnect when started again. The directory remains until
the last entitled service container is deleted.

## Wendy Data (episode-write)

Use `{ "type": "episode-write" }` when an app needs to write into the device's
recorded dataset: it submits structured events or predictions to the episode
recorder. The agent gives each app a private socket and derives the trusted app
identity from that socket; the administrative agent socket is never exposed.

The name says write, because that is what it grants, and the grant is stronger
than plain logging: campaign triggers match on application event names and
prediction attributes, so an app holding `episode-write` can start recordings.
It grants no read access to any sensor. Reading is native: an app opens the
agent-fed device node granted by `camera`, and frame identity arrives in-band
with the frames.

| Boundary | Value |
|---|---|
| Read-only mount | `/run/wendy/data` |
| Injected environment | `WENDY_DATA_SOCKET=/run/wendy/data/data.sock` |
| Supplementary group | GID `2000` |
| Maximum record | 64 KiB, versioned length-prefixed JSON |
| Acknowledgements | `buffered`, `recorded`, or `rejected` |
| Socket restoration | Recreated from persisted container labels after agent/daemon restart |

When no Episode is active, accepted records enter the bounded application
pre-roll buffer. They can trigger an armed campaign by event name or model
uncertainty. Running containers reconnect after socket restoration without a
redeploy. Apps without `episode-write` receive neither this mount nor the
environment
variable.

A `prediction` record may carry an optional `inputs` list of
`{source_id, sample_id}` naming the harness samples it was computed from (see
Wendy Sensors). The agent records the references in the Episode so
`(input, outcome)` pairs can be reconstructed offline; a record without them is
still accepted.

## Network

The network entitlement allows the container to access the device's network. If the device is connected to WiFi, Ethernet or otherwise, the container will have access to make TCP and UDP connections to the internet.

A "network" type entitlement can have the following values:

- **network**: A string representing the network type. Can be `host` (default) or `none`.

> Note: NetworkMode `none` does not support remote debugging.

```json
{
    "type": "network",
    "network": "host"
}
```

## Input

The input entitlement allows the container to access Linux input devices such as game controllers, barcode scanners, keyboards, and other devices that appear under `/dev/input/`. This is separate from the USB entitlement — USB covers `/dev/bus/usb` (raw USB access), while input covers the higher-level Linux input subsystem.

```json
{
    "type": "input"
}
```

The container receives:
- A bind mount of `/dev/input/` (including `by-id/` symlinks for stable device identification)
- Membership in the host's `input` group for device permissions
- A cgroup device rule allowing access to input devices (major 13)

### Device discovery

Event device numbers (`/dev/input/event0`, `event1`, etc.) are assigned dynamically and can change across reboots. Use a stable symlink basename under `/dev/input/by-id/` where one exists, or the evdev device's `uniq` identity, to identify devices reliably:

```
/dev/input/by-id/usb-USBKey_Chip_USBKey_Module_202730041341-event-kbd
```

### Bluetooth game controllers

BlueZ on WendyOS owns pairing, trust, and reconnect. Pair and trust the pad on
the device first (both behaviors are enabled by default):

```sh
wendy device bluetooth connect <address>
```

Then declare `{ "type": "input" }` on the service that reads the controller
and select it by `/dev/input/by-id` basename or evdev `uniq`. No controller API,
mapping service, or raw `usb` entitlement is required for standard Linux
gamepad event codes. The same input entitlement also covers a controller used
over wired USB.

### When to use input vs USB

| Entitlement | Access | Use case |
|-------------|--------|----------|
| `input` | `/dev/input/` (Linux input subsystem) | Reading input events — game controllers, barcode scanners, keyboards |
| `usb` | `/dev/bus/usb` (raw USB) | Low-level USB communication — custom protocols, firmware updates, libusb |

Most USB HID devices (scanners, keyboards) should use `input`. You only need `usb` if your app talks raw USB protocols.

## USB

The USB entitlement allows the container to access USB devices.

## Serial / UART

**UART access is the `serial` entitlement.** In Wendy, talking to a device over a UART means opening its serial tty node, and that is exactly what `serial` grants — so if you're looking for "UART," this is the section. It is **USB-serial only** (`ttyACM*`/`ttyUSB*`): on-board UARTs (`ttyAMA*`, `ttyS*`) are intentionally not supported, because `ttyS` shares its kernel major with a board's system-console UART and exposing it would add attack surface for no peripheral benefit. To reach a USB device with a raw protocol instead of a tty, see [USB](#usb); for HID event devices, see [Input](#input).

The serial entitlement grants a container access to a serial tty node so it can `open()` a port such as `/dev/ttyACM0` or `/dev/ttyUSB0`. The motivating case is USB-serial peripherals — for example the LeRobot SO-101 arm, whose Feetech bus servos are driven over a USB-serial adapter via `pyserial`.

```json
{
    "type": "serial",
    "device": "ttyACM0"
}
```

- **device**: A bare tty node name (not a path), matching `^(ttyACM|ttyUSB)[0-9]+$` — e.g. `ttyACM0`, `ttyUSB0`. A device that does not match this pattern is rejected at validation. The entitlement is **USB-only**: on-board UARTs (`ttyAMA*`, `ttyS*`) are not supported — `ttyS` shares its major with a board's system-console UART, so allowing it would add attack surface for no peripheral benefit.

The container receives:
- A bind mount of the named node `/dev/<device>` (e.g. `/dev/ttyACM0`)
- A cgroup device rule scoped to **exactly that device's** `major:minor` with `rw` access (no `mknod`). The node is resolved by `stat()` on the host at deploy time, so the rule grants access to that one device only — not the whole kernel major. (Expected majors: `ttyACM` = 166, `ttyUSB` = 188.)
- Membership in the `dialout` group (GID 20), which owns serial tty nodes on Debian/Ubuntu hosts

> The device must be connected when you deploy: Wendy resolves its `major:minor` at container creation and fails fast with a clear error if `/dev/<device>` is absent.

> **Replug behavior:** the cgroup rule is pinned to the `major:minor` resolved **at deploy time**. The kernel can hand a USB-serial node a different `minor` when you unplug and replug it (e.g. `/dev/ttyACM0` comes back as `/dev/ttyACM1`, or the same name with a new minor). After reconnecting the device, **redeploy the app** (`wendy run`) so Wendy re-resolves the node and rebuilds the rule — until then the container's access points at the old minor.

### When to use serial vs USB

| Entitlement | Access | Use case |
|-------------|--------|----------|
| `serial` | The kernel tty node (`/dev/ttyACM*`, `/dev/ttyUSB*`, …) | Serial ports — USB-serial adapters, microcontrollers, servo buses, GPS modules; anything `pyserial`/termios opens |
| `usb` | `/dev/bus/usb` (raw libusb, major 189) | Low-level USB protocols — custom protocols, firmware updates, libusb |

Use `serial` for serial ports and `usb` for raw USB protocols — they expose different device majors. The SO-101's `/dev/ttyACM0` is reached with `serial`, not `usb`.

> **Security note:** the cgroup rule is scoped to the named device's exact `major:minor`, so — unlike a whole-major grant — it never exposes other devices that share the major (e.g. other `ttyACM*`/`ttyUSB*` adapters on the host). The entitlement is USB-only, so it can never reach an on-board console UART. See *Replug behavior* above for why a reconnect needs a redeploy.

## SPI

The SPI entitlement grants a container access to the host's SPI buses so it can talk to SPI peripherals (displays, ADCs, sensors, radios) through the `spidev` user-space interface at `/dev/spidev<bus>.<chipselect>`.

```json
{
    "type": "spi"
}
```

The entitlement takes no options — it grants access to the SPI subsystem as a whole.

The container receives:
- A bind mount of every `/dev/spidev*.*` node present on the host at deploy time (e.g. `/dev/spidev0.0`, `/dev/spidev0.1`)
- Membership in the `spi` group, when that group exists on the host, for device permissions
- A cgroup device rule allowing the SPI major (153) with `rw` access (no `mknod`)

### Bus and chip-select numbering

`spidev` nodes are named `spidev<bus>.<chipselect>`. The bus is the SPI controller and the chip-select picks which device on that bus you address — so `/dev/spidev0.0` is bus 0, chip-select 0, and `/dev/spidev0.1` is bus 0, chip-select 1. Your app opens the specific node for the peripheral it drives.

### Enabling SPI on the board

The `spidev` nodes only exist if SPI is enabled in the board's device tree — otherwise `/dev/spidev*` is absent and there is nothing to bind. On Raspberry Pi, enable it in `/boot/firmware/config.txt`:

```
dtparam=spi=on
```

then reboot. Confirm the nodes exist before deploying:

```
ls /dev/spidev*
# /dev/spidev0.0  /dev/spidev0.1
```

> The nodes are bind-mounted as they exist **at deploy time**. If you enable SPI (or wire up a device that adds a new `spidev` node) after the app is already running, **redeploy the app** (`wendy run`) so Wendy re-scans `/dev` and mounts the new node.

### Example

A minimal read/write with Python's [`spidev`](https://pypi.org/project/spidev/):

```python
import spidev

spi = spidev.SpiDev()
spi.open(0, 0)              # /dev/spidev0.0 — bus 0, chip-select 0
spi.max_speed_hz = 1_000_000
resp = spi.xfer2([0x01, 0x02, 0x03])
print(resp)
spi.close()
```

> **Security note:** unlike `serial` and `i2c` — which are scoped to a single named node's exact `major:minor` — `spi` is a **whole-major** grant. An app that declares it can open **every** SPI bus on the host, not just one. This is deliberate: the entitlement exposes the SPI subsystem rather than an individual bus, and a host can present many `spidev*.*` nodes whose minors are not known ahead of time. Grant it only to apps you trust with all of the device's SPI buses.

## Audio

The audio entitlement grants a container playback and capture access, both through the host's PipeWire session and through raw ALSA. One entitlement covers speakers and microphones alike.

```json
{
    "type": "audio"
}
```

The entitlement takes no options.

The container receives:
- A bind mount of `/dev/snd` for raw ALSA access
- Membership in the `audio` group (GID 29) for device permissions
- A cgroup device rule allowing the sound major (116) with `rw` access (no `mknod`)
- The host's user-session PipeWire socket, mounted at `/run/pipewire/pipewire-0`, with `PIPEWIRE_RUNTIME_DIR=/run/pipewire` set
- `PULSE_SERVER=unix:/run/pipewire/pulse-native`, when the host session exposes a PulseAudio compatibility socket

### Prefer PipeWire over raw ALSA

Both paths are granted, but they do not reach the same devices. ALSA can only see sound cards. A Bluetooth speaker or microphone has no card behind it — it exists only as a node in the PipeWire graph — so `aplay` and `arecord` cannot reach one at all.

Use a PipeWire-aware client (`pw-play`, `pw-record`) or a PulseAudio one (`paplay`, GStreamer's `pulsesink`), which covers sound cards and Bluetooth endpoints alike. Reach for raw ALSA only when you specifically need a wired card.

### When the host has no audio session

Only the socket belonging to a **user session** is mounted. WendyOS runs the session manager inside the `wendy` user's session, and a PipeWire instance without one has an empty graph — no sinks, no sources, and no PulseAudio socket beside it.

If no user session is running, the container still gets `/dev/snd` and the audio group, but no socket and no environment variables. This is deliberate: handing an app a socket onto an empty graph would give it something that looks like working audio and plays nothing. Check for `PIPEWIRE_RUNTIME_DIR` if your app needs to distinguish the two cases.

## Persist

The persist entitlement allows the container to persist data across restarts. Data is stored on the host filesystem and mounted into the container at the specified path.

```json
{
    "type": "persist",
    "name": "my-volume",
    "path": "/mnt/data"
}
```

- **name**: A unique name for the volume. Volumes with the same name are shared across apps.
- **path**: The path inside the container where the volume is mounted.

### Shared Volumes

Volumes are identified by name only (not by app ID), so multiple apps can share data by using the same volume name. This is useful for sharing caches or data between apps.

### Recommended Shared Volume Names

| Name | Path | Description |
|------|------|-------------|
| `huggingface-cache` | `/app/.cache/huggingface` | Shared cache for Hugging Face models (transformers, datasets, etc.). Avoids re-downloading large ML models for each app. |

Example for a Python ML app:

```json
{
    "type": "persist",
    "name": "huggingface-cache",
    "path": "/app/.cache/huggingface"
}
```

## Build

The build entitlement runs a container image builder (BuildKit) inside the app container.

```json
{
    "type": "build"
}
```

Grants `CAP_SYS_ADMIN` and un-denies the `unshare` / `clone(CLONE_NEWUSER)` syscalls a nested builder needs (the kernel-module and `kexec` denials are kept).

> **Warning: Privileged-equivalent: a container→host escape surface.** Used so a device can build apps for itself (see the `claude-on-device` example). Grant only to fully-trusted, first-party apps. At most one per app.
