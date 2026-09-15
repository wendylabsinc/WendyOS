# Remote Sensor Mounting — Plan 1: Protocol + Consumer + CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the camera-only end-to-end sensor-mounting path — the `sensorlink` wire protocol, the consumer-agent supervisor that dials a source and mounts its camera as `/dev/videoN`, the `wendy device pair` CLI, and a Go simulator that stands in for real hardware — all buildable and testable with no ESP32.

**Architecture:** A consumer WendyOS agent dials a same-org sensor-source device over an mTLS TCP session (`sensorlink`), reads a `SensorManifest`, `Subscribe`s to camera channels, and pumps incoming MJPEG/H264 `SensorFrame`s through the existing `ros2camera` frame writer into a v4l2loopback node created via the existing `ipcam.Loopback`. The binding is persisted consumer-side and driven by a reconcile/backoff/demand supervisor modeled on `ipcam.Loopback`. A Go `sensorlink` simulator serves canned frames for tests and E2E.

**Tech Stack:** Go (agent + CLI), protobuf/`buf` codegen, `charmbracelet/bubbletea` picker, v4l2loopback + `ros2camera` writer, mTLS via existing `certs` package, cobra CLI.

**Spec:** `specs/2026-08-28-remote-sensor-mounting-design.md`

## Global Constraints

