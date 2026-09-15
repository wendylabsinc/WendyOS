# Remote Sensor Mounting — `wendy device pair`

**Date:** 2026-08-28
**Status:** Design (approved in brainstorming; not yet planned)
**Scope:** Full three-subsystem spec — CLI, consumer agent (Jetson), sensor-source server (ESP32 firmware).

## 1. Summary

Today WendyOS mounts IP cameras and ROS2 cameras into containers via a
v4l2loopback bridge: a *frame source* pumps frames into a `/dev/videoN`
loopback node, and the container sees a normal camera. This design
generalizes that pattern to a **remote sensor source** — a separate
Wendy device on the same LAN that serves cameras, microphones, and
generic sensors, which a consumer device mounts locally as if the
hardware were attached to it.

The user experience is `wendy device pair`: pick a sensor-source device
(same picker used everywhere), and its real-time sensor data flows to
your current device and appears as local mounts. The binding is durable
— it auto-reconnects and re-mounts across reboots, like Bluetooth
pairing.

For v1 the only sensor source that ships a server is an **ESP32 running
Wendy Lite**, and the consumer is a **WendyOS agent device** (e.g. an
NVIDIA Jetson). The design is deliberately not ESP32-specific: the
picker filters on an advertised *capability*, and the wire protocol is
symmetric, so a Raspberry-Pi-with-cameras exporting the same protocol
drops in later with no CLI or protocol change.

## 2. Goals / Non-goals

**Goals**
- `wendy device pair` / `unpair` / `pair --list` to bind a sensor
  source to the current device.
- Durable, supervised, demand-gated streaming of camera / microphone /
  generic sensor data from source to consumer over the LAN.
- Consumer mounts each sensor as a local, unmodified-app-friendly
  surface: camera → `/dev/videoN`, mic → ALSA capture, generic sensor →
  a framed unix socket.
- Reuse existing machinery: the v4l2loopback bridge and frame writer,
  the mesh mTLS/PKI and same-org identity check, the `ipcam.Loopback`
  reconcile/backoff/demand supervisor, the TUI picker.
- Capability-based source selection so non-MCU sources are a future
  drop-in.

**Non-goals (v1)**
- BLE transport. BLE in this stack cannot carry media (GATT notify is
  capped at 256 B with client-side stubs); it is telemetry-only and out
  of scope. Transport is WiFi/LAN.
- Provisioning. Both devices are assumed already enrolled and on WiFi,
  discoverable over LAN/cloud.
- Cloud-relayed sensor data. LAN-direct only for v1 (cloud-broker
  fallback is a documented future extension — see §11).
- Agent-side sensorlink *server* (a Pi exporting its cameras). The
  protocol supports it; only the ESP32 firmware server is built in v1.
- dimensionalOS/ROS2 topic publication of generic sensors (follow-up —
  see §6.3, §11).

## 3. Architecture overview

Three subsystems, one contract:

```
  ┌────────────────────────┐         sensorlink (mTLS/TCP, LAN)        ┌──────────────────────────┐
  │  Sensor source (v1:     │  ── manifest ──►                          │  Consumer agent (Jetson) │
  │  ESP32 / Wendy Lite)    │  ◄── Subscribe ──                         │                          │
  │                         │  ── SensorFrame(channel_id) ──►           │  mcusource.Supervisor    │
  │  capture_source iface   │                                           │   ├─ camera → v4l2loopback│
  │  + sensorlink server    │                                           │   ├─ mic    → snd-aloop   │
  └────────────────────────┘                                           │   └─ sensor → unix socket │
                                                                        └──────────────────────────┘
        CLI: `wendy device pair`  ── AddSensorPairing(esp32_asset_id) ──►  consumer agent (persists)
```

- **CLI** writes the binding to the consumer agent and does nothing with
  the data path itself.
- **Consumer agent** owns discovery, dialing, mounting, and the
  reconcile/backoff/demand loop. All new consumer code lives behind a
  new `mcusource` package that reuses the existing loopback and frame
  writer.
