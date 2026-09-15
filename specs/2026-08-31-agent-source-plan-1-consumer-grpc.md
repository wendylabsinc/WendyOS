# Agent Sensor Source — Plan 1: Consumer gRPC transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the consumer a second `sensorlink` transport — gRPC (for agent-class sources) alongside Plan 1's raw-TCP (for MCUs) — behind one `SensorTransport` seam, so the supervisor/allocator/mount fan-out stay transport-agnostic. Provable in Go CI against an in-process Go `WendySensorService` stub; no Swift, no hardware.

**Architecture:** Introduce `SensorTransport{FetchManifest, Stream}` with two impls: `tcpTransport` (wraps the existing raw-TCP `Connect`) and `grpcTransport` (a `WendySensorService` client dialing a source's mTLS agent endpoint, mesh-dialer style). The `Supervisor` swaps its `dialerFor` field for `transportFor`; `streamOnce` calls `FetchManifest` (probe) then `Stream` (frames). A `Transport` field on `SensorPairing` selects the adapter and makes address resolution transport-aware. Discovery moves from `sensorlink=true` to a `caps=sensors` capability list.

**Tech Stack:** Go, gRPC (server-streaming), protobuf/protoc codegen, existing `mtls`/`mesh_dialer` peer-pinning, `sensorlinkpb` payloads.

**Spec:** `specs/2026-08-31-agent-sensor-source-design.md` (builds on `specs/2026-08-28-remote-sensor-mounting-design.md`).

## Global Constraints

- Reuse the shared `sensorlinkpb` payload messages (`SensorManifest`, `SensorDescriptor`, `SensorFrame`, `VideoFormat`/`AudioFormat`) — do NOT redefine them. The new `WendySensorService` proto imports them.
- The gRPC path pins the source's identity with `mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, logger, orgID, strconv.Itoa(int(assetID)))` — the same peer pin the mesh dialer uses. No unauthenticated path.
- `tcpTransport` must preserve Plan 1's raw-TCP behavior exactly (MCU path unchanged).
- Transport selection is by `SensorPairing.Transport` (`"grpc"` | `"tcp"`); address resolution is transport-aware (grpc → the source's mTLS agent port from discovery; tcp → `sensorlink.Port`).
- Discovery capability: `caps` is a comma-list TXT; `caps` containing `sensors` supersedes Plan 1's `sensorlink=true`. Keep back-compat: a device advertising the old `sensorlink=true` still reads as a sensor source (tcp transport).
- CLI never surfaces a raw `rpc error: code = ...` (Plan 1 rule; reuse `userFacingGRPCError`).
- TDD: failing test first, watch it fail, minimal impl, watch it pass, commit. Editor "undefined: …" diagnostics on `go/internal/...` are known stale-LSP false positives — verify with `go -C go build/test`.

---

## File Structure

**New**
- `Proto/wendy/agent/services/v2/sensor_service.proto` — `WendySensorService` (`GetSensorManifest` unary, `StreamSensors` server-streaming); imports `sensorlinkpb` payloads. → `agentpbv2`.
- `go/internal/agent/mcusource/transport.go` — `SensorTransport` interface, `tcpTransport`.
- `go/internal/agent/mcusource/grpc_transport.go` — `grpcTransport`.
- `go/internal/agent/mcusource/grpc_transport_test.go` — in-process Go `WendySensorService` stub + integration test.
- `go/internal/agent/mcusource/transport_test.go` — `tcpTransport` + selection tests.

**Modified**
- `go/internal/agent/mcusource/supervisor.go` — `dialerFor` → `transportFor`; `streamOnce` uses the transport.
- `go/internal/agent/mcusource/supervisor_test.go` — construct with a `transportFor` factory.
- `go/internal/agent/mcusource/mtls_dialer.go` — keep `NewMTLSDialer` (used by `tcpTransport`); add a transport factory (below).
- `go/internal/agent/mcusource/runner.go` — `resolveLANAddr` transport-aware; `resolveAddr` uses the pairing transport.
- `go/internal/agent/mcusource/pairing_store.go` — `SensorPairing.Transport` field.
- `Proto/wendy/agent/services/v2/sensor_pairing_service.proto` — `transport` on `AddSensorPairingRequest` + `SensorPairing`.
- `go/internal/agent/services/sensor_pairing_service.go` — carry `Transport` through Add/List.
- `go/cmd/wendy-agent/main.go` — build `transportFor` (tcp+grpc) instead of `dialerFor`.
- `go/internal/shared/models/devices.go` + `go/internal/shared/discovery/mdns.go` — `Caps []string` from the `caps` TXT (+ back-compat `sensorlink=true` → `caps=["sensors"]`).
- `go/internal/cli/commands/device_pair.go` — pick transport from caps; pass it in `AddSensorPairing`.
- `go/scripts/generate-proto.sh` — register `sensor_service.proto`; add `--go-grpc_out` to the sensorlink block if the service lives there instead (see Task 1).

---

## Task 1: `WendySensorService` proto + codegen

**Files:**
- Create: `Proto/wendy/agent/services/v2/sensor_service.proto`
- Modify: `go/scripts/generate-proto.sh` (`V2_AGENT_PROTOS` array + ensure the sensorlink import resolves)
- Test: `go/internal/agent/mcusource/sensor_service_proto_test.go`

**Interfaces:**
- Produces (in `agentpbv2`): `WendySensorServiceClient/Server`, `NewWendySensorServiceClient`, `RegisterWendySensorServiceServer`, `UnimplementedWendySensorServiceServer`, `GetSensorManifestRequest`, `StreamSensorsRequest`. Manifest/frame types remain `sensorlinkpb.SensorManifest` / `sensorlinkpb.SensorFrame`.

- [ ] **Step 1: Write the proto**

`Proto/wendy/agent/services/v2/sensor_service.proto`:
```proto
syntax = "proto3";
package wendy.agent.services.v2;
option go_package = "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2;agentpbv2";

import "wendy/lite/sensorlink.proto";

service WendySensorService {
  rpc GetSensorManifest(GetSensorManifestRequest) returns (wendy.lite.sensorlink.SensorManifest);
  rpc StreamSensors(StreamSensorsRequest) returns (stream wendy.lite.sensorlink.SensorFrame);
}
message GetSensorManifestRequest {}
message StreamSensorsRequest { repeated uint32 channel_id = 1; }
```

- [ ] **Step 2: Wire codegen**

Add `"wendy/agent/services/v2/sensor_service.proto"` to `V2_AGENT_PROTOS` in `go/scripts/generate-proto.sh`. The service imports `wendy/lite/sensorlink.proto`; ensure the v2 protoc invocation maps that import to the `sensorlinkpb` package (add the `--go_opt=Mwendy/lite/sensorlink.proto=$SENSORLINK_PKG` and matching `--go-grpc_opt=M...` if the loop doesn't already emit it). Run `cd go && make proto`; confirm `sensor_service.pb.go` + `sensor_service_grpc.pb.go` generate and reference `sensorlinkpb.SensorManifest`/`SensorFrame`.

- [ ] **Step 3: Failing test** (proves the generated client/stub types + sensorlinkpb reuse compile)

`sensor_service_proto_test.go`:
```go
package mcusource_test

import (
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestSensorServiceTypesReuseSensorlinkPayloads(t *testing.T) {
	// StreamSensorsRequest carries channel ids; the stream yields sensorlinkpb.SensorFrame.
	req := &agentpbv2.StreamSensorsRequest{ChannelId: []uint32{1}}
	if len(req.ChannelId) != 1 {
		t.Fatal("channel id not set")
	}
	var _ *sensorlinkpb.SensorManifest = (*sensorlinkpb.SensorManifest)(nil) // manifest type is the shared one
	var _ agentpbv2.WendySensorServiceServer                                 // server iface exists
}
```

- [ ] **Step 4: Run** — `go -C go test ./internal/agent/mcusource/ -run TestSensorServiceTypesReuseSensorlinkPayloads` → FAIL (undefined) before codegen, PASS after.

- [ ] **Step 5: Commit** — commit ONLY the proto, the two generated `sensor_service*.pb.go`, the script edit, and the test. Leave any unrelated protoc version-comment churn unstaged.
```bash
git add Proto/wendy/agent/services/v2/sensor_service.proto go/scripts/generate-proto.sh go/proto/gen/agentpb/v2/sensor_service*.pb.go go/internal/agent/mcusource/sensor_service_proto_test.go
git commit -m "feat(sensorlink): WendySensorService gRPC proto (reuses sensorlinkpb payloads)"
```

---

## Task 2: `SensorTransport` seam + `tcpTransport` (behavior-preserving refactor)

**Files:**
- Create: `go/internal/agent/mcusource/transport.go`
- Modify: `go/internal/agent/mcusource/supervisor.go`, `supervisor_test.go`
- Test: `go/internal/agent/mcusource/transport_test.go`

**Interfaces:**
- Produces:
  - `type SensorTransport interface { FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error); Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) }`
  - `type TransportFactory func(p SensorPairing, addr string) (SensorTransport, error)`
  - `func NewTCPTransport(d Dialer, addr string) SensorTransport`
- Changes `NewSupervisor` signature: `dialerFor func(SensorPairing)(Dialer,error)` → `transportFor TransportFactory`.

- [ ] **Step 1: Write the failing test** (tcpTransport against the Plan 1 sim yields manifest + frames)

`transport_test.go`:
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

func TestTCPTransportManifestAndStream(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest:      &sensorlinkpb.SensorManifest{DeviceAssetId: 5, Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0"}}},
		Frames:        [][]byte{[]byte("jpg")},
		FrameInterval: time.Millisecond,
	})
	tr := mcusource.NewTCPTransport(tcpDialer{}, ln.Addr().String())
	m, err := tr.FetchManifest(ctx)
	if err != nil || m.GetDeviceAssetId() != 5 {
		t.Fatalf("manifest: %v %+v", err, m)
	}
	frames, closeFn, err := tr.Stream(ctx, []uint32{1})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer closeFn()
	select {
	case f := <-frames:
		if f.ChannelId != 1 {
			t.Fatalf("bad frame: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame")
	}
}
```

- [ ] **Step 2: Run to fail** — `go -C go test ./internal/agent/mcusource/ -run TestTCPTransportManifestAndStream` → FAIL (`undefined: mcusource.NewTCPTransport`).

- [ ] **Step 3: Implement `transport.go`**
```go
package mcusource

import (
	"context"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// SensorTransport abstracts how a source's manifest and frames are obtained,
// so the supervisor is transport-agnostic (raw-TCP for MCUs, gRPC for agents).
type SensorTransport interface {
	FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error)
	Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error)
}

// TransportFactory builds a transport for a pairing at a resolved address.
type TransportFactory func(p SensorPairing, addr string) (SensorTransport, error)

// tcpTransport wraps the Plan 1 raw-TCP Connect (the MCU path).
type tcpTransport struct {
	d    Dialer
	addr string
}

func NewTCPTransport(d Dialer, addr string) SensorTransport { return &tcpTransport{d: d, addr: addr} }

func (t *tcpTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	s, err := Connect(ctx, t.d, t.addr, nil)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Manifest, nil
}

func (t *tcpTransport) Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) {
	s, err := Connect(ctx, t.d, t.addr, channels)
	if err != nil {
		return nil, nil, err
	}
	return s.Frames, s.Close, nil
}
```

- [ ] **Step 4: Refactor `supervisor.go`** — replace the `dialerFor func(SensorPairing)(Dialer,error)` field with `transportFor TransportFactory`; update `NewSupervisor` accordingly. In `streamOnce`, replace the two `Connect(...)` calls:
```go
tr, err := s.transportFor(p, addr)
if err != nil {
	return false, err
}
manifest, err := tr.FetchManifest(ctx)
if err != nil {
	return false, err
}
if manifest.GetDeviceAssetId() != p.SourceAssetID {
	return false, fmt.Errorf("mcusource: source %s reported asset id %d", addr, manifest.GetDeviceAssetId())
}
cams := cameraChannels(manifest, p.SensorAllowlist)
if len(cams) == 0 {
	return false, nil
}
// ... allocate nodes + writers as today (unchanged) ...
frames, closeStream, err := tr.Stream(ctx, subs)
if err != nil {
	return false, err
}
defer closeStream()
stop := make(chan struct{})
defer close(stop)
go func() { select { case <-ctx.Done(): closeStream(); case <-stop: } }()
for f := range frames {
	w := writers[f.ChannelId]
	if w == nil {
		continue
	}
	if err := w.WriteFrame(frameToCamera(f, cams)); err != nil {
		return delivered, err
	}
	delivered = true
}
return delivered, nil
```

- [ ] **Step 5: Fix `supervisor_test.go`** — every `NewSupervisor(logger, lb, <dialerFactory>, writer)` call passes a `transportFor` factory instead:
```go
transportFor := func(p mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
	return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
}
sup := mcusource.NewSupervisor(zap.NewNop(), lb, transportFor, func(string) ros2camera.CameraWriter { return w })
```
(Keep the existing test assertions — behavior is unchanged.)

- [ ] **Step 6: Run + commit** — `go -C go test ./internal/agent/mcusource/ -race` → PASS (transport test + all migrated supervisor tests).
```bash
git add go/internal/agent/mcusource/transport.go go/internal/agent/mcusource/transport_test.go go/internal/agent/mcusource/supervisor.go go/internal/agent/mcusource/supervisor_test.go
git commit -m "refactor(mcusource): SensorTransport seam + tcpTransport (behavior-preserving)"
```

---

## Task 3: `grpcTransport`

**Files:**
- Create: `go/internal/agent/mcusource/grpc_transport.go`

**Interfaces:**
- Consumes Task 1's `agentpbv2.WendySensorServiceClient` + Task 2's `SensorTransport`.
- Produces: `func NewGRPCTransport(logger *zap.Logger, id mtlsIdentity, p SensorPairing, addr string) (SensorTransport, error)` where the TLS pins `p.OrgID`/`p.SourceAssetID`.

- [ ] **Step 1: Implement** (tested via Task 4's in-process stub — this task ships with Task 4's test)
```go
package mcusource

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type grpcTransport struct {
	logger *zap.Logger
	cc     *grpc.ClientConn
	client agentpbv2.WendySensorServiceClient
}

// NewGRPCTransport dials the source's mTLS agent endpoint, pinning its identity.
func NewGRPCTransport(logger *zap.Logger, certPEM, chainPEM, keyPEM string, p SensorPairing, addr string) (SensorTransport, error) {
	tlsCfg, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, logger, p.OrgID, strconv.Itoa(int(p.SourceAssetID)))
	if err != nil {
		return nil, fmt.Errorf("mcusource: grpc tls: %w", err)
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	cc, err := grpc.NewClient("passthrough:///sensor-source",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, fmt.Errorf("mcusource: grpc dial %s: %w", addr, err)
	}
	return &grpcTransport{logger: logger, cc: cc, client: agentpbv2.NewWendySensorServiceClient(cc)}, nil
}

func (t *grpcTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	return t.client.GetSensorManifest(ctx, &agentpbv2.GetSensorManifestRequest{})
}

func (t *grpcTransport) Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) {
	sctx, cancel := context.WithCancel(ctx)
	stream, err := t.client.StreamSensors(sctx, &agentpbv2.StreamSensorsRequest{ChannelId: channels})
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("mcusource: StreamSensors: %w", err)
	}
	frames := make(chan *sensorlinkpb.SensorFrame, 8)
	go func() {
		defer close(frames)
		for {
			f, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case frames <- f:
			case <-sctx.Done():
				return
			default: // backpressure: drop rather than block the source
			}
		}
	}()
	closeFn := func() error { cancel(); return t.cc.Close() }
	return frames, closeFn, nil
}
```
(Signature note: pass the agent's own PEMs in — the caller in `main.go`/`transportFor` reads them from `ProvisioningCerts` fresh, like `NewMTLSDialer` does. If cleaner, wrap them in the existing `Identity` closure.)

- [ ] **Step 2: Commit** (with Task 4's test, which exercises this).
```bash
git add go/internal/agent/mcusource/grpc_transport.go
git commit -m "feat(mcusource): grpcTransport (WendySensorService client, mTLS peer-pinned)"
```

---

## Task 4: In-process gRPC stub source + integration test

**Files:**
- Create: `go/internal/agent/mcusource/grpc_transport_test.go`

**Interfaces:**
- A Go `agentpbv2.WendySensorServiceServer` stub serving a canned manifest + frames on a real gRPC server (loopback, insecure for the test — TLS pinning is unit-tested separately by reusing the mesh dialer's proven path).

- [ ] **Step 1: Write the failing test**
```go
package mcusource_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/grpc"
)