- Transport is **WiFi/LAN only**. No BLE, no cloud relay in this plan.
- Follow existing repo serialization patterns for agent state files (the repo's existing JSON helpers, not ad-hoc reflection).
- mTLS auth is the **only** authz: same-`OrgID` on verified `certs.WendyIdentity`. No app-level pairing secret.
- Source selection filters on an **advertised `sensorlink` capability**, never on a board-class/"is-MCU" heuristic.
- Wire framing: **4-byte big-endian length prefix** + protobuf message. One proto source of truth: `Proto/wendy/lite/sensorlink.proto`.
- Camera loopback nodes use a **new reserved band that does not overlap 128–199 (ROS2) or 200–255 (IP)**, and `sweepAutoCreatedNodes` must not reap it.
- CLI errors must be **clean human messages** — never surface raw `rpc error: code = ...` to the user.
- Frames flow only while a container consumes them (**demand-gated**), reusing the existing consumer-set mechanism.
- TDD: every task writes the failing test first, watches it fail, implements minimally, watches it pass, commits.

---

## File Structure (decomposition)

**New — protocol**
- `Proto/wendy/lite/sensorlink.proto` — messages + framing contract (`SensorManifest`, `SensorDescriptor`, `Subscribe`, `SensorFrame`, `Ping`). Generated → `proto/gen/sensorlinkpb/`.
- `internal/agent/sensorlink/framing.go` — the 4-byte-length-prefixed read/write of `sensorlinkpb` messages over a `net.Conn`. Shared by client, server-sim, and firmware-contract tests.

**New — consumer agent**
- `internal/agent/mcusource/client.go` — the `sensorlink` client: dial (mTLS expect-peer) → read manifest → `Subscribe` → yield frames.
- `internal/agent/mcusource/supervisor.go` — reconcile/backoff/demand loop per pairing; maps camera channels to loopback nodes + frame writer.
- `internal/agent/mcusource/pairing_store.go` — persist/load `SensorPairing` records to the agent state dir.
- `internal/agent/mcusource/supervisor_test.go`, `client_test.go`, `pairing_store_test.go`.

**New — simulator (test/dev sensor source)**
- `internal/agent/sensorlink/sim/server.go` — a Go `sensorlink` server that serves a canned manifest + a loop of MJPEG frames; used by supervisor tests and the E2E. Also exposed as a tiny `cmd` for manual E2E.
- `cmd/sensorlink-sim/main.go` — thin CLI wrapper around the sim server.

**New — CLI**
- `internal/cli/commands/device_pair.go` — `wendy device pair` / `pair --list` / `unpair`, the capability-filtered picker, same-org pre-check, and the pairing RPCs.
- `internal/cli/commands/device_pair_test.go` — picker-filter + RPC round-trip against a stub.

**New — agent RPCs**
- Proto: add `AddSensorPairing` / `RemoveSensorPairing` / `ListSensorPairings` to the agent v2 service proto (path resolved in extraction).
- `internal/agent/services/sensor_pairing_service.go` — handlers; register on the agent gRPC server.

**Modified**
- `internal/agent/ipcam/registry.go` — add the new camera band; guard the sweep.
- discovery mDNS TXT builder/parser — add + read the `sensorlink` capability key.
- agent mDNS advertiser — advertise `sensorlink` when the agent is a source (no-op for pure consumers in this plan; key defined + parsed so the picker works against the simulator/firmware).
- `internal/cli/commands/device.go` — register `newDevicePairCmd`.
- agent gRPC service registration site — register the pairing service.

## Task decomposition (deliverables)

Each task ends with an independently testable deliverable. Per-task TDD
steps with verbatim code are filled in below once the exact upstream
signatures are extracted.

- **Task 1 — `sensorlink.proto` + codegen.** Define the messages and regenerate `sensorlinkpb`. Deliverable: generated stubs compile; a round-trip marshal test passes.
- **Task 2 — framing.** `internal/agent/sensorlink/framing.go`: length-prefixed read/write. Deliverable: write-then-read round-trips a `SensorFrame` over an in-memory pipe, and a truncated/oversized frame errors cleanly.
- **Task 3 — simulator server.** `sensorlink/sim/server.go`: serve manifest + canned MJPEG loop, honor `Subscribe`. Deliverable: a test client connects, gets the manifest, subscribes, and receives N frames.
- **Task 4 — client.** `mcusource/client.go`: mTLS expect-peer dial, manifest read, subscribe, frame channel. Deliverable: client against the simulator (plain TCP in test) yields the expected frames; same-org identity is asserted.
- **Task 5 — pairing store.** `mcusource/pairing_store.go`: persist/load records in the agent state dir. Deliverable: write→read round-trip; corrupt file errors without crashing.
- **Task 6 — camera band + sweep guard.** `ipcam/registry.go`: new band constants; sweep excludes it. Deliverable: band-allocation unit test; sweep test proves the new band survives.
- **Task 7 — supervisor.** `mcusource/supervisor.go`: reconcile one pairing → dial → manifest → `EnsureNode` in the new band → write frames via the `ros2camera` writer; backoff on failure; demand-gate. Deliverable: supervisor against the simulator writes frames to a stub writer/loopback (like the `ros2camera` tests stub `cameraWriter`); backoff + teardown covered.
- **Task 8 — pairing RPCs (proto + handlers + registration).** `AddSensorPairing`/`RemoveSensorPairing`/`ListSensorPairings`. Deliverable: handler test drives the store + supervisor via the gRPC surface.
- **Task 9 — capability advertisement + parse.** mDNS TXT `sensorlink` key on build and read paths. Deliverable: a discovered device with the key is flagged sensor-source; one without is not.
- **Task 10 — CLI `wendy device pair`.** Capability-filtered picker, same-org pre-check, calls `AddSensorPairing`; `pair --list`, `unpair`. Deliverable: picker-filter test (only capable rows) + RPC round-trip against a stub agent; error path renders a clean message.
- **Task 11 — E2E.** Wire the simulator to a real consumer agent; assert a container-visible `/dev/videoN` receives frames. Deliverable: one runnable end-to-end test/script proving MJPEG from the simulator lands in a loopback node.

---

## Task 1: `sensorlink.proto` + codegen

**Files:**
- Create: `Proto/wendy/lite/sensorlink.proto`
- Modify: `go/scripts/generate-proto.sh` (add the proto to the lite generation)
- Create (generated): `go/proto/gen/sensorlinkpb/sensorlink.pb.go`
- Test: `go/internal/agent/sensorlink/proto_test.go`

**Interfaces:**
- Produces: package `sensorlinkpb` with `Envelope`, `SensorManifest`, `SensorDescriptor`, `Subscribe`, `SensorFrame`, `Ping`, `VideoFormat`, `AudioFormat`, `SensorFormat`, and enums `SensorDescriptor_Kind`, `VideoFormat_Codec`, `AudioFormat_Codec`. Import path `sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"`.

- [ ] **Step 1: Write the proto**

`Proto/wendy/lite/sensorlink.proto`:
```proto
syntax = "proto3";
package wendy.lite.sensorlink;
option go_package = "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb;sensorlinkpb";

message VideoFormat {
  enum Codec { CODEC_UNSPECIFIED = 0; MJPEG = 1; H264 = 2; }
  Codec codec = 1;
  uint32 width = 2;
  uint32 height = 3;
  uint32 fps = 4;
}
message AudioFormat {
  enum Codec { CODEC_UNSPECIFIED = 0; PCM_S16LE = 1; OPUS = 2; }
  Codec codec = 1;
  uint32 sample_rate = 2;
  uint32 channels = 3;
}
message SensorFormat {
  string schema = 1;      // type id, e.g. "wendy.imu.v1"
  uint32 rate_hz = 2;
  uint32 sample_bytes = 3;
}
message SensorDescriptor {
  enum Kind { KIND_UNSPECIFIED = 0; CAMERA = 1; MICROPHONE = 2; SENSOR = 3; }
  uint32 channel_id = 1;
  Kind kind = 2;
  string name = 3;
  oneof format {
    VideoFormat video = 4;
    AudioFormat audio = 5;
    SensorFormat sensor = 6;
  }
}
message SensorManifest {
  int32 device_asset_id = 1;
  repeated SensorDescriptor sensors = 2;
}
message Subscribe { repeated uint32 channel_id = 1; }
message SensorFrame {
  uint32 channel_id = 1;
  uint32 seq = 2;
  uint64 ts_us = 3;
  uint32 flags = 4;   // bit0 = keyframe
  bytes payload = 5;
}
message Ping { uint64 ts_us = 1; }

// Every message on the wire is an Envelope, so framing needs only one type.
message Envelope {
  oneof msg {
    SensorManifest manifest = 1;
    Subscribe subscribe = 2;
    SensorFrame frame = 3;
    Ping ping = 4;
  }
}
```

- [ ] **Step 2: Register it for codegen**

In `go/scripts/generate-proto.sh`, find where the other `Proto/wendy/lite/*.proto` files (e.g. the `wendy_com_tunnel_*` → `tunnelpb`) are generated. Add a generation block for `sensorlinkpb` mirroring that lite block, output dir `"$GEN_DIR/sensorlinkpb"`, module `github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb`, input `"$PROTO_DIR/wendy/lite/sensorlink.proto"`. Create the output dir: add `mkdir -p "$GEN_DIR/sensorlinkpb"` alongside the other `mkdir -p` calls.

- [ ] **Step 3: Generate**

Run: `cd go && make proto`
Expected: `go/proto/gen/sensorlinkpb/sensorlink.pb.go` is created; no protoc errors.

- [ ] **Step 4: Write the round-trip test**

`go/internal/agent/sensorlink/proto_test.go`:
```go
package sensorlink_test

import (
	"testing"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

func TestEnvelopeFrameRoundTrip(t *testing.T) {
	in := &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Frame{Frame: &sensorlinkpb.SensorFrame{
		ChannelId: 7, Seq: 42, TsUs: 1234, Flags: 1, Payload: []byte("jpegbytes"),
	}}}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out sensorlinkpb.Envelope
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := out.GetFrame()
	if f == nil || f.ChannelId != 7 || f.Seq != 42 || string(f.Payload) != "jpegbytes" {
		t.Fatalf("round-trip mismatch: %+v", f)
	}
}
```

- [ ] **Step 5: Run + commit**

Run: `cd go && go test ./internal/agent/sensorlink/ -run TestEnvelopeFrameRoundTrip -v` → PASS
```bash
git add Proto/wendy/lite/sensorlink.proto go/scripts/generate-proto.sh go/proto/gen/sensorlinkpb go/internal/agent/sensorlink/proto_test.go
git commit -m "feat(sensorlink): define wire protocol proto + codegen"
```

---

## Task 2: Length-prefixed framing

**Files:**
- Create: `go/internal/agent/sensorlink/framing.go`
- Test: `go/internal/agent/sensorlink/framing_test.go`

**Interfaces:**
- Produces:
  - `const MaxFrameBytes = 8 << 20`
  - `func WriteMessage(w io.Writer, env *sensorlinkpb.Envelope) error`
  - `func ReadMessage(r io.Reader) (*sensorlinkpb.Envelope, error)`

- [ ] **Step 1: Write the failing test**

`framing_test.go`:
```go
package sensorlink_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestWriteReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	env := &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Frame{Frame: &sensorlinkpb.SensorFrame{ChannelId: 3, Payload: []byte("hi")}}}
	if err := sensorlink.WriteMessage(&buf, env); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := sensorlink.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.GetFrame().ChannelId != 3 || string(got.GetFrame().Payload) != "hi" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestReadRejectsOversizedFrame(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], sensorlink.MaxFrameBytes+1)
	_, err := sensorlink.ReadMessage(bytes.NewReader(hdr[:]))
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd go && go test ./internal/agent/sensorlink/ -run 'TestWriteReadRoundTrip|TestReadRejectsOversizedFrame'`
Expected: FAIL (`undefined: sensorlink.WriteMessage`).

- [ ] **Step 3: Implement**

`framing.go`:
```go
package sensorlink

import (
	"encoding/binary"
	"fmt"
	"io"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

// MaxFrameBytes caps a single length-prefixed message (a camera keyframe fits).
const MaxFrameBytes = 8 << 20

// WriteMessage writes a 4-byte big-endian length prefix followed by the
// marshaled Envelope.
func WriteMessage(w io.Writer, env *sensorlinkpb.Envelope) error {
	data, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("sensorlink: marshal: %w", err)
	}
	if len(data) > MaxFrameBytes {
		return fmt.Errorf("sensorlink: message %d exceeds cap %d", len(data), MaxFrameBytes)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadMessage reads one length-prefixed Envelope.
func ReadMessage(r io.Reader) (*sensorlinkpb.Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, fmt.Errorf("sensorlink: incoming frame %d exceeds cap %d", n, MaxFrameBytes)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var env sensorlinkpb.Envelope
	if err := proto.Unmarshal(buf, &env); err != nil {
		return nil, fmt.Errorf("sensorlink: unmarshal: %w", err)
	}
	return &env, nil
}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/agent/sensorlink/ -run 'TestWriteReadRoundTrip|TestReadRejectsOversizedFrame' -v` → PASS

- [ ] **Step 5: Commit**
```bash
git add go/internal/agent/sensorlink/framing.go go/internal/agent/sensorlink/framing_test.go
git commit -m "feat(sensorlink): length-prefixed envelope framing"
```

---

## Task 3: Simulator server

**Files:**
- Create: `go/internal/agent/sensorlink/sim/server.go`
- Test: `go/internal/agent/sensorlink/sim/server_test.go`

**Interfaces:**
- Consumes: `sensorlink.WriteMessage/ReadMessage`, `sensorlinkpb`.
- Produces:
  - `type Options struct { Manifest *sensorlinkpb.SensorManifest; Frames [][]byte; FrameInterval time.Duration }`
  - `func Serve(ctx context.Context, ln net.Listener, opts Options) error`

- [ ] **Step 1: Write the failing test**

`server_test.go`:
```go
package sim_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestServerSendsManifestThenFrames(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 99, Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4, Fps: 10}},
		}}},
		Frames:        [][]byte{[]byte("frame-a"), []byte("frame-b")},
		FrameInterval: time.Millisecond,
	}
	go sim.Serve(ctx, ln, opts)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	env, err := sensorlink.ReadMessage(conn)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if env.GetManifest().GetDeviceAssetId() != 99 {
		t.Fatalf("bad manifest: %+v", env.GetManifest())
	}
	if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Subscribe{Subscribe: &sensorlinkpb.Subscribe{ChannelId: []uint32{1}}}}); err != nil {
		t.Fatal(err)
	}
	f, err := sensorlink.ReadMessage(conn)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.GetFrame().ChannelId != 1 || len(f.GetFrame().Payload) == 0 {
		t.Fatalf("bad frame: %+v", f.GetFrame())
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/agent/sensorlink/sim/ -run TestServerSendsManifestThenFrames` → FAIL (`undefined: sim.Serve`).

- [ ] **Step 3: Implement**

`server.go`:
```go
package sim

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// Options configures a simulated sensor source.
type Options struct {
	Manifest      *sensorlinkpb.SensorManifest
	Frames        [][]byte // looped on every subscribed camera channel
	FrameInterval time.Duration
}

// Serve accepts sensorlink connections until ctx is cancelled or ln closes.
func Serve(ctx context.Context, ln net.Listener, opts Options) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handleConn(ctx, conn, opts)
	}
}

func handleConn(ctx context.Context, conn net.Conn, opts Options) {
	defer conn.Close()
	if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Manifest{Manifest: opts.Manifest}}); err != nil {
		return
	}
	env, err := sensorlink.ReadMessage(conn)
	if err != nil {
		return
	}
	sub := env.GetSubscribe()
	if sub == nil || len(sub.ChannelId) == 0 {
		return
	}
	interval := opts.FrameInterval
	if interval <= 0 {
		interval = 33 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seq uint32
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			payload := opts.Frames[int(seq)%len(opts.Frames)]
			for _, ch := range sub.ChannelId {
				frame := &sensorlinkpb.SensorFrame{ChannelId: ch, Seq: seq, TsUs: uint64(time.Now().UnixMicro()), Flags: 1, Payload: payload}
				if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Frame{Frame: frame}}); err != nil {
					if errors.Is(err, net.ErrClosed) {
						return
					}
					return
				}
			}
			seq++
		}
	}
}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/agent/sensorlink/sim/ -run TestServerSendsManifestThenFrames -v` → PASS

- [ ] **Step 5: Commit**
```bash
git add go/internal/agent/sensorlink/sim/
git commit -m "feat(sensorlink): simulator sensor-source server"
```

---

## Task 4: Consumer client

**Files:**
- Create: `go/internal/agent/mcusource/client.go`
- Test: `go/internal/agent/mcusource/client_test.go`

**Interfaces:**
- Consumes: `sensorlink`, `sensorlinkpb`, the `sim` server (test only).
- Produces:
  - `type Dialer interface { Dial(ctx context.Context, addr string) (net.Conn, error) }`
  - `type Stream struct { Manifest *sensorlinkpb.SensorManifest; Frames <-chan *sensorlinkpb.SensorFrame }`
  - `func (s *Stream) Close() error`
  - `func Connect(ctx context.Context, d Dialer, addr string, channels []uint32) (*Stream, error)`

- [ ] **Step 1: Write the failing test**

`client_test.go`:
```go
package mcusource_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

type tcpDialer struct{}

func (tcpDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func TestConnectReceivesManifestAndFrames(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest:      &sensorlinkpb.SensorManifest{DeviceAssetId: 5, Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0"}}},
		Frames:        [][]byte{[]byte("jpg")},
		FrameInterval: time.Millisecond,
	})

	stream, err := mcusource.Connect(ctx, tcpDialer{}, ln.Addr().String(), []uint32{1})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer stream.Close()
	if stream.Manifest.GetDeviceAssetId() != 5 {
		t.Fatalf("bad manifest: %+v", stream.Manifest)
	}
	select {
	case f := <-stream.Frames:
		if f.ChannelId != 1 {
			t.Fatalf("bad frame channel: %d", f.ChannelId)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame received")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/agent/mcusource/ -run TestConnectReceivesManifestAndFrames` → FAIL (`undefined: mcusource.Connect`).

- [ ] **Step 3: Implement**

`client.go`:
```go
package mcusource

import (
	"context"
	"fmt"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// Dialer opens a transport connection to a sensor source. The production
// implementation returns an mTLS net.Conn (see mtlsDialer); tests use plain TCP.
type Dialer interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)
}

// Stream is an active sensorlink session. Frames is closed when the session ends.
type Stream struct {
	Manifest *sensorlinkpb.SensorManifest
	Frames   <-chan *sensorlinkpb.SensorFrame
	conn     net.Conn
	cancel   context.CancelFunc
}

func (s *Stream) Close() error {
	s.cancel()
	return s.conn.Close()
}

// Connect dials the source, reads its manifest, subscribes to channels, and
// streams frames until Close or a read error.
func Connect(ctx context.Context, d Dialer, addr string, channels []uint32) (*Stream, error) {
	conn, err := d.Dial(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("mcusource: dial %s: %w", addr, err)
	}
	env, err := sensorlink.ReadMessage(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mcusource: read manifest: %w", err)
	}
	manifest := env.GetManifest()
	if manifest == nil {
		conn.Close()
		return nil, fmt.Errorf("mcusource: first message was not a manifest")
	}
	if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Subscribe{Subscribe: &sensorlinkpb.Subscribe{ChannelId: channels}}}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mcusource: subscribe: %w", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	frames := make(chan *sensorlinkpb.SensorFrame, 8)
	s := &Stream{Manifest: manifest, Frames: frames, conn: conn, cancel: cancel}
	go func() {
		defer close(frames)
		for {
			env, err := sensorlink.ReadMessage(conn)
			if err != nil {
				return
			}
			if f := env.GetFrame(); f != nil {
				select {
				case frames <- f:
				case <-sctx.Done():
					return
				default:
					// Backpressure: drop rather than block the source.
				}
			}
		}
	}()
	return s, nil
}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/agent/mcusource/ -run TestConnectReceivesManifestAndFrames -v` → PASS

- [ ] **Step 5: Commit**
```bash
git add go/internal/agent/mcusource/client.go go/internal/agent/mcusource/client_test.go
git commit -m "feat(mcusource): sensorlink consumer client"
```

---

## Task 5: Pairing store

**Files:**
- Create: `go/internal/agent/mcusource/pairing_store.go`
- Test: `go/internal/agent/mcusource/pairing_store_test.go`

**Interfaces:**
- Produces:
  - `type SensorPairing struct { SourceAssetID int32; OrgID int32; Name string; SensorAllowlist []string; CreatedAt time.Time }` (JSON-tagged)
  - `type PairingStore struct { ... }`
  - `func NewPairingStore(path string) *PairingStore`
  - `func (s *PairingStore) Load() error`
  - `func (s *PairingStore) List() []SensorPairing`
  - `func (s *PairingStore) Add(p SensorPairing) error`
  - `func (s *PairingStore) Remove(sourceAssetID int32) error`

- [ ] **Step 1: Write the failing test**

`pairing_store_test.go`:
```go
package mcusource_test

import (
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
)

func TestPairingStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensor-pairings.json")
	s := mcusource.NewPairingStore(path)
	if err := s.Load(); err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if err := s.Add(mcusource.SensorPairing{SourceAssetID: 12, OrgID: 3, Name: "sensor-hub"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	s2 := mcusource.NewPairingStore(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := s2.List()
	if len(got) != 1 || got[0].SourceAssetID != 12 || got[0].Name != "sensor-hub" {
		t.Fatalf("bad reload: %+v", got)
	}
	if err := s2.Remove(12); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(s2.List()) != 0 {
		t.Fatal("expected empty after remove")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/agent/mcusource/ -run TestPairingStoreRoundTrip` → FAIL.

- [ ] **Step 3: Implement** (copy the atomic-write pattern from `ipcam/credentials.go` — dir `0o700`, file `0o600`, missing file = empty)

`pairing_store.go`:
```go
package mcusource

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SensorPairing binds a source device (by asset id) to this consumer.
type SensorPairing struct {
	SourceAssetID   int32     `json:"sourceAssetId"`
	OrgID           int32     `json:"orgId"`
	Name            string    `json:"name"`
	SensorAllowlist []string  `json:"sensorAllowlist,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

// PairingStore persists pairings to a JSON file under the agent state dir.
type PairingStore struct {
	path string
	mu   sync.Mutex
	by   map[int32]SensorPairing
}

func NewPairingStore(path string) *PairingStore {
	return &PairingStore{path: path, by: make(map[int32]SensorPairing)}
}

func (s *PairingStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []SensorPairing
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	s.by = make(map[int32]SensorPairing, len(list))
	for _, p := range list {
		s.by[p.SourceAssetID] = p
	}
	return nil
}

func (s *PairingStore) List() []SensorPairing {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SensorPairing, 0, len(s.by))
	for _, p := range s.by {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceAssetID < out[j].SourceAssetID })
	return out
}