- **Sensor source** is a passive server: advertise capability, accept a
  same-org mTLS peer, send a manifest, honor `Subscribe`, stream frames.

### Who initiates

`wendy device pair` runs on the user's laptop. The **consumer is the
current/default device** (existing `resolveTarget`); the **sensor source
is what you pick**. The CLI tells the consumer agent to consume from
asset *X*; the consumer does all connecting. The source never needs to
know about the consumer in advance.

## 4. Identity, authz, pairing model

### Same-org via existing PKI

Both devices carry org asset certs. Identity is
`certs.WendyIdentity{OrgID, EntityType:"asset", EntityID}` extracted by
`internal/shared/certs/orgident.go` from the SAN URI
`urn:wendy:org:<org>:asset:<id>`.

- The consumer dials the source over **mTLS expecting the source's asset
  identity** — the same `NewClientTLSConfigExpectingPeer` pattern the
  mesh dialer (`internal/agent/services/mesh_dialer.go`) uses.
- The source accepts only a peer whose verified cert carries the **same
  `OrgID`**. "If in the same org, both will pair" is a one-line
  org-equality check on verified identities — no pairing secret, no new
  trust machinery.
- The CLI pre-checks org equality (CLI org
  `auth.Certificates[0].OrganizationID` vs the selected asset's org) for
  a clean early error; the consumer re-enforces at dial time.

### Binding state

Consumer-side, persisted to the agent state dir, keyed by source asset
ID:

```
SensorPairing {
  source_asset_id  int32
  org_id           int32
  name             string        // friendly name for display
  sensor_allowlist []string      // optional; empty = all sensors
  created_at       time.Time
}
```

No cloud dependency; survives reboot. `unpair` removes the record and
tears down mounts.

### Lifecycle

Durable and supervised. On boot the agent loads pairings and the
`mcusource.Supervisor` reconciles each: discover source on LAN → dial →
read manifest → mount each sensor → pump frames, with exponential
backoff and idle-grace refcounting mirroring
`internal/agent/ipcam/loopback.go`. Frames flow only while a container
consumes them (reusing the `SetContainerConsumers` demand model), so a
paired-but-unused source captures nothing.

## 5. Wire protocol (`sensorlink`)

One mTLS TCP session over the LAN. Framing: **4-byte big-endian
length-prefixed protobuf** (4 bytes, not the L2CAP path's 2, because a
camera keyframe exceeds 64 KB). New source of truth
`Proto/wendy/lite/sensorlink.proto` → `sensorlinkpb`, generated for both
the Go agent and the ESP32.

Exchange:

1. **Auth** is the TLS handshake (same-org mTLS). No app-level auth.
2. Source → **`SensorManifest { int32 device_asset_id; repeated SensorDescriptor sensors }`**.
   ```
   SensorDescriptor {
     uint32 channel_id
     Kind   kind          // CAMERA | MICROPHONE | SENSOR
     string name
     oneof format {
       VideoFormat  video   // codec(MJPEG|H264), width, height, fps
       AudioFormat  audio   // codec(PCM_S16LE|OPUS), sample_rate, channels
       SensorFormat sensor  // schema (type id, e.g. "wendy.imu.v1"), rate_hz, sample_bytes
     }
   }
   ```
3. Consumer → **`Subscribe { repeated uint32 channel_id }`** — only
   listed channels activate. Demand-gating on the wire; the source
   captures nothing unwatched.
4. Source → **`SensorFrame { uint32 channel_id; uint32 seq; uint64 ts_us; uint32 flags; bytes payload }`**
   (`flags` bit0 = keyframe), streamed.
5. Periodic `Ping`/idle timeout for liveness.

The `channel_id` multiplex mirrors the existing `datagram_relay`
`flow_id` and `MeshDial` framing — nothing exotic for firmware.

The protocol carries no notion of the source's hardware class; it is the
same whether served by ESP32 firmware or (future) a WendyOS agent.

## 6. Consumer mount fan-out

New package `internal/agent/mcusource`: a `Supervisor` (reconcile loop)
plus a small `sensorlink` client. Per channel, mapped by `kind`:

### 6.1 Camera → v4l2loopback (pure reuse)

`ipcam.Loopback.EnsureNode(id, label)` in a **new reserved node band**
(see §8), then frames go straight through the existing
`internal/agent/ros2camera/writer_linux.go` frame writer
(`VIDIOC_S_FMT` + `write()`, already handles MJPEG/H264 and reconfigures
on dimension/codec change). The writer is source-agnostic. Container
sees `/dev/videoN`; no container-side change (major-81 + whole-`/dev`
bind already cover it, per `internal/agent/oci/entitlements.go`
`applyCamera`).

### 6.2 Microphone → `snd-aloop`

New small manager parallel to `Loopback`: ensure an ALSA loopback card
(`modprobe snd-aloop`), open the playback side, and write PCM
(pass-through, or decode Opus first). Container sees a normal capture
device. New but isolated code.

### 6.3 Generic sensor → framed unix socket

The agent serves framed samples on a unix socket at
`/run/wendy/sensors/<source>/<name>`, bind-mounted read-only into the
container. Each sample is length-prefixed and carries the `schema` from
the manifest. Chosen for v1 over bus publication to avoid coupling to
the messaging bus; a **dimensionalOS/ROS2 topic bridge is the structured
follow-up** (aligns with the prefer-dimensionalOS default).

## 7. CLI — `wendy device pair`

New `internal/cli/commands/device_pair.go`; `newDevicePairCmd()`
registered in `newDeviceCmd` (`internal/cli/commands/device.go`) under
group `manage`. Subcommands: `pair`, `pair --list`, `unpair`.

- **Consumer target** = existing `resolveTarget` (default/`--device`).
- **Source picker** = `tui.NewPicker()` used directly (not `pickDevice`,
  which deliberately drops ESP32/Lite rows). Fed only rows whose backing
  device **advertises the `sensorlink` capability**.
- **Capability signal** (the one discovery gap to close): sources
  advertise `sensorlink` via an mDNS TXT key and an `is_sensor_source`
  (capability list) field on the cloud asset, so the picker filters on
  real advertised data — **not** an inferred "is this a
  microcontroller" heuristic. v1's scope limit ("only ESP32 ships a
  server") is a property of what advertises the capability, never a
  board-class check in code.
- **Same-org** pre-checked in the CLI; re-enforced by the consumer at
  dial time.
- On select → new consumer agent RPC
  `AddSensorPairing { source_asset_id, name, allowlist? }`; the agent
  persists and kicks the supervisor. `pair --list` → `ListSensorPairings`
  (shows connection state); `unpair` → `RemoveSensorPairing`.
- Flags: `--sensors cam,mic` (allowlist), `--name`.

## 8. Node band allocation

Existing v4l2loopback bands (`internal/agent/ipcam/registry.go`): ROS2
128–199, IP 200–255. MCU/remote cameras need their own band. Allocate a
new band **above 255** (modern kernels allow video minors >255;
`v4l2loopback` node number is an int). Exact range fixed at
implementation; the band must not overlap the existing two, and
`sweepAutoCreatedNodes` must be updated so it does not reap the new band.

## 9. Sensor source server (ESP32 / Wendy Lite, v1)

New component `wendy-lite/components/wendy_sensorlink` (ESP-IDF, C):

- A **`capture_source` interface** — a manifest descriptor plus a frame
  callback. Implementations:
  - **Camera** (v1 first): `esp32-camera` driver → JPEG directly (cheap
    on-chip encode).
  - **Microphone**: I2S → PCM (optionally Opus).
  - **Generic**: existing `wendy_hal` I2C/SPI/ADC → raw payload + schema.
- A **TCP + mTLS server** (mbedTLS over the WiFi socket) on a fixed
  port, advertised via existing mDNS with the `sensorlink` TXT key,
  speaking the protocol in §5. Reuses the device's existing **asset
  cert** (same identity `wendy_com` uses).
- **Enabled sensors are firmware config**, not autodetected — a
  per-device knob declaring wired peripherals/pins (hardware wiring
  can't be guessed). This is the deliberate calibration knob.

This component (with real capture) is the largest single piece and the
only part with a hardware-verification gate.

## 10. Container surface / entitlements

- **Camera** → existing `camera` entitlement, unchanged (already exposes
  any `/dev/videoN`).
- **Microphone** → the existing audio/`snd` bind if one exists; add a
  `microphone` entitlement only if nothing already binds `/dev/snd`.
- **Generic sensor** → new small `sensors` entitlement that read-only
  bind-mounts `/run/wendy/sensors/` into the container.
- Apps declare hardware classes in `wendy.json` exactly as cameras do
  today; the pairing itself is agent-level, not per-app.

## 11. Error handling

- **Source offline / discovery miss** → supervisor backoff (reuse
  `ipcam` backoff); pairing persists; `pair --list` shows the
  disconnected state.
- **Org mismatch** → refuse at dial (reuse identity refusal); the CLI
  surfaces a clean message and **never emits raw `rpc error: code =`**
  (agent-down-vs-unauthorized rule).
- **Format change / sensor disappears** → the frame writer already
  reconfigures on dimension/codec change; a vanished channel's mount is
  dropped.
- **Partial failure** → per-channel isolation: camera up while mic fails
  does not tear down the pairing.
- **Backpressure** → drop frames rather than block the source; the
  `Subscribe` demand-gate already bounds what is captured.

## 12. Testing

- **Go unit** — protocol codec (manifest/frame round-trip); supervisor
  reconcile against an **in-process fake source** (loopback TCP serving
  a canned manifest + frames), stubbing `Loopback`/writer the way the
  `ros2camera` tests stub `cameraWriter`; a same-org authz test.
- **CLI** — picker-filter test (only `sensorlink`-capable rows);
  pairing-RPC round-trip against a stub agent.
- **End-to-end** — a Go **`sensorlink` simulator** that serves the
  protocol doubles as a mock source: it streams canned MJPEG into a real
  consumer agent, and a container reads `/dev/videoN`. This de-risks the
  firmware and gives a runnable E2E without hardware.
- **Hardware gate** — real ESP32 camera/mic capture is flagged
  hardware-unverified until run on-device.

## 13. Future extensions (out of v1 scope)

- **Agent-side sensorlink server** — a WendyOS agent exports its local
  `/dev/video*` / sensors over the same protocol, so a
  Raspberry-Pi-with-cameras is a drop-in source. The consumer side needs
  no change.
- **Cloud-broker fallback** — route `sensorlink` over the existing
  tunnel broker when the source isn't LAN-reachable.
- **dimensionalOS/ROS2 topic bridge** for generic sensors instead of (or
  alongside) the unix socket.

## 14. Concrete file map

**New**
- `Proto/wendy/lite/sensorlink.proto` (+ generated `sensorlinkpb`)
- `internal/agent/mcusource/` — `supervisor.go`, `client.go`,
  `pairing_store.go`, `sndaloop_linux.go`, `sensorsocket.go`
- `internal/cli/commands/device_pair.go`
- `wendy-lite/components/wendy_sensorlink/` (ESP-IDF C)
- New consumer agent RPCs: `AddSensorPairing`, `RemoveSensorPairing`,
  `ListSensorPairings`

**Reused / touched**
- `internal/agent/ipcam/loopback.go`, `registry.go` (new band; band
  sweep guard)
- `internal/agent/ros2camera/writer_linux.go` (camera frame writer —
  reused as-is)
- `internal/agent/oci/entitlements.go` (new `sensors` bind; mic bind if
  needed)
- `internal/cli/commands/device.go` (register subcommand)
- `internal/cli/tui/picker.go` (reused directly)
- `internal/shared/certs/orgident.go`,
  `internal/agent/services/mesh_dialer.go` (mTLS expect-peer pattern)
- discovery: add `sensorlink` capability to mDNS TXT + cloud asset
  capabilities
