package proofreader

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type signingClock struct{ now time.Time }

func (c signingClock) Now() (trustedtime.Interval, error) {
	return trustedtime.Interval{Earliest: c.now, Latest: c.now.Add(time.Millisecond), Domain: "physical", Authenticated: true, BootID: "fixture", Monotonic: time.Second, Source: "test-time"}, nil
}

type archiveServer struct {
	appv1.UnimplementedWorldAppServiceServer
	mu                                 sync.Mutex
	cfg                                Config
	now, base                          time.Time
	until                              int64
	mode                               string
	proofCalls, mediaCalls, queryCalls int
	blobs                              map[string][]byte
	newer                              []byte
}

func (s *archiveServer) Clock(_ context.Context, r *appv1.ClockRequest) (*appv1.ClockReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	boot := strings.Repeat("a", 32)
	generation := s.cfg.ExpectedGeneration
	if s.mode == "clock-restart" && s.proofCalls > 0 {
		boot = strings.Repeat("b", 32)
	}
	if s.mode == "scope-replaced" && s.proofCalls > 0 {
		generation++
	}
	return &appv1.ClockReply{Nonce: bytes.Clone(r.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: s.now.UnixNano(), LatestUnixNs: s.now.Add(time.Millisecond).UnixNano(), BootId: "fixture-host", MonotonicNs: int64(time.Second + s.now.Sub(s.base)), Source: "test-authenticated"}, SampleClockDomain: "simulation:" + s.cfg.RunID, SampleUnixNs: time.Unix(1700000000, 0).Add(time.Second).UnixNano(), Tenant: s.cfg.Tenant, Site: s.cfg.Site, Device: s.cfg.Device, App: s.cfg.App, Service: s.cfg.Service, ExecutionDomain: "simulation", DeploymentGeneration: generation, ServerBootId: boot}, nil
}
func (s *archiveServer) PhysicalClock(ctx context.Context, r *appv1.ClockRequest) (*appv1.PhysicalClockReply, error) {
	v, err := s.Clock(ctx, r)
	if err != nil {
		return nil, err
	}
	return &appv1.PhysicalClockReply{Nonce: bytes.Clone(v.Nonce), Physical: v.Physical, Tenant: v.Tenant, Site: v.Site, Device: v.Device, App: v.App, Service: v.Service, ExecutionDomain: v.ExecutionDomain, DeploymentGeneration: v.DeploymentGeneration, ServerBootId: v.ServerBootId}, nil
}
func (s *archiveServer) GetRecordProof(_ context.Context, r *appv1.GetRecordProofRequest) (*appv1.GetRecordProofReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proofCalls++
	if s.mode == "proof-denied" {
		return nil, status.Error(codes.PermissionDenied, "reader revoked")
	}
	for _, e := range s.cfg.Expected {
		ref := e.Reference
		if r.EnvelopeSha256 != ref.EnvelopeSHA256 {
			continue
		}
		if r.Source == nil || !reflect.DeepEqual(r.Source, selector(source(ref))) || r.Entity != ref.Key.Entity || r.Epoch != ref.Version.Epoch || r.Sequence != ref.Version.Sequence || r.RecordSha256 != ref.RecordSHA256 {
			return nil, status.Error(codes.InvalidArgument, "wrong exact reference")
		}
		out := &appv1.GetRecordProofReply{SignedRecordJson: bytes.Clone(e.EnvelopeJSON), RetainUntilUnixMs: s.until, Historical: true}
		switch s.mode {
		case "proof-forgery":
			out.SignedRecordJson[0] ^= 1
		case "not-historical":
			out.Historical = false
		case "proof-expired":
			out.RetainUntilUnixMs = s.now.UnixMilli()
		case "proof-unknown":
			out.ProtoReflect().SetUnknown([]byte{0xa0, 6, 1})
		}
		return out, nil
	}
	return nil, status.Error(codes.NotFound, "exact proof absent")
}
func (s *archiveServer) GetEvidence(_ context.Context, r *appv1.GetEvidenceRequest) (*appv1.GetEvidenceReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mediaCalls++
	if r.Source == nil {
		return nil, status.Error(codes.PermissionDenied, "no owner")
	}
	data, ok := s.blobs[r.Source.Stream+"/"+r.Sha256]
	if !ok || r.Offset < 0 || r.Length < 1 || r.Offset+int64(r.Length) > int64(len(data)) {
		return nil, status.Error(codes.NotFound, "wrong owner/range")
	}
	mime := "image/png"
	if r.Source.Stream == "camera-b" {
		mime = "image/jpeg"
	}
	if r.Source.Stream == "audio-1" {
		mime = "audio/wav"
	}
	out := &appv1.GetEvidenceReply{Sha256: r.Sha256, Size: int64(len(data)), Offset: r.Offset, Data: bytes.Clone(data[r.Offset : r.Offset+int64(r.Length)]), ContentType: mime, RetainUntilUnixMs: s.until}
	switch s.mode {
	case "media-forgery":
		out.Data[0] ^= 1
	case "media-offset":
		out.Offset++
	case "media-expired":
		out.RetainUntilUnixMs = s.now.UnixMilli()
	case "media-unknown":
		out.ProtoReflect().SetUnknown([]byte{0xa0, 6, 1})
	case "retention-during-media":
		s.now = s.now.Add(time.Minute)
	}
	return out, nil
}
func (s *archiveServer) Query(_ context.Context, r *appv1.QueryRequest) (*appv1.QueryReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queryCalls++
	if len(r.Sources) != 5 || r.Limit != 5 || r.After != "" {
		return nil, status.Error(codes.InvalidArgument, "bounded exact sources required")
	}
	seen := map[string]bool{}
	for _, source := range r.Sources {
		if source == nil || seen[source.Stream] {
			return nil, status.Error(codes.InvalidArgument, "bad selector")
		}
		seen[source.Stream] = true
	}
	out := &appv1.QueryReply{}
	switch s.mode {
	case "still-live":
		out.SignedRecordJson = [][]byte{s.cfg.Expected[0].EnvelopeJSON}
	case "newer":
		out.SignedRecordJson = [][]byte{s.newer}
	case "query-cursor":
		out.Next = "unexplained"
	case "query-error":
		return nil, status.Error(codes.Unavailable, "query unavailable")
	case "query-unknown":
		out.ProtoReflect().SetUnknown([]byte{0xa0, 6, 1})
	case "retention-during-query":
		s.now = s.now.Add(time.Minute)
	}
	return out, nil
}
func pngBytes(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	var seed uint32 = 7
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			seed = seed*1664525 + 1013904223
			im.Set(x, y, color.NRGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	if b.Len() <= 48<<10 {
		t.Fatal("need multichunk PNG")
	}
	return b.Bytes()
}
func wavBytes() []byte {
	const n = 2000
	data := make([]byte, 44+n*2)
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
	binary.LittleEndian.PutUint32(data[40:], n*2)
	return data
}
func setupReader(t *testing.T, mode string) (*archiveServer, appv1.WorldAppServiceClient) {
	t.Helper()
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, base, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{Version: 1, Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.example.multimodal", Service: "audit-reader", ExpectedGeneration: 1<<63 + 9, RunID: "run", DurationMS: 5000}
	s := &archiveServer{cfg: cfg, mode: mode, base: base, now: base.Add(6 * time.Second), until: base.Add(time.Minute).UnixMilli(), blobs: map[string][]byte{}}
	if mode == "expired-signing-certificate" {
		s.now = base.Add(2 * time.Hour)
		s.until = base.Add(3 * time.Hour).UnixMilli()
	}
	if mode == "capture-not-expired" {
		s.now = base.Add(4 * time.Second)
		s.cfg.DurationMS = 100
	}
	publish := func(service string, p serviceorigin.PublishRequest) (ExpectedProof, *serviceorigin.Publisher) {
		pub, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: cfg.Tenant, Site: cfg.Site, Device: cfg.Device, App: cfg.App, Service: service, DeploymentGeneration: 5, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: p.ComponentType, Stream: p.Stream}}}, Epoch: 31, Lifetime: 10 * time.Minute, Clock: signingClock{base}, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pub.Close() })
		signed, err := pub.Publish(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(signed)
		_, ref, err := decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		return ExpectedProof{Reference: ref, EnvelopeJSON: raw}, pub
	}
	anchor := time.Unix(1700000000, 0).UTC()
	frame := world.FrameRef{ID: "floor", Revision: "v1"}
	var refs []proofref.Reference
	var leaves []world.Input
	for _, sensor := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/" + sensor + "/17", SiteID: cfg.Site, Frame: frame, SensorID: sensor, Sequence: 17, SampleNS: int64(250 * time.Millisecond), RunID: "run", ScenarioVersion: "v1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "test-fixture", PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "cal-v1"}
		var media []byte
		mime := ""
		switch sensor {
		case "camera-a", "camera-b":
			sample.Kind = "camera"
			sample.Camera = &sensing.Camera{Width: 256, Height: 256, FX: 256, FY: 256, CX: 128, CY: 128, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
			media = pngBytes(t)
			mime = "image/png"
		case "gas-1":
			sample.Kind = "gas"
			sample.Gas = &sensing.Gas{ValuePPM: 80, UncertaintyPPM: 2, Species: "test-gas"}
		case "audio-1":
			sample.Kind = "audio"
			sample.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
			media = wavBytes()
			mime = "audio/wav"
		}
		if media != nil {
			sample.Evidence = &sensing.EvidenceRef{SHA256: digest(media), Size: int64(len(media)), ContentType: mime}
		}
		sampleJSON, _ := json.MarshalIndent(sample, "", "  ")
		capture := sensorrecord.Capture{Version: 1, SceneBootID: "scene-boot", CapturedUnixNS: base.UnixNano(), SimulationAnchorUnixNS: anchor.UnixNano(), SampleJSON: sampleJSON}
		if sensor == "camera-b" {
			im, err := png.Decode(bytes.NewReader(media))
			if err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			if err = jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); err != nil {
				t.Fatal(err)
			}
			media = b.Bytes()
			capture, err = sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{ChannelID: 4, Seq: 9, TsUs: uint64(base.UnixMicro()), Flags: 1, Payload: media})
			if err != nil {
				t.Fatal(err)
			}
		}
		if media != nil {
			s.blobs[sensor+"/"+digest(media)] = media
		}
		p, err := sensorrecord.Publication(capture, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		e, pub := publish(sensor, p)
		s.cfg.Expected = append(s.cfg.Expected, e)
		refs = append(refs, e.Reference)
		leaves = append(leaves, world.Input{ID: e.Reference.ID(), Origin: world.Simulated, Simulation: p.Provenance.Simulation})
		if sensor == "gas-1" {
			next, err := pub.Publish(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			s.newer, _ = json.Marshal(next)
		}
	}
	payload, err := resultrecord.Encode(resultrecord.Result{Version: 1, ResultSchema: "example.hazard.v1", ResultJSON: []byte("\n {\"label\": \"test hazard\", \"confidence\":0.75} \n"), Inputs: refs, RetainUntilUnixMS: base.Add(3 * time.Hour).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	request := serviceorigin.PublishRequest{Entity: "run/hazard", ComponentType: resultrecord.ComponentType, Stream: "hazard", Frame: frame, Timing: world.Timing{SampleTime: anchor, ClockDomain: "simulation:run", ErrorKnown: true}, TTL: 2 * time.Second, Payload: payload, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "test-v1", ModelID: "test-rule", ModelVersion: "1", Inputs: leaves}}
	expected, _ := publish("fusion", request)
	s.cfg.Expected = append(s.cfg.Expected, expected)
	dir, err := os.MkdirTemp("", "proof-reader-")
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
	appv1.RegisterWorldAppServiceServer(server, s)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := liveapp.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return s, appv1.NewWorldAppServiceClient(conn)
}
func TestHistoricalReaderKeepsExactBytesAfterCaptureExpiry(t *testing.T) {
	for _, mode := range []string{"complete", "newer", "expired-signing-certificate"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setupReader(t, mode)
			out, err := Run(context.Background(), s.cfg, api)
			if err != nil || out.Status != "complete" || !out.PhysicalCaptureExpired || !out.OriginalVersionsAbsent || out.CurrentSourceAuthority || len(out.Proofs) != 5 || len(out.Media) != 3 {
				t.Fatalf("historical read failed: status=%s proofs=%d media=%d err=%v", out.Status, len(out.Proofs), len(out.Media), err)
			}
			if s.mediaCalls <= 3 {
				t.Fatal("multi-chunk media was not read by ranges")
			}
			for i, p := range out.Proofs {
				if !p.Historical || p.Reference != s.cfg.Expected[i].Reference || !bytes.Equal(p.EnvelopeJSON, s.cfg.Expected[i].EnvelopeJSON) {
					t.Fatal("historical proof changed")
				}
			}
			for _, m := range out.Media {
				if !bytes.Equal(m.Data, s.blobs[m.Source.Stream+"/"+m.Reference.SHA256]) {
					t.Fatal("media changed")
				}
			}
		})
	}
}
func TestHistoricalReaderRefusesUnknownExpiredAndChangedData(t *testing.T) {
	for _, mode := range []string{"proof-denied", "proof-forgery", "not-historical", "proof-expired", "proof-unknown", "clock-restart", "scope-replaced", "media-forgery", "media-offset", "media-expired", "media-unknown", "retention-during-media", "still-live", "query-cursor", "query-error", "query-unknown", "retention-during-query", "capture-not-expired"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setupReader(t, mode)
			out, err := Run(context.Background(), s.cfg, api)
			if err == nil || out.Status == "complete" || out.Error == "" {
				t.Fatalf("refused case succeeded: %s %v", mode, err)
			}
			if mode == "capture-not-expired" && s.proofCalls != 0 {
				t.Fatal("read claimed expiry before original physical capture bound")
			}
		})
	}
}
func TestExpectedProofConfigIsExactOwnedAndBounded(t *testing.T) {
	s, _ := setupReader(t, "complete")
	original := s.cfg
	for name, change := range map[string]func(*Config){"generation": func(c *Config) { c.ExpectedGeneration = 0 }, "missing": func(c *Config) { c.Expected = c.Expected[:4] }, "hash": func(c *Config) { c.Expected[0].Reference.RecordSHA256 = strings.Repeat("b", 64) }, "bytes": func(c *Config) { c.Expected[0].EnvelopeJSON[0] ^= 1 }, "tenant": func(c *Config) { c.Tenant = "another" }, "run": func(c *Config) { c.RunID = "another" }, "self": func(c *Config) { c.Service = "fusion" }} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(original)
			var cfg Config
			_ = json.Unmarshal(b, &cfg)
			change(&cfg)
			if _, err := prepare(cfg); err == nil {
				t.Fatal("bad expected configuration accepted")
			}
		})
	}
	p, err := prepare(original)
	if err != nil {
		t.Fatal(err)
	}
	original.Expected[0].EnvelopeJSON[0] ^= 1
	if p.cfg.Expected[0].EnvelopeJSON[0] == original.Expected[0].EnvelopeJSON[0] {
		t.Fatal("caller mutation changed prepared expected bytes")
	}
}

func TestLoadCommissionedConfigRejectsLinksWritesAndUnknownFields(t *testing.T) {
	s, _ := setupReader(t, "complete")
	path := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || len(got.Expected) != 5 {
		t.Fatal("valid embedded expected proofs refused", err)
	}
	link := path + ".link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(link); err == nil {
		t.Fatal("symlink config accepted")
	}
	if err = os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(path); err == nil {
		t.Fatal("unprotected config accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{append([]byte(`{"unknown":1,`), raw[1:]...), append([]byte(`{"version":1,`), raw[1:]...), bytes.Repeat([]byte(" "), MaxConfigBytes+1)} {
		if err = os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadConfig(path); err == nil {
			t.Fatal("invalid or oversized config accepted")
		}
	}
}