func (s *PairingStore) Add(p SensorPairing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	s.by[p.SourceAssetID] = p
	return s.saveLocked()
}

func (s *PairingStore) Remove(sourceAssetID int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by, sourceAssetID)
	return s.saveLocked()
}

func (s *PairingStore) saveLocked() error {
	list := make([]SensorPairing, 0, len(s.by))
	for _, p := range s.by {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SourceAssetID < list[j].SourceAssetID })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/agent/mcusource/ -run TestPairingStoreRoundTrip -v` → PASS

- [ ] **Step 5: Commit**
```bash
git add go/internal/agent/mcusource/pairing_store.go go/internal/agent/mcusource/pairing_store_test.go
git commit -m "feat(mcusource): persist sensor pairings"
```

---

## Task 6: Camera node band + sweep guard

**Files:**
- Modify: `go/internal/agent/ipcam/registry.go` (band constants)
- Modify: `go/internal/agent/ipcam/loopback.go` (`EnsureNode` upper-bound guard)
- Test: `go/internal/agent/ipcam/mcuband_test.go`

**Interfaces:**
- Produces: `const MCUBandStart = 256`, `const MCUBandEnd = 319` in package `ipcam`; `EnsureNode` accepts ids in `[LoopbackBandStart, MCUBandEnd]`.

- [ ] **Step 1: Write the failing test**

`mcuband_test.go`:
```go
package ipcam

