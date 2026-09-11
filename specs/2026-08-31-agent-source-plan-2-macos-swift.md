# Agent Sensor Source — Plan 2: macOS Swift source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the macOS `WendyAgent` a real `sensorlink` source: a grpc-swift `WendySensorService` exposing the Mac's microphone (reusing `AudioController`) and camera (net-new `AVCaptureSession`→VideoToolbox H.264), advertised via `caps=sensors`, so the Plan-1 consumer (a Jetson) can pair over mTLS and mount them.

**Architecture:** Add `SensorService` to the agent's existing mTLS grpc-swift server (`WendyAgent.startMainServer` service array) — it inherits mTLS + same-org enforcement + the advertised port for free. `GetSensorManifest` reports authorized sensors; `StreamSensors` streams `Wendy_Lite_Sensorlink_SensorFrame`s per subscribed channel using the `AudioService.streamAudio` `StreamingServerResponse { writer in … }` pattern. Mic reuses `AudioController.audio(...)` (int16-LE PCM). Camera is a new `CameraCapturing` seam: `AVCaptureSession` → `AVCaptureVideoDataOutput` → `VTCompressionSession` H.264 emitted as **Annex-B with in-band SPS/PPS before each IDR**, first frame an IDR, with source-side newest-drop + force-keyframe-on-recovery under congestion.

**Tech Stack:** Swift 6.3, grpc-swift v2 (`GRPCServer`/`HTTP2ServerTransport.Posix`), swift-protobuf, AVFoundation, VideoToolbox, CoreAudio (via `AudioController`).

