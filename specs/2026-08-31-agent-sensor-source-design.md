# Agent Sensor Source — macOS/Linux agents as `sensorlink` sources

**Date:** 2026-08-31
**Status:** Design (approved in brainstorming; not yet planned)
**Builds on:** `specs/2026-08-28-remote-sensor-mounting-design.md` (Plan 1 — the consumer + raw-TCP `sensorlink` for MCUs)
**Scope:** A macOS (and later Linux) WendyOS agent exposes its own camera + microphone as a `sensorlink` sensor source over gRPC, so another WendyOS device (a Jetson) can pair with it and mount those sensors locally.

## 1. Summary

Plan 1 built the consumer side of remote sensor mounting and a raw-TCP `sensorlink` transport aimed at microcontrollers. This design adds the **first real source**: a developer's Mac (or any agent-class device) exposes its built-in webcam and mic, and a Linux consumer pairs with it and sees them as a local `/dev/videoN` and an ALSA capture device.

The value: it exercises the entire consumer path (discovery → same-org mTLS pairing → mount) against a **real** source with real media, before any ESP32 firmware exists — de-risking the contract the MCUs will later target, and shipping a demoable feature now (pair your Jetson with your Mac's camera/mic).

Two transports carry the same `sensorlink` payload messages:
- **gRPC** for agent-class sources (macOS/Linux) — over the agent's existing mTLS port.
- **raw-TCP framing** (Plan 1) for microcontrollers (ESP32).

QUIC was evaluated and rejected: `swift-nio-quic` (0.1.0 and `main`) has an unresolvable dependency graph (`swift-crypto 5.0.0-beta.2` vs `swift-certificates`'s `<5.0.0`) and collides with `WendyAgent`'s existing `swift-certificates` dependency. gRPC reuses the agent's already-running mTLS server, needs no new port, and models "more than sensors" as "more services on the same connection."

## 2. Goals / Non-goals

**Goals**
- A grpc-swift `WendySensorService` in `WendyAgent` exposing camera + mic, reusing the agent's mTLS identity, port, and mDNS.
- Consumer gains a gRPC transport adapter alongside the Plan 1 raw-TCP adapter, behind one `SensorTransport` seam; the supervisor/allocator/mount fan-out are transport-agnostic and unchanged.
- Camera over **H.264** (hardware VideoToolbox encode); mic over **PCM_S16LE** (reuse `AudioController`).
- A Linux `snd-aloop` mic mount (the audio twin of Plan 1's v4l2loopback camera mount).
- **Congestion handling**: source-side latest-wins frame dropping, video-dropped-before-audio, H.264 keyframe-on-recovery. Media degrades by dropping, never by growing latency.
- Capability-based discovery (`caps=sensors`), transport recorded on the pairing.

**Non-goals (v1)**
- QUIC / `swift-nio-quic` (rejected, see §1).
- Adaptive bitrate / resolution (documented follow-up).
- MJPEG (H.264 is the v1 camera codec; the consumer writer still supports MJPEG for the MCU path).
- The Linux **agent-source** role (a Pi exporting its cameras): the gRPC service is designed to be reusable there, but v1 ships only the macOS source.
- Generic (non camera/mic) sensors from an agent source.

## 3. Architecture

```
  macOS WendyAgent (Swift)                         Linux consumer (Go, Plan 1 + this)
  ┌───────────────────────────┐                    ┌─────────────────────────────────┐
  │ WendySensorService (gRPC) │  GetSensorManifest │ grpcTransport ─┐                 │
  │  ├ CameraCapture           │◄──────────────────►│                ├ Supervisor       │
  │  │  AVCaptureSession       │  StreamSensors     │ tcpTransport ──┘  (unchanged)     │
  │  │  → VTCompressionSession │  (stream frames)   │   ├ camera → v4l2loopback         │
  │  │  → H.264 Annex-B        │                    │   └ mic    → snd-aloop (new)      │
  │  └ AudioController (PCM)   │   mTLS (org+asset)  │                                   │
  │ BonjourAdvertiser caps=... │◄──── mDNS ─────────│ pair picker (filters caps=sensors)│
  └───────────────────────────┘                    └─────────────────────────────────┘
```

- Shared: the `sensorlink` protobuf payloads + the consumer supervisor, node allocator, and mount fan-out.
- Differs only in transport envelope: gRPC (agents) vs raw-TCP framing (MCUs).
- The consumer must be **Linux** (macOS has no v4l2loopback/snd-aloop). The Mac is only ever a source.

## 4. The `SensorService` gRPC contract

New v2 service, generated for both Go (consumer client) and Swift (agent server) from one proto. The shared payload messages (`SensorManifest`, `SensorDescriptor`, `SensorFrame`, formats) are lifted from Plan 1's `sensorlink.proto` into a shared proto both transports import.

```proto
service WendySensorService {
  rpc GetSensorManifest(GetSensorManifestRequest) returns (SensorManifest);
  rpc StreamSensors(StreamSensorsRequest) returns (stream SensorFrame);
}
message GetSensorManifestRequest {}
message StreamSensorsRequest { repeated uint32 channel_id = 1; }
// SensorManifest / SensorDescriptor / SensorFrame / VideoFormat / AudioFormat: reused from sensorlinkpb.
```

- `GetSensorManifest` → the source's cameras/mics with formats (H.264 video; PCM_S16LE audio).
- `StreamSensors(channels)` → server-streams `SensorFrame`s for the subscribed channels until the client cancels.
- This maps onto the supervisor's existing "probe manifest → subscribe → stream" flow: the gRPC adapter uses `GetSensorManifest` for the probe and `StreamSensors` for frames.

## 5. macOS source (`WendyAgent`, Swift)

New `WendySensorService` registered on the existing mTLS gRPC server (`WendyAgent.makeMainServer` service list). No new port, no new TLS.

### 5.1 Camera (net-new)
- `CameraCapture`: `AVCaptureSession` + `AVCaptureVideoDataOutput` (default video device, BGRA sample buffers on a dispatch queue).
- Encode via **`VTCompressionSession`** (hardware H.264): real-time, main profile, configured target bitrate + GOP (keyframe interval), with `kVTEncodeFrameOptionKey_ForceKeyFrame` support for recovery.
- **Wire format: H.264 Annex-B byte stream** (start codes), with **SPS/PPS injected in-band before every IDR** (VideoToolbox emits AVCC + SPS/PPS in the format description — convert to Annex-B and prepend parameter sets at each keyframe). The **first frame sent to any consumer is an IDR** so a fresh subscriber decodes immediately.
- Each access unit → `SensorFrame{codec=H264, flags=keyframe on IDR, payload=annexb}`.
- This must be compatible with the consumer's existing H.264 v4l2loopback write path (the repo's live-Go2-H.264 support); the format contract (Annex-B, in-band SPS/PPS, IDR flagged) is the interop point.

### 5.2 Microphone (reuse)
- `getSensorManifest` reports a `MICROPHONE` descriptor (PCM_S16LE, device-native rate/channels).
- `streamSensors` pulls `AudioController.audio(...)`'s existing int16-LE PCM `AsyncThrowingStream`, wrapping each buffer as a `SensorFrame` on the mic channel.

### 5.3 Demand-driven capture
Capture starts on a `StreamSensors` subscription and stops when the stream ends — camera light off, mic idle whenever nothing is paired/consuming.

### 5.4 Permissions
Camera/mic are TCC-gated (the agent already prompts via `WelcomeAndPermissions`). An unauthorized sensor is omitted from `getSensorManifest`, and `streamSensors` returns `PermissionDenied` for that channel (surfaced to the consumer as a clean pairing error, never a hang).

### 5.5 Discovery
Extend `BonjourAdvertiser.encodeTXT` to add `caps=sensors` (comma-list) when the service is registered. Reuses the existing `_wendyos._udp` + `assetid`/`tls` TXT and the mTLS port already advertised.

## 6. Consumer gRPC transport adapter (Go)

Introduce a transport seam so the supervisor is transport-agnostic:
```go
type SensorTransport interface {
    FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error)
    Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, error)
}
```
- `tcpTransport` — wraps Plan 1's raw-TCP `Connect` (MCU path, unchanged behavior).
- `grpcTransport` — a `WendySensorService` client: `FetchManifest`=`GetSensorManifest`; `Stream`=`StreamSensors`.

The `Supervisor`'s `dialerFor` field becomes `transportFor func(SensorPairing) (SensorTransport, error)`; `streamOnce` calls `FetchManifest` (probe) and `Stream` (frames). The node allocator, codec→writer mapping, backoff, ctx-cancel, and manifest-`DeviceAssetId` check are untouched (they operate on `Manifest`/`SensorFrame`).

**Selection & mTLS.** `SensorPairing` gains a `Transport` field (`grpc`|`tcp`), recorded at pair time from the source's advertised `caps`/transport.
- `grpc` → dial the source's **mTLS agent endpoint** (the port already in its mDNS TXT) with `mtls.NewClientTLSConfigExpectingPeer(orgID, assetID)` — the same peer-pinning the mesh dialer uses; the source's `ClientCertAuthorizer` enforces same-org. Reuses `grpcclient` dial machinery.
- `tcp` → Plan 1's per-pairing raw-TCP mTLS dialer.

## 7. Linux `snd-aloop` mic mount (consumer)

The audio analogue of `ipcam.Loopback`.

- **Mechanism:** `snd-aloop` pairs a playback device with a capture device — PCM written to playback is readable on capture. The consumer writes incoming mic PCM to playback; a container reads the capture side as a normal mic.
- **New `audioloop` manager:** `modprobe snd-aloop`, then per subscribed `MICROPHONE` channel allocate a reserved aloop card/subdevice (a per-`(source,channel)` allocator, same discipline as the MCU camera band), open the **playback** side with the manifest format (S16LE, source rate/channels), and the supervisor fan-out writes each PCM `SensorFrame` there. Mic is a second `kind` in the existing fan-out.
- **PCM write path — reuse** the Go agent's existing Linux ALSA plumbing; fallback is a thin `alsa-lib` `snd_pcm_writei` writer. (Exact reuse point confirmed at implementation — the one design seam not fully pinned.)
- **Container access:** the aloop capture device appears under `/dev/snd`, bound in by the existing audio entitlement (plus the `audio` group); if none grants `/dev/snd`, add a small `microphone` entitlement mirroring `camera`. Format mismatches absorbed by ALSA `plughw` on the container side.
- **Demand-gating & format changes:** playback opens only while a container consumes; reopens on a format change.
- **Dependency:** requires **`snd-aloop` in the WendyOS Jetson kernel** — a build/image prerequisite, called out explicitly (the one thing that can block the mic mount with correct code). Parallels Plan 1's `v4l2loopback` requirement.

## 8. Media efficiency & congestion

- **Codec: H.264** (hardware VideoToolbox on the Mac; consumer writer + v4l2loopback already support it). ~5–10× lighter than MJPEG for the same quality — the WiFi-appropriate default. Resolution / fps / target bitrate are configurable in the manifest.
- **Source-side latest-wins dropping (v1, required).** gRPC-over-TCP flow-control-blocks the server send on a slow link; the source must NOT queue unboundedly. It keeps a depth-1 (tiny) queue and drops the oldest when the send can't keep up, so latency stays bounded and there's no "swim through stale frames" on recovery.
- **H.264 recovery:** after a drop, drop remaining frames **to the next encoder keyframe and force an IDR** (with SPS/PPS) so the consumer re-syncs cleanly. The keyframe flag marks IDRs.
- **Video before audio.** Under congestion, starve video first and protect a small audio buffer (audio is ~1.5 Mbps and glitches badly if dropped).
- **Follow-up (not v1):** adaptive bitrate/resolution driven by observed send lag (VideoToolbox supports dynamic bitrate).

## 9. Error handling

- Per-channel `PermissionDenied` (TCC) → clean pairing error, not a hang.
- Source offline / gRPC unavailable → supervisor backoff (reuse Plan 1); `pair --list` shows disconnected.
- Manifest `DeviceAssetId` ≠ pairing's `SourceAssetID` → refuse (Plan 1 defense-in-depth, transport-agnostic).
- Format change (video or audio) → reopen writer / playback.
- Missing `v4l2loopback` or `snd-aloop` module → explicit error, never a silent stall.
- CLI never surfaces raw `rpc error: code = ...` (Plan 1 rule).

## 10. Testing

- **Go consumer (CI, no Swift/hardware):** `grpcTransport` against an **in-process Go `WendySensorService`** stub serving a canned manifest + H.264/PCM frames over a real gRPC server on loopback — exercises adapter → supervisor → allocator → mount (stub writer) against the exact proto contract. `SensorTransport` selection test. Plan 1's `tcpTransport` tests carry over.
- **Swift source (macOS):** `WendySensorService` handler tests with an injected fake capture producer (manifest reflects authorized sensors; `streamSensors` yields + stops on cancel). `AVCaptureSession`/`VTCompressionSession` camera capture is a gated manual check on a real Mac (camera light on, decodable H.264 Annex-B out, mic PCM non-silent).
- **`snd-aloop`:** unit test of the `audioloop` allocator/format logic (stub ALSA writer) + a build-tagged hardware test (needs `snd-aloop` on Linux), the audio twin of Plan 1's v4l2loopback E2E.
- **Acceptance demo (manual, hardware-gated):** enrolled Mac agent running `SensorService` ↔ Jetson. `wendy device pair` on the Jetson discovers the Mac via `caps=sensors`, pairs over mTLS, a Jetson container opens `/dev/videoN` (webcam) + ALSA capture (mic). Requires Mac + Jetson + `v4l2loopback` + `snd-aloop`.

## 11. Suggested plan decomposition

Three stacked plans, each independently testable:
1. **Shared proto + consumer gRPC adapter + `SensorTransport` seam + `transport` field** — testable in Go CI against an in-process Go `WendySensorService` stub. No Swift, no hardware.
2. **macOS Swift source** — `WendySensorService` + `AudioController` mic + `AVCaptureSession`→VideoToolbox H.264 camera + `caps=sensors` mDNS. Tested against plan 1's Go consumer.
3. **Linux `snd-aloop` mic mount + microphone entitlement** — completes the mic path on the consumer.

Congestion handling (§8) lands within plans 1 (consumer drop already exists) and 2 (source-side drop + H.264 recovery).

## 12. Concrete file map (indicative)

**New**
- `Proto/wendy/agent/services/v2/sensor_service.proto` (+ Go `agentpbv2` + Swift gen)
- Shared payload proto (lift `SensorManifest`/`SensorDescriptor`/`SensorFrame`/formats to a proto both transports import)
- `go/internal/agent/mcusource/transport.go` — `SensorTransport`, `tcpTransport`, `grpcTransport`
- `go/internal/agent/audioloop/` — `snd-aloop` manager + allocator + PCM writer
- `swift/WendyAgentCore/.../Services/SensorService.swift`, `.../CameraCapture.swift` (AVCaptureSession + VTCompressionSession → H.264 Annex-B)

**Modified**
- `go/internal/agent/mcusource/supervisor.go` — `dialerFor` → `transportFor`; mic `kind` in the fan-out
- `go/internal/agent/mcusource/pairing_store.go` + the pairing proto — `Transport` field
- `go/internal/cli/commands/device_pair.go` — record transport from advertised `caps`
- `go/internal/agent/oci/entitlements.go` — `microphone`/`/dev/snd` bind if not already present
- discovery mDNS — parse `caps` (supersedes the `sensorlink=true` flag from Plan 1)
- `swift/WendyAgentCore/.../WendyAgent.swift` — register `SensorService`; `BonjourAdvertiser` — `caps=sensors`