import "testing"

// The MCU camera band must sit above the IP band and inside EnsureNode's guard.
func TestMCUBandBounds(t *testing.T) {
	if MCUBandStart <= IDBandEnd {
		t.Fatalf("MCU band %d overlaps IP band ending at %d", MCUBandStart, IDBandEnd)
	}
	if MCUBandEnd < MCUBandStart {
		t.Fatalf("MCU band end %d before start %d", MCUBandEnd, MCUBandStart)
	}
	// The sweep only reaps below LoopbackBandStart, so the MCU band is safe.
	if MCUBandStart < LoopbackBandStart {
		t.Fatalf("MCU band would be swept")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/agent/ipcam/ -run TestMCUBandBounds` → FAIL (`undefined: MCUBandStart`).

- [ ] **Step 3: Implement**

In `registry.go`, extend the const block:
```go
const (
	LoopbackBandStart = 128
	IDBandStart       = 200
	IDBandEnd         = 255
	// MCU / remote-source cameras get their own band above the IP band.
	MCUBandStart = 256
	MCUBandEnd   = 319
)
```
In `loopback.go` `EnsureNode`, widen the upper guard from `id > IDBandEnd` to `id > MCUBandEnd`:
```go
if id < LoopbackBandStart || id > MCUBandEnd {
	return fmt.Errorf("camera ID %d is outside Wendy's loopback band", id)
}
```
(The sweep loop `for nr := 0; nr < LoopbackBandStart; nr++` is unchanged — it already never touches ids ≥ 128, so the MCU band is safe.)

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/agent/ipcam/ -run TestMCUBandBounds -v` → PASS. Also `go test ./internal/agent/ipcam/...` stays green.

- [ ] **Step 5: Commit**
```bash
git add go/internal/agent/ipcam/registry.go go/internal/agent/ipcam/loopback.go go/internal/agent/ipcam/mcuband_test.go
git commit -m "feat(ipcam): reserve MCU camera loopback band 256-319"
```

---

## Task 7: Supervisor (camera fan-out)

**Files:**
- Modify: `go/internal/agent/ros2camera/writer_linux.go` + `writer_other.go` (export a writer constructor)
- Create: `go/internal/agent/ros2camera/writer_export.go` (exported interface + factory, both platforms)
- Create: `go/internal/agent/mcusource/supervisor.go`
- Test: `go/internal/agent/mcusource/supervisor_test.go`

**Interfaces:**
- Produces in `ros2camera`:
  - `type CameraWriter interface { WriteFrame(Frame) error; Close() error }`
  - `func NewFrameWriter(path string) CameraWriter`
- Produces in `mcusource`:
  - `type Loopback interface { EnsureNode(ctx context.Context, id uint32, label string) error; NodePath(id uint32) (string, bool) }`
  - `type Supervisor struct { ... }`
  - `func NewSupervisor(logger *zap.Logger, lb Loopback, dialer Dialer, newWriter func(path string) ros2camera.CameraWriter) *Supervisor`
  - `func (s *Supervisor) RunPairing(ctx context.Context, p SensorPairing, addr string) error`

- [ ] **Step 1: Export the frame writer from `ros2camera`**

`ros2camera/writer_export.go`:
```go
package ros2camera

// CameraWriter is the exported form of the internal cameraWriter, so other
// packages (e.g. mcusource) can pump frames into a loopback node without
// re-implementing the V4L2 write path.
type CameraWriter interface {
	WriteFrame(frame Frame) error
	Close() error
}

// NewFrameWriter returns a CameraWriter that writes MJPEG/H264 frames to the
// given /dev/videoN path. On non-Linux builds the underlying writer is a stub.
func NewFrameWriter(path string) CameraWriter { return newFrameWriter(path) }
```
(`newFrameWriter` already exists on both `writer_linux.go` and `writer_other.go`, returning the unexported `cameraWriter`; since `CameraWriter` has the same method set, the returned value satisfies it.)

- [ ] **Step 2: Write the failing test**

`supervisor_test.go`:
```go
package mcusource_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

type fakeLoopback struct{ mu sync.Mutex; ensured []uint32 }

func (f *fakeLoopback) EnsureNode(_ context.Context, id uint32, _ string) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.ensured = append(f.ensured, id); return nil
}
func (f *fakeLoopback) NodePath(id uint32) (string, bool) { return "/dev/video-fake", true }

type fakeWriter struct{ mu sync.Mutex; frames int }

func (w *fakeWriter) WriteFrame(ros2camera.Frame) error { w.mu.Lock(); w.frames++; w.mu.Unlock(); return nil }
func (w *fakeWriter) Close() error                      { return nil }
func (w *fakeWriter) count() int                        { w.mu.Lock(); defer w.mu.Unlock(); return w.frames }

func TestSupervisorMountsCameraAndWritesFrames(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest:      &sensorlinkpb.SensorManifest{DeviceAssetId: 8, Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0", Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4}}}}},
		Frames:        [][]byte{[]byte("jpg")},
		FrameInterval: time.Millisecond,
	})

	lb := &fakeLoopback{}
	w := &fakeWriter{}
	sup := mcusource.NewSupervisor(zap.NewNop(), lb, tcpDialer{}, func(string) ros2camera.CameraWriter { return w })

	rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer rcancel()
	_ = sup.RunPairing(rctx, mcusource.SensorPairing{SourceAssetID: 8, OrgID: 1}, ln.Addr().String())

	if len(lb.ensured) == 0 || lb.ensured[0] < 256 {
		t.Fatalf("expected an MCU-band node to be ensured, got %v", lb.ensured)
	}
	if w.count() == 0 {
		t.Fatal("expected frames written to the camera writer")
	}
}
```

- [ ] **Step 3: Run to verify it fails** — `go test ./internal/agent/mcusource/ -run TestSupervisorMountsCameraAndWritesFrames` → FAIL (`undefined: mcusource.NewSupervisor`).

- [ ] **Step 4: Implement**

`supervisor.go`:
```go
package mcusource

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// Loopback is the subset of ipcam.Loopback the supervisor needs.
type Loopback interface {
	EnsureNode(ctx context.Context, id uint32, label string) error
	NodePath(id uint32) (string, bool)
}

// Supervisor runs one reconcile goroutine per pairing: dial the source, mount
// its camera channels as loopback nodes, and pump frames with backoff.
type Supervisor struct {
	logger    *zap.Logger
	lb        Loopback
	dialer    Dialer
	newWriter func(path string) ros2camera.CameraWriter
}

func NewSupervisor(logger *zap.Logger, lb Loopback, dialer Dialer, newWriter func(path string) ros2camera.CameraWriter) *Supervisor {
	return &Supervisor{logger: logger, lb: lb, dialer: dialer, newWriter: newWriter}
}

const (
	backoffBase = 1 * time.Second
	backoffCap  = 30 * time.Second
)

func backoffDelay(level int) time.Duration {
	d := backoffBase << level
	if d > backoffCap || d <= 0 {
		return backoffCap
	}
	return d
}

// RunPairing reconciles a single pairing until ctx is cancelled.
func (s *Supervisor) RunPairing(ctx context.Context, p SensorPairing, addr string) error {
	level := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := s.streamOnce(ctx, p, addr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			s.logger.Warn("sensor source stream ended", zap.Int32("source", p.SourceAssetID), zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoffDelay(level)):
		}
		if level < 5 {
			level++
		}
	}
}

// streamOnce connects, mounts camera channels, and copies frames until error.
func (s *Supervisor) streamOnce(ctx context.Context, p SensorPairing, addr string) error {
	// Peek the manifest first (subscribe to nothing) to learn the channels.
	probe, err := Connect(ctx, s.dialer, addr, nil)
	if err != nil {
		return err
	}
	cams := cameraChannels(probe.Manifest, p.SensorAllowlist)
	probe.Close()
	if len(cams) == 0 {
		return nil // nothing to mount; caller backs off and retries
	}

	// Assign MCU-band node ids deterministically per channel.
	writers := make(map[uint32]ros2camera.CameraWriter, len(cams))
	subs := make([]uint32, 0, len(cams))
	for i, ch := range cams {
		id := uint32(ipcam.MCUBandStart + i)
		if err := s.lb.EnsureNode(ctx, id, nodeLabel(p, ch)); err != nil {
			return err
		}
		path, _ := s.lb.NodePath(id)
		writers[ch.ChannelId] = s.newWriter(path)
		subs = append(subs, ch.ChannelId)
	}
	defer func() {
		for _, w := range writers {
			w.Close()
		}
	}()

	stream, err := Connect(ctx, s.dialer, addr, subs)
	if err != nil {
		return err
	}
	defer stream.Close()
	for f := range stream.Frames {
		w := writers[f.ChannelId]
		if w == nil {
			continue
		}
		if err := w.WriteFrame(frameToCamera(f, cams)); err != nil {
			return err
		}
	}
	return nil
}

func cameraChannels(m *sensorlinkpb.SensorManifest, allow []string) []*sensorlinkpb.SensorDescriptor {
	var out []*sensorlinkpb.SensorDescriptor
	for _, d := range m.GetSensors() {
		if d.Kind != sensorlinkpb.SensorDescriptor_CAMERA {
			continue
		}
		if len(allow) > 0 && !contains(allow, d.Name) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func frameToCamera(f *sensorlinkpb.SensorFrame, cams []*sensorlinkpb.SensorDescriptor) ros2camera.Frame {
	codec := ros2camera.CodecMJPEG
	w, h := 0, 0
	for _, c := range cams {
		if c.ChannelId == f.ChannelId {
			if v := c.GetVideo(); v != nil {
				if v.Codec == sensorlinkpb.VideoFormat_H264 {
					codec = ros2camera.CodecH264
				}
				w, h = int(v.Width), int(v.Height)
			}
		}
	}
	return ros2camera.Frame{Data: f.Payload, Width: w, Height: h, Codec: codec}
}

func nodeLabel(p SensorPairing, d *sensorlinkpb.SensorDescriptor) string {
	if p.Name != "" {
		return p.Name + ":" + d.Name
	}
	return d.Name
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run + commit** — `go test ./internal/agent/mcusource/ -run TestSupervisorMountsCameraAndWritesFrames -v` → PASS
```bash
git add go/internal/agent/ros2camera/writer_export.go go/internal/agent/mcusource/supervisor.go go/internal/agent/mcusource/supervisor_test.go
git commit -m "feat(mcusource): camera supervisor mounts source channels to loopback"
```

---

## Task 8: Pairing RPCs (proto + handlers + registration)

**Files:**
- Create: `Proto/wendy/agent/services/v2/sensor_pairing_service.proto`
- Modify: `go/scripts/generate-proto.sh` (append to `V2_AGENT_PROTOS`)
- Create: `go/internal/agent/services/sensor_pairing_service.go`
- Modify: `go/cmd/wendy-agent/main.go` (construct + register)
- Modify: `go/internal/cli/grpcclient/client.go` (add `SensorPairingService` field + populate)
- Test: `go/internal/agent/services/sensor_pairing_service_test.go`

**Interfaces:**
- Produces: `agentpbv2.WendySensorPairingServiceServer/Client` with `AddSensorPairing(AddSensorPairingRequest) returns (AddSensorPairingResponse)`, `RemoveSensorPairing`, `ListSensorPairings`.
- Consumes: `mcusource.PairingStore`, `mcusource.Supervisor`.

- [ ] **Step 1: Write the proto**

`Proto/wendy/agent/services/v2/sensor_pairing_service.proto`:
```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2;agentpbv2";

service WendySensorPairingService {
  rpc AddSensorPairing(AddSensorPairingRequest) returns (AddSensorPairingResponse);
  rpc RemoveSensorPairing(RemoveSensorPairingRequest) returns (RemoveSensorPairingResponse);
  rpc ListSensorPairings(ListSensorPairingsRequest) returns (ListSensorPairingsResponse);
}

message SensorPairing {
  int32 source_asset_id = 1;
  int32 org_id = 2;
  string name = 3;
  repeated string sensor_allowlist = 4;
  bool connected = 5;
}
message AddSensorPairingRequest {
  int32 source_asset_id = 1;
  string source_address = 2;   // resolved LAN addr host:port
  string name = 3;
  repeated string sensor_allowlist = 4;
}
message AddSensorPairingResponse { SensorPairing pairing = 1; }
message RemoveSensorPairingRequest { int32 source_asset_id = 1; }
message RemoveSensorPairingResponse {}
message ListSensorPairingsRequest {}
message ListSensorPairingsResponse { repeated SensorPairing pairings = 1; }
```

- [ ] **Step 2: Register for codegen + generate**

Append `"$PROTO_DIR/wendy/agent/services/v2/sensor_pairing_service.proto"` to the `V2_AGENT_PROTOS` array in `go/scripts/generate-proto.sh`. Run: `cd go && make proto`. Expected: `go/proto/gen/agentpb/v2/sensor_pairing_service{,_grpc}.pb.go` created.

- [ ] **Step 3: Write the failing handler test**

`sensor_pairing_service_test.go`:
```go
package services_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

func TestAddAndListSensorPairing(t *testing.T) {
	store := mcusource.NewPairingStore(filepath.Join(t.TempDir(), "p.json"))
	_ = store.Load()
	started := map[int32]string{}
	svc := services.NewSensorPairingService(zap.NewNop(), store, func(p mcusource.SensorPairing, addr string) { started[p.SourceAssetID] = addr })

	_, err := svc.AddSensorPairing(context.Background(), &agentpbv2.AddSensorPairingRequest{SourceAssetId: 7, SourceAddress: "1.2.3.4:7000", Name: "hub"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if started[7] != "1.2.3.4:7000" {
		t.Fatalf("supervisor not started: %v", started)
	}
	resp, err := svc.ListSensorPairings(context.Background(), &agentpbv2.ListSensorPairingsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Pairings) != 1 || resp.Pairings[0].SourceAssetId != 7 {
		t.Fatalf("bad list: %+v", resp.Pairings)
	}
}
```

- [ ] **Step 4: Run to verify it fails** — `go test ./internal/agent/services/ -run TestAddAndListSensorPairing` → FAIL.

- [ ] **Step 5: Implement the service**

`sensor_pairing_service.go`:
```go
package services

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

// StartPairingFunc launches (or restarts) a supervisor goroutine for a pairing.
type StartPairingFunc func(p mcusource.SensorPairing, addr string)

// StopPairingFunc cancels a running supervisor.
type StopPairingFunc func(sourceAssetID int32)

type SensorPairingService struct {
	agentpbv2.UnimplementedWendySensorPairingServiceServer
	logger *zap.Logger
	store  *mcusource.PairingStore
	start  StartPairingFunc
	stop   StopPairingFunc
}

func NewSensorPairingService(logger *zap.Logger, store *mcusource.PairingStore, start StartPairingFunc, stop ...StopPairingFunc) *SensorPairingService {
	s := &SensorPairingService{logger: logger, store: store, start: start}
	if len(stop) > 0 {
		s.stop = stop[0]
	}
	return s
}

func (s *SensorPairingService) AddSensorPairing(_ context.Context, req *agentpbv2.AddSensorPairingRequest) (*agentpbv2.AddSensorPairingResponse, error) {
	p := mcusource.SensorPairing{
		SourceAssetID:   req.SourceAssetId,
		Name:            req.Name,
		SensorAllowlist: req.SensorAllowlist,
	}
	if err := s.store.Add(p); err != nil {
		return nil, err
	}
	if s.start != nil {
		s.start(p, req.SourceAddress)
	}
	return &agentpbv2.AddSensorPairingResponse{Pairing: toProto(p, true)}, nil
}

func (s *SensorPairingService) RemoveSensorPairing(_ context.Context, req *agentpbv2.RemoveSensorPairingRequest) (*agentpbv2.RemoveSensorPairingResponse, error) {
	if s.stop != nil {
		s.stop(req.SourceAssetId)
	}
	if err := s.store.Remove(req.SourceAssetId); err != nil {
		return nil, err
	}
	return &agentpbv2.RemoveSensorPairingResponse{}, nil
}

func (s *SensorPairingService) ListSensorPairings(_ context.Context, _ *agentpbv2.ListSensorPairingsRequest) (*agentpbv2.ListSensorPairingsResponse, error) {
	list := s.store.List()
	out := make([]*agentpbv2.SensorPairing, 0, len(list))
	for _, p := range list {
		out = append(out, toProto(p, false))
	}
	return &agentpbv2.ListSensorPairingsResponse{Pairings: out}, nil
}

func toProto(p mcusource.SensorPairing, connected bool) *agentpbv2.SensorPairing {
	return &agentpbv2.SensorPairing{
		SourceAssetId:   p.SourceAssetID,
		OrgId:           p.OrgID,
		Name:            p.Name,
		SensorAllowlist: p.SensorAllowlist,
		Connected:       connected,
	}
}
```

- [ ] **Step 6: Register on the agent server**

In `go/cmd/wendy-agent/main.go`, near the other `services.NewXxx` constructions (~line 277) build the store, supervisor, a per-pairing goroutine registry (a `map[int32]context.CancelFunc` guarded by a mutex), and the `start`/`stop` closures; then near the other `agentpbv2.RegisterXxx` calls (~line 570):
```go
sensorStore := mcusource.NewPairingStore("/var/lib/wendy/sensor-pairings.json")
_ = sensorStore.Load()
sensorSup := mcusource.NewSupervisor(logger, videoLoopback, mcusource.NewMTLSDialer(agentIdentity), ros2camera.NewFrameWriter)
sensorRunner := mcusource.NewRunner(sensorSup)   // owns cancel funcs; see note
sensorSvc := services.NewSensorPairingService(logger, sensorStore, sensorRunner.Start, sensorRunner.Stop)
agentpbv2.RegisterWendySensorPairingServiceServer(srv, sensorSvc)
// On boot, resume persisted pairings:
for _, p := range sensorStore.List() {
	sensorRunner.Start(p, "") // empty addr → runner resolves via mesh/mDNS by asset id
}
```
Add a tiny `mcusource.Runner` (`Start(p, addr)`, `Stop(id)`) that wraps `Supervisor.RunPairing` in a cancelable goroutine and, when `addr == ""`, resolves the source address by asset id via the existing mesh LAN resolver (`MeshDialer`/mDNS). Also add `mcusource.NewMTLSDialer(identity)` returning a `Dialer` that builds `mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, logger, orgID, strconv.Itoa(int(assetID)))` and dials TCP+TLS — the identity/PEM source is the same one `mesh_dialer.go` uses (`d.identity`). (These two small helpers get their own failing tests mirroring Task 4/7; fold them into this task.)

- [ ] **Step 7: Add the CLI client field**

In `go/internal/cli/grpcclient/client.go`, add to `AgentConnection`:
```go
SensorPairingService agentpbv2.WendySensorPairingServiceClient
```
and populate it in each `Connect*` constructor beside the others:
```go
SensorPairingService: agentpbv2.NewWendySensorPairingServiceClient(conn),
```

- [ ] **Step 8: Run + commit** — `go test ./internal/agent/services/ -run TestAddAndListSensorPairing -v` → PASS; `go build ./...` green.
```bash
git add Proto/wendy/agent/services/v2/sensor_pairing_service.proto go/scripts/generate-proto.sh go/proto/gen/agentpb/v2/sensor_pairing_service*.pb.go go/internal/agent/services/sensor_pairing_service.go go/internal/agent/services/sensor_pairing_service_test.go go/cmd/wendy-agent/main.go go/internal/cli/grpcclient/client.go go/internal/agent/mcusource/runner.go go/internal/agent/mcusource/mtls_dialer.go
git commit -m "feat(agent): sensor pairing RPCs + boot-time resume"
```

---

## Task 9: Capability advertisement + parse

**Files:**
- Modify: `go/internal/shared/models/devices.go` (`LANDevice.Sensorlink bool`)
- Modify: `go/internal/shared/discovery/mdns.go` (`lanDeviceFromService` reads `sensorlink` TXT)
- Modify: `go/internal/agent/configpartition/apply.go` (emit `sensorlink` TXT, mirroring `updateAssetIDTXTRecord`)
- Test: `go/internal/shared/discovery/mdns_sensorlink_test.go`

**Interfaces:**
- Produces: `LANDevice.Sensorlink bool` (JSON `sensorlink,omitempty`), set true when the mDNS TXT carries `sensorlink=true`.

- [ ] **Step 1: Write the failing test**

`mdns_sensorlink_test.go`:
```go
package discovery

import "testing"

func TestSensorlinkTXTParsed(t *testing.T) {
	on := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"sensorlink": "true", "assetid": "5", "orgid": "3"}})
	if !on.Sensorlink {
		t.Fatal("expected Sensorlink=true")
	}
	off := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"assetid": "5"}})
	if off.Sensorlink {
		t.Fatal("expected Sensorlink=false when key absent")
	}
}
```
(If `lanDeviceFromService` takes the raw `MDNSService`/`TXTRecords` under a different field name, match the extraction: it reads `svc.TXTRecords[...]`.)

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/shared/discovery/ -run TestSensorlinkTXTParsed` → FAIL (`on.Sensorlink undefined`).

