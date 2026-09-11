# Agent Sensor Source — Plan 3: Linux snd-aloop mic mount Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the mic path: on the Linux consumer, mount a paired source's microphone as an ALSA capture device via `snd-aloop`, so a container sees the remote mic as a normal input. This is the audio analogue of Plan 1's v4l2loopback camera mount.

**Architecture:** A new `audioloop` package (Linux-tagged, non-Linux stub) `modprobe`s `snd-aloop`, allocates a loopback subdevice per `(source, channel)`, and exposes an `AudioWriter` PCM sink that pumps `SensorFrame` PCM payloads to the playback side `hw:Loopback,0,<sub>` (the container reads the mirrored capture side `hw:Loopback,1,<sub>`). Since no Go ALSA playback exists, the writer shells out to `aplay -t raw` (matching the repo's existing `arecord`/`aplay -l` shell-out convention). The `mcusource.Supervisor` fan-out gains a parallel MICROPHONE path alongside camera; the `audio` entitlement already binds `/dev/snd`, so the container side needs no new entitlement.

**Tech Stack:** Go (Linux build tags), `snd-aloop` kernel module, `aplay` (alsa-utils), the existing `mcusource` supervisor + `sensorlinkpb` payloads.

**Spec:** `specs/2026-08-31-agent-sensor-source-design.md` §7 (builds on Plan 1's consumer + Plan 2's mic-serving source).

## Global Constraints

- No reusable ALSA playback path exists — build a new one. Use `aplay -D hw:Loopback,0,<sub> -f S16_LE -r <rate> -c <ch> -t raw -` reading PCM from stdin (mirror `audio.ArecordCommand`'s shell-out shape). cgo alsa-lib is a future option, not v1.
- `snd-aloop` has **no per-node control ioctl** (unlike v4l2loopback): the card + subdevices are fixed at `modprobe` (`pcm_substreams`, default 8). Allocation = pick a free subdevice index; there is no `EnsureNode`/`NodePath`.
- Address the loopback card by NAME (`hw:Loopback,0,<sub>` / `hw:Loopback,1,<sub>`), stable regardless of card index.
- Reuse the existing `audio` entitlement (`applyAudio` already binds `/dev/snd`, major 116) for the container capture side — do NOT add a `microphone` entitlement.
- Audio codec is **PCM_S16LE** (Plan 2 serves this); the playback format comes from the channel's `AudioFormat` (`GetSampleRate()`/`GetChannels()`). The aloop capture side mirrors the playback format; the container uses `plughw` to resample if needed.
- Linux/non-Linux build-tag split (mirror `ipcam/loopback_linux.go` / `ros2camera/writer_other.go`): the real ALSA path is `//go:build linux`; a non-Linux stub errors cleanly so `go build ./...` passes on macOS.
- The camera fan-out behavior must be PRESERVED — this adds a parallel mic path, it does not change the camera path.
- Dependency (build/image): requires **`snd-aloop` in the WendyOS Jetson kernel** — call it out explicitly (parallels Plan 1's `v4l2loopback` requirement). The real mount is a manual hardware gate; unit tests cover the allocator + arg-builder + supervisor fan-out with stubs.
- TDD; editor "undefined" diagnostics on `go/internal/...` are stale-LSP — verify with `go -C go build/test`.

---

## File Structure

**New**
- `go/internal/agent/audioloop/audioloop.go` — `Manager` (portable: ensure/allocate/writer via injected deps), `AudioWriter` interface, `PCMFormat`, the pure `aplayArgs(...)` builder.
- `go/internal/agent/audioloop/audioloop_linux.go` — real deps: `modprobe snd-aloop`, `aplay`-backed `AudioWriter`.
- `go/internal/agent/audioloop/audioloop_other.go` — non-Linux stub deps (error).
- `go/internal/agent/audioloop/audioloop_test.go` — allocator + `aplayArgs` + manager-with-stub-deps tests.

**Modified**
- `go/internal/agent/mcusource/supervisor.go` — `microphoneChannels`; `streamOnce` builds camera AND mic writers, merges `subs`, routes frames by kind; guard becomes "no cams AND no mics"; inject an `AudioLoop` dependency into `Supervisor`/`NewSupervisor`.
- `go/internal/agent/mcusource/supervisor_test.go` — construct with the new dep (stub audio loop); a both-kinds test.
- `go/cmd/wendy-agent/main.go` — construct the real `audioloop.Manager` and pass it to `NewSupervisor`.
- (docs only) note that a mic-consuming app declares the existing `audio` entitlement.

---

## Task 1: `audioloop` package — manager + PCM `AudioWriter` + subdevice allocator

**Files:** Create `audioloop.go`, `audioloop_linux.go`, `audioloop_other.go`, `audioloop_test.go`.

**Interfaces:**
- `type PCMFormat struct { SampleRate uint32; Channels uint32 }`
- `type AudioWriter interface { WritePCM(pcm []byte) error; Close() error }`
- `type Manager struct { ... }` with injected deps `type deps struct { modprobe func(context.Context) error; newWriter func(hwID string, f PCMFormat) (AudioWriter, error) }` (real deps from the build-tagged files).
- `func NewManager(logger *zap.Logger) *Manager` (wires `defaultDeps()`).
- `func (m *Manager) EnsureModule(ctx context.Context) error` — `modprobe snd-aloop` once (idempotent).
- `func (m *Manager) Allocate(sourceAssetID int32, channelID uint32) (sub int, err error)` — per-`(source,channel)` free-subdevice allocator in `[0, MaxSubdevices)`; stable on repeat; error when exhausted. (Mirror `Supervisor.nodeID`.)
- `func (m *Manager) OpenWriter(ctx context.Context, sub int, f PCMFormat) (AudioWriter, error)` — `EnsureModule` then `newWriter("hw:Loopback,0,"+itoa(sub), f)`.
- `const MaxSubdevices = 8` (snd-aloop default `pcm_substreams`).
- Pure helper `func aplayArgs(hwID string, f PCMFormat) []string`.

- [ ] **Step 1: Failing tests** (`audioloop_test.go`) — allocator + arg-builder (both pure, CI-safe):
```go
package audioloop

import "testing"

func TestAllocateStableAndUnique(t *testing.T) {
	m := NewManager(nil)
	a, err := m.Allocate(1, 10)
	if err != nil { t.Fatal(err) }
	if again, _ := m.Allocate(1, 10); again != a {
		t.Fatalf("expected stable subdevice, got %d then %d", a, again)
	}
	b, _ := m.Allocate(2, 10)
	if b == a { t.Fatalf("different (source,channel) must get distinct subdevices, both %d", a) }
}

func TestAllocateExhaustion(t *testing.T) {
	m := NewManager(nil)
	for i := 0; i < MaxSubdevices; i++ {
		if _, err := m.Allocate(int32(i), 0); err != nil { t.Fatalf("alloc %d: %v", i, err) }
	}
	if _, err := m.Allocate(99, 0); err == nil { t.Fatal("expected exhaustion error") }
}

func TestAplayArgs(t *testing.T) {
	got := aplayArgs("hw:Loopback,0,3", PCMFormat{SampleRate: 48000, Channels: 2})
	want := []string{"-D", "hw:Loopback,0,3", "-f", "S16_LE", "-r", "48000", "-c", "2", "-t", "raw", "-q", "-"}
	if len(got) != len(want) { t.Fatalf("args %v != %v", got, want) }
	for i := range want { if got[i] != want[i] { t.Fatalf("arg %d: %q != %q", i, got[i], want[i]) } }
}
```

- [ ] **Step 2: Run to fail** — `go -C go test ./internal/agent/audioloop/` → FAIL (package/symbols undefined).

- [ ] **Step 3: Implement `audioloop.go`** — portable `Manager` (mutex-guarded `subs map[string]int` allocator mirroring `Supervisor.nodeID`, over `[0, MaxSubdevices)`), `EnsureModule`/`Allocate`/`OpenWriter` delegating to injected `deps`, `PCMFormat`, `AudioWriter`, and:
```go
func aplayArgs(hwID string, f PCMFormat) []string {
	return []string{"-D", hwID, "-f", "S16_LE",
		"-r", strconv.FormatUint(uint64(f.SampleRate), 10),
		"-c", strconv.FormatUint(uint64(f.Channels), 10),
		"-t", "raw", "-q", "-"}
}
```
`audioloop_linux.go` (`//go:build linux`): `defaultDeps()` with `modprobe`= `modprobe snd-aloop` (idempotent — treat "already loaded"/module-present as success; mirror `ipcam.modprobeLoopback`'s fallback shape), and `newWriter` = an `aplay`-backed writer: `exec.CommandContext(ctx, "aplay", aplayArgs(hwID,f)...)`, `cmd.Stdin` = a pipe; `WritePCM` writes to the pipe; `Close` closes the pipe + waits. `audioloop_other.go` (`//go:build !linux`): `defaultDeps()` whose `modprobe`/`newWriter` return `errors.New("snd-aloop mic mount requires Linux")`.

- [ ] **Step 4: Run to pass** — `go -C go test ./internal/agent/audioloop/ -race` → PASS; `go -C go build ./...` clean (stub covers macOS).

- [ ] **Step 5: Commit**
```bash
git add go/internal/agent/audioloop/
git commit -m "feat(audioloop): snd-aloop PCM sink + subdevice allocator (Linux; stub elsewhere)"
```

---

## Task 2: Supervisor mic fan-out + agent wiring

**Files:** Modify `mcusource/supervisor.go`, `supervisor_test.go`, `cmd/wendy-agent/main.go`.

**Interfaces:**
- New `Supervisor` dep: `type AudioLoop interface { Allocate(sourceAssetID int32, channelID uint32) (int, error); OpenWriter(ctx context.Context, sub int, f audioloop.PCMFormat) (audioloop.AudioWriter, error) }`.
- `NewSupervisor(logger, lb, transportFor, newWriter, audioLoop AudioLoop)` — add the `audioLoop` parameter.

- [ ] **Step 1: Failing test** — a both-kinds `streamOnce`/supervisor test: a manifest with a CAMERA (channel 2) + a MICROPHONE (channel 1, `AudioFormat{PCM_S16LE,48000,1}`), a stub `AudioLoop` returning a recording `AudioWriter`, the existing stub camera `Loopback`/writer, and a fake transport streaming one frame per channel. Assert: the camera frame reaches the camera writer AND the mic PCM payload reaches the audio writer; `subs` included both channels.
```go
// sketch — mirror the existing supervisor tests' fakes
type fakeAudioLoop struct{ w *recordingAudioWriter }
func (f *fakeAudioLoop) Allocate(int32, uint32) (int, error) { return 0, nil }
func (f *fakeAudioLoop) OpenWriter(context.Context, int, audioloop.PCMFormat) (audioloop.AudioWriter, error) { return f.w, nil }
// recordingAudioWriter records WritePCM payloads.
```

- [ ] **Step 2: Run to fail** — `NewSupervisor` arity / `microphoneChannels` undefined.

- [ ] **Step 3: Implement** in `supervisor.go`:
  - Add `audioLoop AudioLoop` field + `NewSupervisor` param.
  - `func microphoneChannels(m *sensorlinkpb.SensorManifest, allow []string) []*sensorlinkpb.SensorDescriptor` — mirror `cameraChannels` with `Kind == SensorDescriptor_MICROPHONE`.
  - In `streamOnce`: compute `cams` AND `mics`; change the short-circuit to `if len(cams)==0 && len(mics)==0 { return false, nil }`. Build camera writers (as today) AND mic writers: for each mic channel `sub,_ := s.audioLoop.Allocate(p.SourceAssetID, ch.ChannelId)`, `aw,_ := s.audioLoop.OpenWriter(ctx, sub, audioloop.PCMFormat{SampleRate: ch.GetAudio().GetSampleRate(), Channels: ch.GetAudio().GetChannels()})`; keep `audioWriters map[uint32]audioloop.AudioWriter` (defer Close all). Append both kinds to `subs`.
  - In the frame loop, route by channel: if a camera writer exists → `WriteFrame(frameToCamera(...))`; else if an audio writer exists → `WritePCM(f.Payload)`; set `delivered = true` on either.
  - Keep the ctx-cancel goroutine, backoff, `delivered` semantics, and `DeviceAssetId` guard unchanged.

- [ ] **Step 4: Wire `main.go`** — construct `audioLoop := audioloop.NewManager(logger)` and pass it to `NewSupervisor(...)` (keeps the build green — the signature change forces this call-site update in the same task).

- [ ] **Step 5: Run + commit** — `go -C go build ./... && go -C go test ./internal/agent/mcusource/ -race` → PASS (both-kinds test + all existing camera tests unchanged).
```bash
git add go/internal/agent/mcusource/supervisor.go go/internal/agent/mcusource/supervisor_test.go go/cmd/wendy-agent/main.go
git commit -m "feat(mcusource): microphone fan-out via snd-aloop alongside camera"
```

---

## Acceptance (manual, hardware-gated — not an SDD task)

On a WendyOS Jetson with `snd-aloop` + `v4l2loopback` in the kernel, paired to a Plan-2 Mac source: `wendy device pair` the Mac, then a container declaring the `camera` + `audio` entitlements opens `/dev/videoN` (Mac webcam) and `hw:Loopback,1,<sub>` (Mac mic). Verify the mic is audible/recordable (`arecord -D hw:Loopback,1,0 …`) and matches the Mac input.

## Self-Review

**Spec coverage** (§7): `snd-aloop` manager + PCM sink + subdevice allocator → Task 1; supervisor mic fan-out + demand-driven writers → Task 2; `/dev/snd` via the existing `audio` entitlement (reused, no new code) → documented; `snd-aloop` kernel dependency → flagged. Format from the manifest `AudioFormat` → Task 2. Congestion: the consumer already drops on a full frame channel (Plan 1); audio is low-bandwidth, no extra drop logic.

**Placeholder scan:** the real `aplay`/`modprobe` path (Task 1 `audioloop_linux.go`) is specified at the command level and is a manual hardware gate (no CI ALSA); the pure allocator + `aplayArgs` + the supervisor fan-out (with stubs) ARE fully unit-tested. No `TODO`s.

**Type consistency:** `AudioWriter`/`PCMFormat`/`Manager` (Task 1) consumed by the `AudioLoop` interface + `streamOnce` (Task 2) and `main.go`. `microphoneChannels` mirrors `cameraChannels`; `GetAudio().GetSampleRate()/GetChannels()` match the `sensorlinkpb.AudioFormat` accessors. `NewSupervisor`'s new `audioLoop` param — all call sites (main.go + tests) updated in Task 2.

**Completes the stack:** with this, camera AND mic are end-to-end (Mac source → Jetson mounts `/dev/videoN` + `hw:Loopback` capture). Remaining deferrals live in the design's future-work (adaptive bitrate, generic sensors, agent-as-source on Linux).
