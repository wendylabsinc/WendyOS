package liveapp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var anchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
var frame = world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}

type fixtureClock struct{ now time.Time }

func (c fixtureClock) Now() (trustedtime.Interval, error) {
	return trustedtime.Interval{Earliest: c.now, Latest: c.now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only"}, nil
}

func imageBytes(t *testing.T, large bool) []byte {
	t.Helper()
	w, h := 4, 3
	if large {
		w, h = 160, 160
	}
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	var random uint32 = 42
	for i := range im.Pix {
		random = random*1664525 + 1013904223
		im.Pix[i] = byte(random >> 24)
	}
	for i := 3; i < len(im.Pix); i += 4 {
		im.Pix[i] = 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func audioBytes() []byte {
	data := make([]byte, 44+4000)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 8000)
	binary.LittleEndian.PutUint32(data[28:], 16000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], 4000)
	return data
}
func capture(t *testing.T, sensor string, sequence uint64, large bool) (serviceorigin.PublishRequest, []byte) {
	t.Helper()
	s := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("run/%s/%d", sensor, sequence), SiteID: "office", Frame: frame, SensorID: sensor, Sequence: sequence, SampleNS: int64(sequence) * int64(time.Second), RunID: "run", ScenarioVersion: "mujoco4-test", Seed: 1, PackageSHA256: strings.Repeat("a", 64), SensorModel: "test-model", PositionM: []float64{-2, .2, 0}, UncertaintyM: .01, CalibrationRevision: "cal-1"}
	var media []byte
	switch sensor {
	case "gas-1":
		s.Kind = "gas"
		s.Gas = &sensing.Gas{ValuePPM: 60, UncertaintyPPM: 1, Species: "test"}
	case "audio-1":
		s.Kind = "audio"
		s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
		media = audioBytes()
		s.Evidence = &sensing.EvidenceRef{SHA256: hash(media), Size: int64(len(media)), ContentType: "audio/wav"}
	default:
		s.Kind = "camera"
		media = imageBytes(t, large)
		w, h := 4, 3
		if large {
			w, h = 160, 160
		}
		s.Camera = &sensing.Camera{Width: w, Height: h, FX: float64(w), FY: float64(h), CX: float64(w) / 2, CY: float64(h) / 2, CameraToSite: []float64{1, 0, 0, -2, 0, 1, 0, .2, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
		s.Evidence = &sensing.EvidenceRef{SHA256: hash(media), Size: int64(len(media)), ContentType: "image/png"}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	p, err := sensorrecord.Publication(sensorrecord.Capture{Version: 1, SceneBootID: "scene-boot", CapturedUnixNS: time.Now().UnixMilli() * int64(time.Millisecond), SimulationAnchorUnixNS: anchor.UnixNano(), SampleJSON: raw}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return p, media
}

// This is a local test RPC server with generated untrusted signing material.
// It tests the app protocol, not agent authorization or journal durability.
type rpcFixture struct {
	appv1.UnimplementedWorldAppServiceServer
	mu        sync.Mutex
	publisher *serviceorigin.Publisher
	puts      []*appv1.PutEvidenceRequest
	requests  []*appv1.PublishRequest
	proof     []byte
	mode      string
	until     int64
}

func (s *rpcFixture) PutEvidence(ctx context.Context, r *appv1.PutEvidenceRequest) (*appv1.PutEvidenceReply, error) {
	s.mu.Lock()
	s.puts = append(s.puts, proto.Clone(r).(*appv1.PutEvidenceRequest))
	mode := s.mode
	s.mu.Unlock()
	if mode == "upload-timeout" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if mode == "upload-rejected" {
		return nil, status.Error(codes.PermissionDenied, "refused")
	}
	ack := &appv1.PutEvidenceReply{Sha256: r.Sha256, Size: r.Size, Received: r.Offset + int64(len(r.Data)), Complete: r.Offset+int64(len(r.Data)) == r.Size, ContentType: r.ContentType, RetainUntilUnixMs: s.until}
	if mode == "wrong-offset" {
		ack.Received++
	}
	if mode == "wrong-digest" {
		ack.Sha256 = strings.Repeat("b", 64)
	}
	if mode == "wrong-type" {
		ack.ContentType = "audio/wav"
	}
	if mode == "premature-complete" {
		ack.Complete = true
	}
	if mode == "already-complete" {
		ack.Complete = true
		ack.Received = r.Size
	}
	if mode == "renewed-retention" && r.Offset > 0 {
		ack.RetainUntilUnixMs++
	}
	return ack, nil
}
func (s *rpcFixture) Publish(ctx context.Context, r *appv1.PublishRequest) (*appv1.PublishReply, error) {
	s.mu.Lock()
	s.requests = append(s.requests, proto.Clone(r).(*appv1.PublishRequest))
	mode := s.mode
	s.mu.Unlock()
	if mode == "publish-timeout" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if mode == "publish-rejected" {
		return nil, status.Error(codes.PermissionDenied, "refused")
	}
	p, err := appv1.DecodePublish(r)
	if err != nil {
		return nil, err
	}
	if mode == "changed-payload" {
		p.Payload = append(p.Payload, ' ')
	}
	signed, err := s.publisher.Publish(ctx, p)
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(signed, "", " ")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.proof = bytes.Clone(raw)
	s.mu.Unlock()
	return &appv1.PublishReply{Accepted: mode != "unaccepted", SignedRecordJson: raw}, nil
}
func rpcClient(t *testing.T, sensor, mode string) (appv1.WorldAppServiceClient, *rpcFixture) {
	t.Helper()
	now := time.Now().UTC()
	id := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.example.capture", Service: "sensor", DeploymentGeneration: 7, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: sensor}}}, Epoch: 31, Lifetime: time.Minute, Clock: fixtureClock{now}, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	dir, err := os.MkdirTemp("", "ww-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "world.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	fixture := &rpcFixture{publisher: p, mode: mode, until: now.Add(time.Hour).UnixMilli()}
	appv1.RegisterWorldAppServiceServer(server, fixture)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return appv1.NewWorldAppServiceClient(conn), fixture
}

func TestPublisherUploadsChunksThenPreservesOriginalAcknowledgment(t *testing.T) {
	for _, sensor := range []string{"camera-a", "audio-1", "gas-1"} {
		t.Run(sensor, func(t *testing.T) {
			client, server := rpcClient(t, sensor, "")
			p, err := NewPublisher(client, PublisherConfig{})
			if err != nil {
				t.Fatal(err)
			}
			req, media := capture(t, sensor, 17, true)
			out, err := p.Submit(context.Background(), req, media)
			if err != nil {
				t.Fatal(err)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			var uploaded []byte
			for i, r := range server.puts {
				if r.Offset != int64(len(uploaded)) || len(r.Data) > serviceevidence.MaxChunk || r.Stream != sensor || r.ComponentType != sensorrecord.ComponentType || (i > 0 && r.RetainUntilUnixMs != server.until) {
					t.Fatal("chunk scope/offset/retention changed")
				}
				uploaded = append(uploaded, r.Data...)
			}
			if !bytes.Equal(uploaded, media) || len(server.requests) != 1 || !bytes.Equal(out.SignedRecordJSON, server.proof) || out.NativeSequence != 17 || out.CaptureID != "run/"+sensor+"/17" || out.ProofSHA256 != hash(server.proof) {
				t.Fatal("capture bytes, identity or exact proof changed")
			}
			if sensor == "gas-1" && len(server.puts) != 0 {
				t.Fatal("gas uploaded media")
			}
			if sensor == "camera-a" && len(server.puts) < 2 {
				t.Fatal("fixture did not exercise chunking")
			}
		})
	}
}

func TestPublisherRejectsAmbiguousMutationsWithoutRetry(t *testing.T) {
	for _, mode := range []string{"upload-rejected", "wrong-offset", "wrong-digest", "wrong-type", "premature-complete", "renewed-retention", "upload-timeout", "publish-timeout", "publish-rejected", "unaccepted", "changed-payload"} {
		t.Run(mode, func(t *testing.T) {
			client, server := rpcClient(t, "camera-a", mode)
			p, _ := NewPublisher(client, PublisherConfig{RequestTimeout: 30 * time.Millisecond})
			req, media := capture(t, "camera-a", 1, true)
			out, err := p.Submit(context.Background(), req, media)
			if !errors.Is(err, ErrUncertain) || len(out.SignedRecordJSON) != 0 {
				t.Fatalf("ambiguous result accepted: %v", err)
			}
			server.mu.Lock()
			puts, publications := len(server.puts), len(server.requests)
			server.mu.Unlock()
			if _, err = p.Submit(context.Background(), req, media); !errors.Is(err, ErrUncertain) {
				t.Fatal("faulted publisher retried")
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if len(server.puts) != puts || len(server.requests) != publications || publications > 1 {
				t.Fatal("ambiguous mutation automatically retried")
			}
		})
	}
}

func TestPublisherRejectsDuplicateRegressionAndBootChange(t *testing.T) {
	client, server := rpcClient(t, "gas-1", "")
	p, _ := NewPublisher(client, PublisherConfig{})
	req, media := capture(t, "gas-1", 17, false)
	if _, err := p.Submit(context.Background(), req, media); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Submit(context.Background(), req, media); !errors.Is(err, ErrDuplicate) {
		t.Fatal("duplicate was not skipped", err)
	}
	for _, mode := range []string{"regressed", "changed-sequence", "boot", "anchor"} {
		bad := req
		c, _ := sensorrecord.Decode(req.Payload)
		s, _ := c.Sample()
		switch mode {
		case "regressed":
			s.Sequence--
		case "changed-sequence":
			s.Gas.ValuePPM++
		case "boot":
			c.SceneBootID = "new-boot"
		case "anchor":
			c.SimulationAnchorUnixNS++
		}
		c.SampleJSON, _ = json.Marshal(s)
		bad, _ = sensorrecord.Publication(c, req.TTL)
		if _, err := p.Submit(context.Background(), bad, media); !errors.Is(err, ErrCapture) {
			t.Fatal("capture ordering accepted", mode, err)
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) != 1 {
		t.Fatal("duplicate/regression reached agent")
	}
}

func TestPublisherReusesOnlyExplicitCompleteOwnerDigestAcknowledgment(t *testing.T) {
	client, server := rpcClient(t, "camera-a", "already-complete")
	p, _ := NewPublisher(client, PublisherConfig{})
	req, media := capture(t, "camera-a", 1, true)
	if _, err := p.Submit(context.Background(), req, media); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.puts) != 1 || len(server.requests) != 1 || len(media) <= serviceevidence.MaxChunk {
		t.Fatal("complete evidence acknowledgment did not avoid redundant transfer")
	}
}

func snapshot(t *testing.T, seq uint64) livecapture.Snapshot {
	t.Helper()
	w := time.Now().UnixMilli()
	s := livecapture.Snapshot{SchemaVersion: livecapture.Schema, RunID: "run", BootID: "scene-boot", Sequence: seq, SampleUnixMS: w, SimulationTimeNS: int64(seq) * int64(time.Second), Observations: []sensing.Sample{}, Media: []livecapture.Media{}}
	s.State = livecapture.State{SchemaVersion: "wendy.sim.actuator.v0", Synthetic: true, BootID: s.BootID, Session: "test", SiteID: "office", Frame: frame, Position: [3]float64{-2, .2, 0}, SampleUnixMS: w, SimulationTimeS: float64(seq), Sensors: map[string]bool{"gas": true, "local-obstacles": true}, ObstacleClear: true, Energy: 1, WatchdogStopped: true, Provenance: livecapture.Provenance{Mode: "simulated", PackageSHA256: strings.Repeat("a", 64), RobotModel: "test", GasModel: "test"}}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		p, data := capture(t, id, seq, false)
		c, _ := sensorrecord.Decode(p.Payload)
		sample, _ := c.Sample()
		if id == "camera-b" {
			sample.PositionM[1] = 2
			sample.PositionM[2] = 2
			sample.Camera.CameraToSite[7] = 2
			sample.Camera.CameraToSite[11] = 2
		}
		s.Observations = append(s.Observations, sample)
		if sample.Evidence != nil {
			already := false
			for _, m := range s.Media {
				already = already || m.SHA256 == sample.Evidence.SHA256
			}
			if !already {
				s.Media = append(s.Media, livecapture.Media{SHA256: sample.Evidence.SHA256, Data: data})
			}
		}
	}
	return s
}
func runConfig(t *testing.T, url string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "read-token")
	if err := os.WriteFile(path, []byte(strings.Repeat("r", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{SceneURL: url, ReadTokenFile: path, RunID: "run", Site: "office", Frame: frame, SensorID: "gas-1", SimulationAnchorUnixNS: anchor.UnixNano(), DurationMS: 4000, MaxSamples: 2, PollIntervalMS: 1000}
}
func TestRunRealHTTPAndUnixGRPCPreserveChangingCaptures(t *testing.T) {
	var calls atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/samples" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("r", 32) {
			t.Error("request escaped read-only capture endpoint")
		}
		_ = json.NewEncoder(w).Encode(snapshot(t, calls.Add(1)))
	}))
	defer server.Close()
	api, rpc := rpcClient(t, "gas-1", "")
	out, err := Run(context.Background(), runConfig(t, server.URL), api)
	if err != nil || out.Status != "complete" || len(out.Receipts) != 2 || out.Receipts[0].NativeSequence != 1 || out.Receipts[1].NativeSequence != 2 || out.Receipts[0].CapturedUnixNS >= out.Receipts[1].CapturedUnixNS {
		t.Fatal("live run did not preserve changing original captures", err)
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if len(rpc.requests) != 2 || len(rpc.puts) != 0 {
		t.Fatal("unexpected mutations")
	}
}
func TestRunDuplicateBecomesStaleWithoutRepublishing(t *testing.T) {
	s := snapshot(t, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(s) }))
	defer server.Close()
	api, rpc := rpcClient(t, "gas-1", "")
	out, err := Run(context.Background(), runConfig(t, server.URL), api)
	if !errors.Is(err, livecapture.ErrStale) || out.ErrorCode != "capture_stale" || len(out.Receipts) != 1 {
		t.Fatal("duplicate age renewed", err)
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if len(rpc.requests) != 1 {
		t.Fatal("stale duplicate republished")
	}
}
func TestConfigurationRefusesUnknownFieldsTokenLeaksAndBounds(t *testing.T) {
	cfg := runConfig(t, "http://127.0.0.1:1")
	for _, change := range []func(*Config){func(c *Config) { c.MaxSamples = 65 }, func(c *Config) { c.PollIntervalMS = 49 }, func(c *Config) { c.DurationMS = 600001 }, func(c *Config) { c.SensorID = "actuator" }, func(c *Config) { c.SimulationAnchorUnixNS = 0 }} {
		bad := cfg
		change(&bad)
		if _, err := bad.normalized(); err == nil {
			t.Fatal("unbounded configuration accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(cfg)
	raw = append(raw[:len(raw)-1], []byte(`,"actuator_token":"secret"}`)...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("actuator credential field accepted")
	}
	api, _ := rpcClient(t, "gas-1", "")
	out, err := Run(context.Background(), cfg, api)
	raw, _ = json.Marshal(out)
	if err == nil || bytes.Contains(raw, []byte(strings.Repeat("r", 32))) || bytes.Contains(raw, []byte(cfg.ReadTokenFile)) || bytes.Contains(raw, []byte(cfg.SceneURL)) {
		t.Fatal("failed run exposed token or private configuration")
	}
}