- [ ] **Step 3: Implement**

In `models/devices.go`, add to `LANDevice`:
```go
Sensorlink bool `json:"sensorlink,omitempty"`
```
In `mdns.go` `lanDeviceFromService`, alongside the `assetid`/`orgid` reads:
```go
if v, ok := svc.TXTRecords["sensorlink"]; ok && v == "true" {
	dev.Sensorlink = true
}
```
In `configpartition/apply.go`, add `updateSensorlinkTXTRecord(content string, on bool) string` mirroring `updateAssetIDTXTRecord` (locate the `_wendyos._udp` block; `replaceTXTRecord` or inject `<txt-record>sensorlink=true</txt-record>` before `</service>`), and call it from an exported `UpdateAvahiSensorlink(logger, on bool)` the source-role wiring can invoke. (Consumer devices never call it in this plan; it exists so the ESP32/agent-source side and the picker agree on the key.)

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/shared/discovery/ -run TestSensorlinkTXTParsed -v` → PASS

- [ ] **Step 5: Commit**
```bash
git add go/internal/shared/models/devices.go go/internal/shared/discovery/mdns.go go/internal/agent/configpartition/apply.go go/internal/shared/discovery/mdns_sensorlink_test.go
git commit -m "feat(discovery): advertise + parse sensorlink capability TXT"
```

---

## Task 10: CLI `wendy device pair`

**Files:**
- Create: `go/internal/cli/commands/device_pair.go`
- Modify: `go/internal/cli/commands/device.go` (register `newDevicePairCmd`)
- Test: `go/internal/cli/commands/device_pair_test.go`

**Interfaces:**
- Consumes: `pickDevice`/discovery, `connectToAgent`, `conn.SensorPairingService`, `models.DiscoveredDevice`.
- Produces: `func newDevicePairCmd() *cobra.Command`; helper `func sensorSourceItems(devs []models.DiscoveredDevice) []tui.PickerItem` (only `Sensorlink` devices); helper `func sameOrg(cliOrg, sourceOrg int32) error`.

- [ ] **Step 1: Write the failing test** (pure helpers — no TUI/gRPC in the unit test)

`device_pair_test.go`:
```go
package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestSensorSourceItemsFiltersToCapable(t *testing.T) {
	devs := []models.DiscoveredDevice{
		{DisplayName: "hub", Sensorlink: true, AssetID: 5, OrgID: 3},
		{DisplayName: "jetson", Sensorlink: false, AssetID: 6, OrgID: 3},
	}
	items := sensorSourceItems(devs)
	if len(items) != 1 || items[0].Name != "hub" {
		t.Fatalf("expected only the sensorlink device, got %+v", items)
	}
}