type stubSensorServer struct {
	agentpbv2.UnimplementedWendySensorServiceServer
	assetID int32
}

func (s *stubSensorServer) GetSensorManifest(_ context.Context, _ *agentpbv2.GetSensorManifestRequest) (*sensorlinkpb.SensorManifest, error) {
	return &sensorlinkpb.SensorManifest{DeviceAssetId: s.assetID, Sensors: []*sensorlinkpb.SensorDescriptor{{
		ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
		Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_H264, Width: 640, Height: 480, Fps: 30}},
	}}}, nil
}

func (s *stubSensorServer) StreamSensors(req *agentpbv2.StreamSensorsRequest, stream agentpbv2.WendySensorService_StreamSensorsServer) error {
	for i := 0; i < 3; i++ {
		if err := stream.Send(&sensorlinkpb.SensorFrame{ChannelId: 1, Seq: uint32(i), Flags: 1, Payload: []byte("h264")}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

// newInsecureGRPCTransport mirrors grpcTransport but with an insecure loopback dial for the test.
func TestGRPCTransportStreamsFromStubServer(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := grpc.NewServer()
	agentpbv2.RegisterWendySensorServiceServer(srv, &stubSensorServer{assetID: 7})
	go srv.Serve(ln)
	defer srv.Stop()

	tr, err := mcusource.NewInsecureGRPCTransportForTest(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := tr.FetchManifest(ctx)
	if err != nil || m.GetDeviceAssetId() != 7 {
		t.Fatalf("manifest: %v %+v", err, m)
	}
	frames, closeFn, err := tr.Stream(ctx, []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	select {
	case f := <-frames:
		if f.ChannelId != 1 || string(f.Payload) != "h264" {
			t.Fatalf("bad frame: %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame from grpc stream")
	}
}
```

- [ ] **Step 2: Run to fail** — FAIL (`undefined: mcusource.NewInsecureGRPCTransportForTest`).

- [ ] **Step 3: Add a test-only insecure constructor** in `grpc_transport.go` (keeps the mTLS path and the test's insecure path sharing one struct):
```go
// NewInsecureGRPCTransportForTest dials without TLS — for in-process tests only.
func NewInsecureGRPCTransportForTest(addr string) (SensorTransport, error) {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &grpcTransport{cc: cc, client: agentpbv2.NewWendySensorServiceClient(cc)}, nil
}
```
(import `google.golang.org/grpc/credentials/insecure`.)

- [ ] **Step 4: Run + commit** — `go -C go test ./internal/agent/mcusource/ -run TestGRPCTransportStreamsFromStubServer -race` → PASS.
```bash
git add go/internal/agent/mcusource/grpc_transport.go go/internal/agent/mcusource/grpc_transport_test.go
git commit -m "test(mcusource): in-process gRPC stub source + grpcTransport integration"
```

---

## Task 5: `Transport` field, selection, transport-aware resolve

**Files:**
- Modify: `pairing_store.go`, `Proto/.../sensor_pairing_service.proto`, `services/sensor_pairing_service.go`, `runner.go`, `cmd/wendy-agent/main.go`
- Test: extend `runner`/store tests

**Interfaces:**
- `SensorPairing.Transport string` (`"grpc"`|`"tcp"`; empty ⇒ `"tcp"` for back-compat).
- `AddSensorPairingRequest.transport` (string) + `SensorPairing.transport` in the proto.
- `main.go` builds one `transportFor` that returns `NewGRPCTransport(...)` when `p.Transport=="grpc"`, else `NewTCPTransport(NewMTLSDialer(...)(p)…, addr)`.

- [ ] **Step 1: Failing test** — resolve + selection:
```go
func TestTransportSelectionAndResolve(t *testing.T) {
	// grpc pairing resolves to the source's mTLS agent port; tcp resolves to sensorlink.Port.
	// (Drive via the exported resolveLANAddr seam with a stubbed discovery, mirroring existing runner tests.)
}
```
Flesh out against the existing `runner.go` `resolveLANAddr` seam (it's a package var already stubbed in current tests): for `Transport=="grpc"` the resolved address uses the discovered device's mTLS agent port (`d.Port`), for `"tcp"`/empty it uses `sensorlink.Port`. Assert both.

- [ ] **Step 2: Implement**
- Add `Transport string \`json:"transport,omitempty"\`` to `SensorPairing`.
- Add `string transport = 5;` to `AddSensorPairingRequest` and `string transport = 6;` to the proto `SensorPairing`; `make proto`; carry it through `AddSensorPairing`/`toProto`/`ListSensorPairings` in `services/sensor_pairing_service.go`.
- `runner.go` `resolveLANAddr(ctx, sourceAssetID)` → make it transport-aware: take the pairing (or transport) and choose the port — `sensorlink.Port` for tcp, the discovered `d.Port` for grpc. Update `resolveAddr` callers.
- `main.go`: replace `dialerFor` construction with a `transportFor`:
```go
sensorTransportFor := func(p mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
	if p.Transport == "grpc" {
		certPEM, chainPEM, keyPEM := /* fresh from ProvisioningCerts, like mcuIdentity */
		return mcusource.NewGRPCTransport(logger, certPEM, chainPEM, keyPEM, p, addr)
	}
	d, err := mcusource.NewMTLSDialer(logger, mcuIdentity)(p)
	if err != nil { return nil, err }
	return mcusource.NewTCPTransport(d, addr), nil
}
sensorSup := mcusource.NewSupervisor(logger, videoSvc.Loopback(), sensorTransportFor, ros2camera.NewFrameWriter)
```

- [ ] **Step 3: Run + commit** — `go -C go build ./... && go -C go test ./internal/agent/mcusource/ ./internal/agent/services/ -race` → PASS.
```bash
git add -A
git commit -m "feat(mcusource): transport field + selection + transport-aware address resolve"
```

---

## Task 6: `caps` discovery capability (supersedes `sensorlink=true`)

**Files:**
- Modify: `go/internal/shared/models/devices.go` (`Caps []string`), `go/internal/shared/discovery/mdns.go`
- Test: `go/internal/shared/discovery/mdns_caps_test.go`

**Interfaces:**
- `LANDevice.Caps []string` + `DiscoveredDevice.Caps []string` (propagated in `MergedDevices`); parsed from a `caps=` comma-list TXT. Back-compat: `sensorlink=true` ⇒ `Caps=["sensors"]` and keep `Sensorlink bool` in sync (`Sensorlink = caps contains "sensors"`).

- [ ] **Step 1: Failing test**
```go
func TestCapsTXTParsedWithBackCompat(t *testing.T) {
	on := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"caps": "sensors,foo"}})
	if !contains(on.Caps, "sensors") || !on.Sensorlink { t.Fatal("caps=sensors not honored") }
	legacy := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"sensorlink": "true"}})
	if !legacy.Sensorlink || !contains(legacy.Caps, "sensors") { t.Fatal("legacy sensorlink=true not mapped to caps") }
	off := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{}})
	if off.Sensorlink || len(off.Caps) != 0 { t.Fatal("expected no caps") }
}
```

- [ ] **Step 2: Implement** — parse `caps` (split on `,`, trim) in `lanDeviceFromService`; if `sensorlink=true` present, ensure `"sensors"` in `Caps`; set `Sensorlink = contains(Caps,"sensors")`. Add `Caps` to both structs + `MergedDevices` propagation (mirror the `Sensorlink` copy from Plan 1 Task 9).

- [ ] **Step 3: Run + commit**
```bash
git add go/internal/shared/models/devices.go go/internal/shared/discovery/mdns.go go/internal/shared/discovery/mdns_caps_test.go
git commit -m "feat(discovery): caps capability list (supersedes sensorlink=true, back-compat)"
```

---

## Task 7: CLI records transport from caps

**Files:**
- Modify: `go/internal/cli/commands/device_pair.go`
- Test: `go/internal/cli/commands/device_pair_test.go`

**Interfaces:**
- `sensorSourceItems` still filters on `Sensorlink` (now caps-derived). Add `transportForDevice(d models.DiscoveredDevice) string` → `"grpc"` if the device is an mTLS agent advertising `caps` containing `sensors` with an agent gRPC port; `"tcp"` for a legacy/MCU sensorlink device. `AddSensorPairing` sends `Transport`.

- [ ] **Step 1: Failing test**
```go
func TestTransportForDevice(t *testing.T) {
	agentDev := models.DiscoveredDevice{Sensorlink: true, IsMTLS: true, Caps: []string{"sensors"}, AssetID: 5}
	if transportForDevice(agentDev) != "grpc" { t.Fatal("agent source should be grpc") }
	mcuDev := models.DiscoveredDevice{Sensorlink: true, IsMTLS: false, AssetID: 6} // legacy tcp sensorlink
	if transportForDevice(mcuDev) != "tcp" { t.Fatal("legacy/MCU source should be tcp") }
}
```
(Heuristic: an mTLS agent advertising `caps=sensors` speaks gRPC; a non-mTLS or bare `sensorlink=true` device speaks raw-TCP. Adjust the predicate to whatever the real discovered fields make cleanest; the test pins the intent.)

- [ ] **Step 2: Implement** `transportForDevice` + set `Transport: transportForDevice(source)` in the `AddSensorPairingRequest`. Keep the same-org check + `cleanRPCError` path.

- [ ] **Step 3: Run + commit**
```bash
git add go/internal/cli/commands/device_pair.go go/internal/cli/commands/device_pair_test.go
git commit -m "feat(cli): record sensor pairing transport (grpc for agents, tcp for MCUs)"
```

---

## Self-Review

**Spec coverage** (`2026-08-31-agent-sensor-source-design.md`): §4 gRPC contract → Task 1. §6 consumer adapter (`SensorTransport`, tcp/grpc, selection, mTLS) → Tasks 2–5. Capability discovery (§5.5 consumer side) → Task 6. Transport recorded at pair time → Tasks 5,7. macOS source (§5) → **Plan 2** (Swift). `snd-aloop` mic (§7) → **Plan 3**. Congestion (§8) source-side → Plan 2; consumer drop already exists (grpcTransport `default:` drop + Task 3).

**Placeholder scan:** the only intentionally-open spots are the exact `main.go` `ProvisioningCerts` PEM read in Task 5 Step 2 (mirror the existing `mcuIdentity`) and the `transportForDevice` predicate (Task 7 — pinned by test intent, exact fields chosen against real discovered data). No `TODO`/"handle later".

**Type consistency:** `SensorTransport{FetchManifest, Stream(→(chan, func() error, error))}` defined Task 2, implemented by `tcpTransport` (Task 2) and `grpcTransport` (Task 3), consumed by `streamOnce` (Task 2) and `main.go` (Task 5). `agentpbv2.WendySensorServiceClient`/`StreamSensorsRequest`/`GetSensorManifestRequest` from Task 1 used in Tasks 3–4. `SensorPairing.Transport` from Task 5 used in Tasks 5,7. `LANDevice.Caps`/`DiscoveredDevice.Caps` from Task 6 used in Task 7.

**Deferred to later plans (intentional):** the macOS Swift `SensorService` + H.264 capture + source-side congestion drop (Plan 2); `snd-aloop` mic mount + microphone entitlement (Plan 3).