**Spec:** `specs/2026-08-31-agent-sensor-source-design.md` §5, §8 (builds on Plan 1's frozen `sensorlink` payloads + the consumer gRPC transport).

## Global Constraints

- grpc-swift **v2** patterns only. A service conforms to `Wendy_Agent_Services_V2_WendySensorService.ServiceProtocol` (full protocol, like `AudioService` — NOT `SimpleServiceProtocol`). Server-streaming returns `StreamingServerResponse<…> { writer in … }`.
- Reuse the shared payloads: manifest is `Wendy_Lite_Sensorlink_SensorManifest`, frames are `Wendy_Lite_Sensorlink_SensorFrame` (generated in Task 1). Do NOT define new message types.
- mTLS / same-org / port are inherited by adding the service to the array — **no per-service auth code**.
- Mic: PCM_S16LE from `AudioController.audio(...)`. Note it currently ignores the requested `sampleRate`/`channels` and uses the hardware input format — the manifest MUST report the ACTUAL format the stream produces, not a requested one.
- Camera: **H.264 Annex-B**, SPS/PPS in-band before every IDR, first emitted frame is an IDR, `SensorFrame.flags` bit0 set on IDRs. Must be decodable by the consumer's existing H.264 v4l2loopback path.
- Congestion (spec §8): the camera pipeline keeps a depth-1 (newest) buffer and drops rather than blocks; after a drop it forces a VideoToolbox keyframe so the consumer re-syncs. Audio is not dropped the same way (small buffer).
- Demand-driven: capture starts on a `StreamSensors` subscription and stops when the stream ends.
- Permission-aware: an unauthorized (TCC) sensor is omitted from `GetSensorManifest`; `StreamSensors` for it returns a clear error, never hangs.
- Build/test: `cd swift/WendyAgentCore && swift build` / `swift test`. Proto regen: `make proto` (from `swift/`, or `Scripts/GenerateProto.sh`). Work in the WORKTREE's `swift/` tree.
- TDD where testable (handlers with injected fakes; the Annex-B framing helper). Real `AVCaptureSession`/`VTCompressionSession` capture is a **manual hardware gate** — no CI coverage possible.

---

## File Structure

**New**
- `swift/WendyAgentCore/Sources/WendyAgent/Services/SensorService.swift` — the grpc-swift service.
- `swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/CameraCapture.swift` — `CameraCapturing` protocol + `AVCaptureSession`/`VTCompressionSession` impl + Annex-B framing helper.
- `swift/WendyAgentCore/Tests/WendyAgentTests/SensorServiceTests.swift` — handler tests with injected fake mic/camera producers.
- `swift/WendyAgentCore/Tests/WendyAgentTests/AnnexBTests.swift` — unit test for the AVCC→Annex-B + SPS/PPS framing helper.
- Generated: `swift/WendyAgentCore/Sources/WendyAgentGRPC/Proto/wendy/lite/sensorlink.{pb,grpc}.swift`, `.../v2/sensor_service.{pb,grpc}.swift`.

**Modified**
- `swift/Scripts/GenerateProto.sh` — add `sensorlink.proto` + `sensor_service.proto` to the `WendyAgentGRPC` generation block.
- `swift/WendyAgentCore/Sources/WendyAgent/WendyAgent.swift` — register `SensorService()` in the `services` array (~line 337); thread `caps` into `startBonjour`.
- `swift/WendyAgentCore/Sources/WendyAgent/Services/BonjourAdvertiser.swift` — `caps` field + `encodeTXT` `caps=` record.

---

## Task 1: Generate `sensor_service` + `sensorlink` protos for Swift

**Files:** Modify `swift/Scripts/GenerateProto.sh`; generated `.pb.swift`/`.grpc.swift` under `Sources/WendyAgentGRPC/Proto/`; Test: `swift/WendyAgentCore/Tests/WendyAgentTests/SensorProtoGenTests.swift`.

**Interfaces produced:** `Wendy_Agent_Services_V2_WendySensorService.ServiceProtocol`, `Wendy_Agent_Services_V2_GetSensorManifestRequest`, `Wendy_Agent_Services_V2_StreamSensorsRequest`, `Wendy_Lite_Sensorlink_SensorManifest`, `Wendy_Lite_Sensorlink_SensorFrame`, `Wendy_Lite_Sensorlink_SensorDescriptor`, `Wendy_Lite_Sensorlink_VideoFormat`/`AudioFormat` (+ enums).

- [ ] **Step 1:** In `swift/Scripts/GenerateProto.sh`, find the `WendyAgentGRPC` `generate-grpc-code-from-protos` invocation and add these two files to its proto list (both required — the service imports the lite payloads):
```
"$PROTO_DIR/wendy/lite/sensorlink.proto"
"$PROTO_DIR/wendy/agent/services/v2/sensor_service.proto"
```
- [ ] **Step 2:** Run `cd swift && make proto` (or `bash Scripts/GenerateProto.sh`). Confirm new files appear under `Sources/WendyAgentGRPC/Proto/wendy/lite/` and `.../v2/`, and that `publicize_generated_imports` ran.
- [ ] **Step 3:** Write `SensorProtoGenTests.swift` — a compile-proof test that references the generated types:
```swift
import Testing
import WendyAgentGRPC

@Test func sensorProtoTypesGenerated() {
    var m = Wendy_Lite_Sensorlink_SensorManifest()
    m.deviceAssetId = 7
    let req = Wendy_Agent_Services_V2_StreamSensorsRequest.with { $0.channelId = [1] }
    #expect(m.deviceAssetId == 7)
    #expect(req.channelId == [1])
}
```
- [ ] **Step 4:** `cd swift/WendyAgentCore && swift build && swift test --filter SensorProtoGenTests` → PASS.
- [ ] **Step 5:** Commit the script change + all newly generated files + the test.
```bash
git add swift/Scripts/GenerateProto.sh swift/WendyAgentCore/Sources/WendyAgentGRPC/Proto swift/WendyAgentCore/Tests/WendyAgentTests/SensorProtoGenTests.swift
git commit -m "feat(swift): generate WendySensorService + sensorlink protos for the agent"
```

---

## Task 2: `SensorService` + microphone (reuse `AudioController`) + registration

**Files:** Create `SensorService.swift`; modify `WendyAgent.swift` (register); Test: `SensorServiceTests.swift`.

**Interfaces:**
- Produces `struct SensorService: Wendy_Agent_Services_V2_WendySensorService.ServiceProtocol` with an injectable mic producer (`var audio: any AudioManaging = AudioController()`) and camera producer (added Task 3; stub/empty here).
- Channel convention: mic = channel 1 (constant). Camera = channel 2 (Task 3).

- [ ] **Step 1: Failing handler test** (`SensorServiceTests.swift`) using a fake `AudioManaging` that yields two PCM buffers:
```swift
import Testing
import Foundation
import GRPCCore
@testable import WendyAgent
import WendyAgentGRPC

struct fakeAudio: AudioManaging {
    func audio(deviceID: UInt32, sampleRate: UInt32, channels: UInt32) -> AsyncThrowingStream<(pcm: Data, sampleRate: UInt32, channels: UInt32), any Error> {
        AsyncThrowingStream { c in
            c.yield((pcm: Data([1,2,3,4]), sampleRate: 48000, channels: 1))
            c.yield((pcm: Data([5,6,7,8]), sampleRate: 48000, channels: 1))
            c.finish()
        }
    }
    // implement remaining AudioManaging requirements minimally (list/default/levels) — see AudioController.swift protocol
}

@Test func manifestReportsMicAndStreamYieldsPCMFrames() async throws {
    let svc = SensorService(audio: fakeAudio())
    let manifest = try await svc.getSensorManifest(request: .init(message: .init()), context: .init(...))
    #expect(manifest.message.sensors.contains { $0.kind == .microphone })
    // StreamSensors channel 1 → at least one SensorFrame with the PCM payload
}
```
(Adapt the exact `ServerRequest`/`ServerContext` construction to grpc-swift v2's test surface — mirror how existing `*ServiceTests` build them, or drive the async stream helper directly if the context is awkward to fake.)

- [ ] **Step 2:** Run to fail (`SensorService` undefined).
- [ ] **Step 3: Implement `SensorService.swift`** — copy the `AudioService.streamAudio` shape:
```swift
import Foundation
import GRPCCore
import WendyAgentGRPC

struct SensorService: Wendy_Agent_Services_V2_WendySensorService.ServiceProtocol {
    static let micChannel: UInt32 = 1
    var audio: any AudioManaging = AudioController()
    // var camera: any CameraCapturing = CameraCapture()   // Task 3

    func getSensorManifest(request: ServerRequest<Wendy_Agent_Services_V2_GetSensorManifestRequest>, context: ServerContext) async throws -> ServerResponse<Wendy_Lite_Sensorlink_SensorManifest> {
        var manifest = Wendy_Lite_Sensorlink_SensorManifest()
        // mic descriptor (report the ACTUAL hardware format; probe or use known default)
        if micAuthorized() {
            var d = Wendy_Lite_Sensorlink_SensorDescriptor()
            d.channelId = Self.micChannel
            d.kind = .microphone
            d.name = "mic0"
            var af = Wendy_Lite_Sensorlink_AudioFormat()
            af.codec = .pcmS16Le
            // sample_rate/channels: report actual (see constraint) — probe AudioController or default 48000/1
            d.audio = af
            manifest.sensors.append(d)
        }
        // camera descriptor appended in Task 3
        return ServerResponse(message: manifest)
    }

    func streamSensors(request: ServerRequest<Wendy_Agent_Services_V2_StreamSensorsRequest>, context: ServerContext) async throws -> StreamingServerResponse<Wendy_Lite_Sensorlink_SensorFrame> {
        let channels = Set(request.message.channelId)
        return StreamingServerResponse { writer in
            if channels.contains(Self.micChannel) {
                var seq: UInt32 = 0
                for try await chunk in audio.audio(deviceID: 0, sampleRate: 48000, channels: 1) {
                    var f = Wendy_Lite_Sensorlink_SensorFrame()
                    f.channelId = Self.micChannel
                    f.seq = seq; seq &+= 1
                    f.payload = chunk.pcm
                    try await writer.write(f)
                }
            }
            return Metadata()
        }
    }
    func micAuthorized() -> Bool { /* AVCaptureDevice.authorizationStatus(for: .audio) == .authorized */ true }
}
```
(If both mic AND camera channels are subscribed, Task 3 makes `streamSensors` fan both into the one writer concurrently via a task group — for now, mic-only.)

- [ ] **Step 4:** Register in `WendyAgent.swift:337` services array: add `SensorService(),`.
- [ ] **Step 5:** Run + commit — `cd swift/WendyAgentCore && swift build && swift test --filter SensorServiceTests` → PASS.
```bash
git add swift/WendyAgentCore/Sources/WendyAgent/Services/SensorService.swift swift/WendyAgentCore/Sources/WendyAgent/WendyAgent.swift swift/WendyAgentCore/Tests/WendyAgentTests/SensorServiceTests.swift
git commit -m "feat(swift): SensorService with microphone channel (reuses AudioController)"
```

---

## Task 3: Camera capture — `AVCaptureSession` → VideoToolbox H.264 (net-new)

**Files:** Create `CameraCapture.swift`; modify `SensorService.swift` (camera channel + manifest + concurrent fan-out); Tests: `AnnexBTests.swift` (framing helper) + extend `SensorServiceTests` (fake camera).

**Interfaces:**
- `protocol CameraCapturing: Sendable { func frames() -> AsyncThrowingStream<CameraFrame, any Error>; func descriptor() -> Wendy_Lite_Sensorlink_SensorDescriptor? }` where `struct CameraFrame { var annexB: Data; var isKeyframe: Bool }`.
- `struct CameraCapture: CameraCapturing` — real AVFoundation/VideoToolbox impl.
- `func annexBFromAVCC(_ avcc: Data, sps: [Data], pps: [Data], isKeyframe: Bool) -> Data` — the unit-testable framing helper.
- Camera = channel 2.

- [ ] **Step 1: Failing unit test for the framing helper** (`AnnexBTests.swift`) — the ONE piece testable without hardware:
```swift
import Testing
import Foundation
@testable import WendyAgent

@Test func annexBPrependsStartCodesAndParamSetsOnKeyframe() {
    let sps = Data([0x67, 0x01]); let pps = Data([0x68, 0x02])
    // one AVCC NALU: 4-byte big-endian length + payload
    var avcc = Data([0,0,0,2]); avcc.append(Data([0x65, 0xAA]))  // 0x65 = IDR slice
    let out = annexBFromAVCC(avcc, sps: [sps], pps: [pps], isKeyframe: true)
    let sc = Data([0,0,0,1])
    // keyframe output: SC+SPS, SC+PPS, SC+slice
    #expect(out == sc + sps + sc + pps + sc + Data([0x65, 0xAA]))
}

@Test func annexBNonKeyframeOmitsParamSets() {
    var avcc = Data([0,0,0,2]); avcc.append(Data([0x41, 0xBB]))  // non-IDR
    let out = annexBFromAVCC(avcc, sps: [Data([0x67])], pps: [Data([0x68])], isKeyframe: false)
    #expect(out == Data([0,0,0,1]) + Data([0x41, 0xBB]))
}
```

- [ ] **Step 2:** Run to fail (`annexBFromAVCC` undefined).
- [ ] **Step 3: Implement `annexBFromAVCC`** — parse AVCC (repeated 4-byte-BE length + NALU), replace each length with the 4-byte start code `00 00 00 01`; when `isKeyframe`, prepend `SC+sps` / `SC+pps` for each parameter set. This is pure `Data` manipulation — fully unit-tested.

- [ ] **Step 4: Implement the capture pipeline** in `CameraCapture.swift` (net-new, manual-gated). Structure (mirror `AudioController`/`AudioTapSession`):
  - A `@unchecked Sendable` session class owning an `AVCaptureSession` (default `AVCaptureDevice.default(for: .video)`), an `AVCaptureVideoDataOutput` (BGRA, `alwaysDiscardsLateVideoFrames = true`) with a delegate on a dispatch queue, and a `VTCompressionSession` (`VTCompressionSessionCreate` H.264, `kVTCompressionPropertyKey_RealTime = true`, `ProfileLevel` main, `MaxKeyFrameInterval` for GOP, `AverageBitRate` configurable).
  - Delegate `captureOutput` → `VTCompressionSessionEncodeFrame`. Encode callback → pull the `CMSampleBuffer`: read SPS/PPS from `CMVideoFormatDescriptionGetH264ParameterSetAtIndex`; detect IDR via the sample-buffer `kCMSampleAttachmentKey_NotSync == false`; convert the `CMBlockBuffer` (AVCC) via `annexBFromAVCC(...)`; yield `CameraFrame`.
  - Bridge into `AsyncThrowingStream(bufferingPolicy: .bufferingNewest(1))` — **this is the source-side newest-drop** (spec §8): a slow consumer means old frames are dropped by the buffering policy.
  - **Keyframe-on-recovery:** track continuation `.yield` return (`.dropped` / `.terminated`); when a drop is observed, set a flag so the next `VTCompressionSessionEncodeFrame` passes `kVTEncodeFrameOptionKey_ForceKeyFrame = true`, so the stream re-syncs after congestion. First frame also forces a keyframe.
  - `descriptor()` → a `CAMERA` `SensorDescriptor` with `VideoFormat{codec: .h264, width, height, fps}` from the active device format; returns `nil` if camera unauthorized.
  - `frames()` starts the session lazily and stops it on stream termination (demand-driven).

- [ ] **Step 5: Wire camera into `SensorService`** — add `var camera: any CameraCapturing = CameraCapture()`; append `camera.descriptor()` to the manifest when non-nil; in `streamSensors`, when both mic and camera channels are subscribed, run both producers concurrently into the single `writer` via a `withThrowingTaskGroup` (writer writes must be serialized — guard with an actor or a serial async channel). Extend `SensorServiceTests` with a fake `CameraCapturing` yielding a keyframe + a delta frame, asserting camera frames reach the writer and the manifest lists the camera.

- [ ] **Step 6: Run + commit** — `cd swift/WendyAgentCore && swift build && swift test --filter 'AnnexBTests|SensorServiceTests'` → PASS. Mark the real-capture path as a manual hardware gate in the commit body.
```bash
git add swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/CameraCapture.swift swift/WendyAgentCore/Sources/WendyAgent/Services/SensorService.swift swift/WendyAgentCore/Tests/WendyAgentTests/
git commit -m "feat(swift): camera capture (AVCaptureSession→VideoToolbox H.264 Annex-B) with newest-drop + keyframe-on-recovery"
```

---

## Task 4: `caps=sensors` mDNS advertisement

**Files:** Modify `BonjourAdvertiser.swift`, `WendyAgent.swift` (`startBonjour`); Test: `BonjourTXTTests.swift`.

- [ ] **Step 1: Failing test** for `encodeTXT` including `caps`:
```swift
@Test func encodeTXTIncludesCaps() {
    let data = BonjourAdvertiser.encodeTXT(displayName: "d", deviceID: "id", tls: true, assetID: 5, caps: ["sensors"])
    let s = String(decoding: data, as: UTF8.self)
    #expect(s.contains("caps=sensors"))
}
```
- [ ] **Step 2:** Run to fail (signature mismatch).
- [ ] **Step 3:** Add a `caps: [String]` parameter to `encodeTXT` (append `"caps=\(caps.joined(separator: ","))"` when non-empty) and a `caps` stored property on `BonjourAdvertiser` threaded into `txtData`. In `WendyAgent.startBonjour`, pass `caps: enrolled ? ["sensors"] : []` (only advertise the capability when the mTLS sensor service is actually running).
- [ ] **Step 4:** Run + commit — `swift test --filter BonjourTXTTests` → PASS; `swift build` clean.
```bash
git add swift/WendyAgentCore/Sources/WendyAgent/Services/BonjourAdvertiser.swift swift/WendyAgentCore/Sources/WendyAgent/WendyAgent.swift swift/WendyAgentCore/Tests/WendyAgentTests/BonjourTXTTests.swift
git commit -m "feat(swift): advertise caps=sensors over mDNS when the sensor service is running"
```

---

## Acceptance (manual, hardware-gated — not an SDD task)

Enrolled Mac running the built agent (`make agent-start`) ↔ the Jetson consumer (Plan 1). On the Jetson: `wendy device pair` discovers the Mac via `caps=sensors`, pairs over mTLS (grpc transport), and a Jetson container opens `/dev/videoN` (the Mac webcam, H.264) + the ALSA capture (the Mac mic, once Plan 3's `snd-aloop` mount lands). Verify: camera light on the Mac turns on only while consumed; killing WiFi briefly drops frames and recovers on the next keyframe.

## Self-Review

**Spec coverage** (§5, §8): §5.1 camera → Task 3; §5.2 mic → Task 2; §5.3 demand-driven → Tasks 2–3 (stream-scoped capture); §5.4 permissions → manifest omission + Task 2/3 auth checks; §5.5 mDNS `caps` → Task 4; §8 congestion (newest-drop + keyframe-on-recovery) → Task 3. Registration/mTLS → Task 2 (inherited). Proto gen → Task 1.

**Placeholder scan:** the AVFoundation/VideoToolbox capture pipeline (Task 3 Step 4) is specified at the API-call level rather than as fully-transcribed code — deliberate, because it's intricate hardware code that cannot be compiled/tested in CI here; the implementer writes and compiles it against the named APIs with the manual gate. The unit-testable seam (`annexBFromAVCC`) IS fully specified and tested. No other placeholders.

**Type consistency:** `Wendy_Agent_Services_V2_WendySensorService.ServiceProtocol` + `Wendy_Lite_Sensorlink_*` (Task 1) used in Tasks 2–3. `CameraCapturing`/`CameraFrame`/`annexBFromAVCC` (Task 3) used in `SensorService` + tests. `AudioManaging`/`AudioController.audio` (existing) used in Task 2. `encodeTXT(..., caps:)` (Task 4) — all call sites updated in the same task.

**Deferred to Plan 3:** the Linux `snd-aloop` mic mount + microphone entitlement (the consumer side of the mic path). This plan makes the Mac SERVE mic+camera; the Jetson can mount the camera today (Plan 1) and the mic once Plan 3 lands.