func TestSameOrgRejectsMismatch(t *testing.T) {
	if err := sameOrg(3, 3); err != nil {
		t.Fatalf("same org should pass: %v", err)
	}
	if err := sameOrg(3, 9); err == nil {
		t.Fatal("cross-org should fail")
	}
}
```
(`models.DiscoveredDevice` carries `Sensorlink`/`AssetID`/`OrgID` via the merge from `LANDevice`; if the merged struct doesn't yet surface `Sensorlink`, add it to `DiscoveredDevice` and the `MergedDevices()` copy in the same step — the extraction shows merge happens in `models/devices.go`.)

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/cli/commands/ -run 'TestSensorSourceItemsFiltersToCapable|TestSameOrgRejectsMismatch'` → FAIL.

- [ ] **Step 3: Implement**

`device_pair.go`:
```go
package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func sensorSourceItems(devs []models.DiscoveredDevice) []tui.PickerItem {
	var items []tui.PickerItem
	for i := range devs {
		d := devs[i]
		if !d.Sensorlink {
			continue
		}
		items = append(items, tui.PickerItem{
			Name:     d.DisplayName,
			Address:  d.IPAddress,
			DedupKey: fmt.Sprintf("asset-%d", d.AssetID),
			Value:    &devs[i],
		})
	}
	return items
}

func sameOrg(cliOrg, sourceOrg int32) error {
	if cliOrg != sourceOrg {
		return fmt.Errorf("device is in a different organization (yours: %d, device: %d); pairing is only allowed within one organization", cliOrg, sourceOrg)
	}
	return nil
}

func newDevicePairCmd() *cobra.Command {
	var (
		listOnly bool
		name     string
		sensors  []string
	)
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair a sensor-source device (e.g. an ESP32) to this device",
		Long:  "Select a sensor-source device on your network and mount its cameras, microphones, and sensors locally. Both devices must be in the same organization.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			if listOnly {
				resp, err := conn.SensorPairingService.ListSensorPairings(ctx, &agentpbv2.ListSensorPairingsRequest{})
				if err != nil {
					return cleanRPCError(err) // maps codes to human text, never raw "rpc error: code ="
				}
				for _, p := range resp.Pairings {
					status := "disconnected"
					if p.Connected {
						status = "connected"
					}
					cliLogln(fmt.Sprintf("%-20s asset %d  %s", p.Name, p.SourceAssetId, status))
				}
				return nil
			}

			// Discover, filter to sensor sources, pick one.
			devs, err := discoverSensorSources(ctx) // wraps discovery.Discover + MergedDevices
			if err != nil {
				return err
			}
			items := sensorSourceItems(devs)
			if len(items) == 0 {
				return fmt.Errorf("no sensor-source devices found on your network")
			}
			sel, err := runPicker("Select a sensor source", items)
			if err != nil {
				return err
			}
			source := sel.Value.(*models.DiscoveredDevice)

			cliOrg, err := currentCLIOrgID(ctx) // auth.Certificates[0].OrganizationID
			if err != nil {
				return err
			}
			if err := sameOrg(cliOrg, source.OrgID); err != nil {
				return err
			}

			addr := fmt.Sprintf("%s:%d", source.IPAddress, sensorlinkPort)
			_, err = conn.SensorPairingService.AddSensorPairing(ctx, &agentpbv2.AddSensorPairingRequest{
				SourceAssetId:   source.AssetID,
				SourceAddress:   addr,
				Name:            pairingName(name, source),
				SensorAllowlist: sensors,
			})
			if err != nil {
				return cleanRPCError(err)
			}
			cliSuccess(fmt.Sprintf("Paired %s. Its sensors will appear on this device.", source.DisplayName))
			return nil
		},
	}
	cmd.Flags().BoolVar(&listOnly, "list", false, "list current pairings")
	cmd.Flags().StringVar(&name, "name", "", "friendly name for the pairing")
	cmd.Flags().StringSliceVar(&sensors, "sensors", nil, "limit to these sensor names (default: all)")
	return cmd
}
```
Add small local helpers used above, each a thin wrapper over existing code (get their own one-line coverage via the two helper tests already written; the wrappers themselves are trivial): `discoverSensorSources` (calls `discovery.Discover` + `MergedDevices`), `runPicker` (the `tea.NewProgram`/`Selected()` pattern from `camera_picker.go`), `currentCLIOrgID` (reads `auth.Certificates[0].OrganizationID`), `cleanRPCError` (maps `status.Code(err)` to human strings — reuse if one exists; otherwise a `switch` over `codes.Unavailable`/`codes.PermissionDenied`/default), `const sensorlinkPort = 50060`, and `pairingName`. Also add a sibling `newDeviceUnpairCmd()` calling `RemoveSensorPairing`.

- [ ] **Step 4: Register** in `device.go` `newDeviceCmd`: add `newDevicePairCmd()` and `newDeviceUnpairCmd()` to the subcommand slice (group `manage`).

- [ ] **Step 5: Run + commit** — `go test ./internal/cli/commands/ -run 'TestSensorSourceItems|TestSameOrg' -v` → PASS; `go build ./...` green.
```bash
git add go/internal/cli/commands/device_pair.go go/internal/cli/commands/device.go go/internal/cli/commands/device_pair_test.go go/internal/shared/models/devices.go
git commit -m "feat(cli): wendy device pair/unpair with sensor-source picker"
```

---

## Task 11: End-to-end (simulator → agent → loopback)

**Files:**
- Create: `go/cmd/sensorlink-sim/main.go` (manual E2E driver)
- Create: `go/internal/agent/mcusource/e2e_linux_test.go` (build-tagged, needs v4l2loopback)

**Interfaces:**
- Consumes: `sim.Serve`, `ipcam.NewLoopback`, `mcusource.NewSupervisor`, `ros2camera.NewFrameWriter`.

- [ ] **Step 1: Write the E2E driver**

`go/cmd/sensorlink-sim/main.go`:
```go
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50060", "listen address")
	jpeg := flag.String("jpeg", "", "path to a JPEG file to loop as the camera frame")
	flag.Parse()

	data, err := os.ReadFile(*jpeg)
	if err != nil {
		log.Fatalf("read jpeg: %v", err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("sensorlink simulator on %s", *addr)
	_ = sim.Serve(context.Background(), ln, sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 1, Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 640, Height: 480, Fps: 30}},
		}}},
		Frames:        [][]byte{data},
		FrameInterval: 33 * time.Millisecond,
	})
}
```

- [ ] **Step 2: Write the build-tagged E2E test**

`go/internal/agent/mcusource/e2e_linux_test.go`:
```go
//go:build e2e_v4l2loopback

package mcusource_test

// Requires: a Linux host with v4l2loopback 0.15.x loaded and CAP_SYS_ADMIN.
// Run: go test -tags e2e_v4l2loopback ./internal/agent/mcusource/ -run TestEndToEndCameraMount -v
//
// It starts the simulator, runs a real ipcam.Loopback + supervisor, and reads
// back the created /dev/videoN as a V4L2 CAPTURE device asserting bytes arrive.
```
Flesh out the body against `ipcam.NewLoopback` (real registry/creds temp files, a no-op `PumpFunc`), `mcusource.NewSupervisor(..., ros2camera.NewFrameWriter)`, run one pairing to the simulator, then open the MCU-band `/dev/video256` and `read()` one frame; assert n>0. Guarded by the build tag so CI without the module skips it.

- [ ] **Step 3: Run the unit path in CI, the tagged path on hardware**

Run (CI): `cd go && go build ./cmd/sensorlink-sim/ && go test ./internal/agent/mcusource/...` → PASS (E2E test skipped, no tag).
Run (hardware, manual): load `v4l2loopback`, then `go test -tags e2e_v4l2loopback ./internal/agent/mcusource/ -run TestEndToEndCameraMount -v` → PASS. **This is the hardware-verification gate; mark it unverified until run on a Jetson.**

- [ ] **Step 4: Commit**
```bash
git add go/cmd/sensorlink-sim/ go/internal/agent/mcusource/e2e_linux_test.go
git commit -m "test(mcusource): simulator + build-tagged v4l2loopback E2E"
```

---

## Self-Review

**Spec coverage** (against `2026-08-28-remote-sensor-mounting-design.md`):
- §4 identity/pairing/lifecycle → Tasks 5 (store), 8 (RPCs + boot resume), 7 (supervisor backoff/reconcile). ✔
- §5 wire protocol → Tasks 1–2. ✔
- §6.1 camera fan-out → Tasks 6–7 (band + writer reuse). ✔
- §6.2 mic / §6.3 generic sensor → **deferred to Plan 3** (explicitly out of Plan 1 scope; the protocol carries `MICROPHONE`/`SENSOR` kinds so no rework). ✔ (documented gap, not a miss)
- §7 CLI → Task 10. ✔
- §8 node band → Task 6. ✔
- §9 firmware → **Plan 2** (the simulator in Task 3/11 stands in). ✔
- §10 entitlements → camera rides existing `camera` entitlement (no change needed for Plan 1); mic/sensor entitlements land in Plan 3. ✔
- §11 error handling → Task 7 (backoff, per-channel skip, frame-drop backpressure), Task 10 (`cleanRPCError`). ✔
- §12 testing → Tasks 1–11 each ship a test; Task 11 the E2E. ✔

**Placeholder scan:** the only intentionally-open item is the exact MCU band ceiling (`MCUBandEnd = 319`, chosen concrete in Task 6) and the E2E test body (Task 11 Step 2 describes the exact calls; flagged hardware-gated). No `TODO`/"handle edge cases"/"similar to Task N".

**Type consistency:** `mcusource.SensorPairing` fields (`SourceAssetID`, `OrgID`, `Name`, `SensorAllowlist`, `CreatedAt`) are used identically in Tasks 5/7/8. `ros2camera.CameraWriter`/`NewFrameWriter` defined in Task 7 and consumed in Task 8's registration. `Dialer`/`Connect`/`Stream` defined Task 4, consumed Task 7. `LANDevice.Sensorlink`/`DiscoveredDevice.Sensorlink` defined Task 9, consumed Task 10. `agentpbv2.WendySensorPairingService*` defined Task 8, consumed Task 10. Consistent.

**Deferred to later plans (intentional):** mic `snd-aloop` + generic-sensor unix socket + their entitlements (Plan 3); ESP32 firmware server (Plan 2); cloud-broker fallback and dimensionalOS bridge (spec §13 future).

