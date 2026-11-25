package livefusion

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testClock struct {
	mu     sync.Mutex
	window trustedtime.Interval
}

func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window, nil
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.Earliest = c.window.Earliest.Add(d)
	c.window.Latest = c.window.Latest.Add(d)
	c.window.Monotonic += d
}

type fixtureServer struct {
	appv1.UnimplementedWorldAppServiceServer
	mu                             sync.Mutex
	cfg                            Config
	clock                          *testClock
	proofs                         map[string][]byte
	original                       map[string][]byte
	media                          map[string][]byte
	owners                         map[string]int
	claim                          *serviceorigin.Publisher
	publishers                     map[string]*serviceorigin.Publisher
	replacement, newer             []byte
	mode                           string
	mediaSeen                      bool
	published                      int
	queries                        int
	faultQuery                     int
	faultKind                      string
	clockGeneration                uint64
	sampleAdvance                  time.Duration
	clocks                         int
	combinedClocks, physicalClocks int
	publishHook                    func(context.Context, *appv1.PublishReply) (*appv1.PublishReply, error)
	omitted                        map[string]bool
	omitAfterMedia                 map[string]bool
}

func (s *fixtureServer) Clock(_ context.Context, p *appv1.ClockRequest) (*appv1.ClockReply, error) {
	s.mu.Lock()
	s.clocks++
	s.combinedClocks++
	generation := s.clockGeneration
	sampleAdvance := s.sampleAdvance
	s.mu.Unlock()
	if generation == 0 {
		generation = 7
	}
	w, _ := s.clock.Now()
	return &appv1.ClockReply{Nonce: bytes.Clone(p.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, SampleClockDomain: "simulation:" + s.cfg.RunID, SampleUnixNs: s.cfg.SimulationAnchorUnixNS + int64(500*time.Millisecond+sampleAdvance), Tenant: s.cfg.Tenant, Site: s.cfg.Site, Device: s.cfg.Device, App: s.cfg.App, Service: s.cfg.Service, ExecutionDomain: "simulation", DeploymentGeneration: generation, ServerBootId: strings.Repeat("a", 32)}, nil
}
func (s *fixtureServer) PhysicalClock(_ context.Context, p *appv1.ClockRequest) (*appv1.PhysicalClockReply, error) {
	s.mu.Lock()
	s.clocks++
	s.physicalClocks++
	generation := s.clockGeneration
	s.mu.Unlock()
	if generation == 0 {
		generation = 7
	}
	w, _ := s.clock.Now()
	return &appv1.PhysicalClockReply{Nonce: bytes.Clone(p.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, Tenant: s.cfg.Tenant, Site: s.cfg.Site, Device: s.cfg.Device, App: s.cfg.App, Service: s.cfg.Service, ExecutionDomain: "simulation", DeploymentGeneration: generation, ServerBootId: strings.Repeat("a", 32)}, nil
}
func (s *fixtureServer) Query(_ context.Context, p *appv1.QueryRequest) (*appv1.QueryReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries++
	out := &appv1.QueryReply{}
	if p.After != "" || p.Limit != uint32(len(p.Sources)) || len(p.Sources) != len(s.cfg.Sources) {
		return nil, status.Error(codes.InvalidArgument, "bad query")
	}
	fault := s.queries == s.faultQuery
	if fault {
		switch s.faultKind {
		case "expiry":
			s.clock.advance(6 * time.Second)
		case "sample-expiry":
			s.sampleAdvance = 3 * time.Second
		case "reader-replaced":
			s.clockGeneration = 8
		case "rpc":
			return nil, status.Error(codes.PermissionDenied, "reader removed")
		}
	}
	for _, source := range p.Sources {
		if s.omitted[source.Stream] || s.mediaSeen && s.omitAfterMedia[source.Stream] {
			continue
		}
		if fault && s.faultKind == "missing" && source.Stream == "gas-1" {
			continue
		}
		found := false
		for _, allowed := range s.cfg.Sources {
			device := allowed.Device
			if device == "" {
				device = s.cfg.Device
			}
			if source.Device == device && source.App == allowed.App && source.Service == allowed.Service && source.ComponentType == sensorrecord.ComponentType && source.Stream == allowed.SensorID {
				found = true
			}
		}
		if !found {
			return nil, status.Error(codes.PermissionDenied, "foreign source")
		}
		if s.mode == "missing" && source.Stream == "gas-1" || s.mode == "revoke" && s.mediaSeen && source.Stream == "camera-a" {
			continue
		}
		raw := s.proofs[source.Stream]
		if fault && s.faultKind == "replacement" && source.Stream == "camera-a" {
			raw = s.replacement
		}
		if s.mode == "skew-then-forged" && s.queries == 1 && source.Stream == "gas-1" {
			raw = s.replacement
		}
		if s.mediaSeen && s.mode == "replace" && source.Stream == "camera-a" {
			raw = s.replacement
		}
		if s.mediaSeen && s.mode == "newer" && source.Stream == "gas-1" {
			raw = s.newer
		}
		if s.mode == "forged" && source.Stream == "camera-a" {
			var proof serviceorigin.SignedRecord
			_ = json.Unmarshal(raw, &proof)
			proof.Signature[0] ^= 1
			raw, _ = json.Marshal(proof)
		}
		out.SignedRecordJson = append(out.SignedRecordJson, bytes.Clone(raw))
	}
	if s.mode == "cursor" || fault && s.faultKind == "cursor" {
		out.Next = "unexplained"
	}
	return out, nil
}
func (s *fixtureServer) GetEvidence(_ context.Context, p *appv1.GetEvidenceRequest) (*appv1.GetEvidenceReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Source == nil {
		return nil, status.Error(codes.PermissionDenied, "owner required")
	}
	allowed := false
	for _, source := range s.cfg.Sources {
		resolved, err := source.selector().Resolve(s.cfg.Device)
		if err == nil && p.Source.Device == resolved.Device && p.Source.App == source.App && p.Source.Service == source.Service && p.Source.Stream == source.SensorID && p.Source.ComponentType == sensorrecord.ComponentType {
			allowed = true
		}
	}
	key := p.Source.Stream + "/" + p.Sha256
	data, exists := s.media[key]
	if !allowed || !exists || p.Offset < 0 || p.Length < 1 || p.Offset+int64(p.Length) > int64(len(data)) {
		return nil, status.Error(codes.PermissionDenied, "exact media absent")
	}
	s.owners[p.Source.Stream]++
	s.mediaSeen = true
	if s.mode == "expiry" {
		s.clock.advance(6 * time.Second)
	}
	content := "image/png"
	if p.Source.Stream == "camera-b" && s.mode != "lidar" {
		content = "image/jpeg"
	}
	if p.Source.Stream == "audio-1" {
		content = "audio/wav"
	}
	w, _ := s.clock.Now()
	body := bytes.Clone(data[p.Offset : p.Offset+int64(p.Length)])
	if s.mode == "media-forgery" {
		body[len(body)-1] ^= 1
	}
	offset := p.Offset
	if s.mode == "media-offset" {
		offset++
	}
	return &appv1.GetEvidenceReply{Sha256: p.Sha256, Size: int64(len(data)), Offset: offset, Data: body, ContentType: content, RetainUntilUnixMs: w.Earliest.Add(time.Hour).UnixMilli()}, nil
}
func (s *fixtureServer) Publish(ctx context.Context, p *appv1.PublishRequest) (*appv1.PublishReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published++
	request, err := appv1.DecodePublish(p)
	if err != nil {
		return nil, err
	}
	if s.mode == "wrong-ack" {
		request.Entity = "different-result"
	}
	proof, err := s.claim.Publish(ctx, request)
	if err != nil {
		return nil, err
	}
	if s.mode == "uncertain" {
		return nil, status.Error(codes.Unavailable, "commit response lost")
	}
	raw, _ := json.Marshal(proof)
	ack := &appv1.PublishReply{Accepted: true, SignedRecordJson: raw}
	if s.publishHook != nil {
		return s.publishHook(ctx, ack)
	}
	return ack, nil
}

func imageBytes(t *testing.T, asJPEG bool) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			im.Set(x, y, color.NRGBA{R: 240, G: 10, B: 10, A: 255})
		}
	}
	var b bytes.Buffer
	var e error
	if asJPEG {
		e = jpeg.Encode(&b, im, &jpeg.Options{Quality: 90})
	} else {
		e = png.Encode(&b, im)
	}
	if e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func audioBytes() []byte {
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
	for i := range n {
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(int16(10000*math.Sin(2*math.Pi*880*float64(i)/8000))))
	}
	return data
}

func largeImageBytes(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	var state uint32 = 7
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			state = state*1664525 + 1013904223
			g := uint8(state>>24) % 60
			state = state*1664525 + 1013904223
			im.Set(x, y, color.NRGBA{R: 240, G: g, B: uint8(state>>24) % 60, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	if b.Len() <= 48<<10 {
		t.Fatal("fixture must require multiple evidence chunks")
	}
	return b.Bytes()
}

func setup(t *testing.T, mode string) (*fixtureServer, appv1.WorldAppServiceClient) {
	t.Helper()
	physical := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, physical, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	s := &fixtureServer{mode: mode, clock: &testClock{window: trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "test-physical-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-issued-time"}}, proofs: map[string][]byte{}, original: map[string][]byte{}, media: map[string][]byte{}, owners: map[string]int{}}
	s.cfg = Config{Version: 1, Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.example.multimodal", Service: "fusion", ExpectedGeneration: 7, RootsDER: [][]byte{id.RootDER}, RunID: "run", SceneBootID: "scene-boot", PackageSHA256: strings.Repeat("a", 64), SimulationAnchorUnixNS: time.Unix(1700000000, 0).UnixNano(), Frame: world.FrameRef{ID: "floor", Revision: "v1"}, ScenarioVersion: "mujoco-live-v1", Seed: 19, ResultStream: "hazards", DurationMS: 10000, MaxAttempts: 1}
	newPublisher := func(service, component, stream string, generation, epoch uint64) *serviceorigin.Publisher {
		p, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: s.cfg.Tenant, Site: s.cfg.Site, Device: s.cfg.Device, App: s.cfg.App, Service: service, DeploymentGeneration: generation, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: component, Stream: stream}}}, Epoch: epoch, Lifetime: 10 * time.Minute, Clock: s.clock, Certificates: certificates, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	s.claim = newPublisher("fusion", resultrecord.ComponentType, "hazards", 7, 77)
	s.publishers = make(map[string]*serviceorigin.Publisher)
	sensorIDs := []string{"camera-a", "camera-b", "gas-1", "audio-1"}
	if mode == "lidar" {
		sensorIDs = append(sensorIDs, "lidar-a", "lidar-b", "lidar-c")
	}
	for _, sensor := range sensorIDs {
		source := Source{SensorID: sensor, Device: s.cfg.Device, App: s.cfg.App, Service: "reader-" + sensor}
		s.cfg.Sources = append(s.cfg.Sources, source)
		sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/" + sensor + "/17", SiteID: s.cfg.Site, Frame: s.cfg.Frame, SensorID: sensor, Sequence: 17, SampleNS: int64(250 * time.Millisecond), RunID: s.cfg.RunID, ScenarioVersion: s.cfg.ScenarioVersion, Seed: s.cfg.Seed, PackageSHA256: s.cfg.PackageSHA256, SensorModel: "development-fixture", PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "cal-v1"}
		var media []byte
		content := ""
		switch sensor {
		case "camera-a", "camera-b":
			sample.Kind = "camera"
			sample.Camera = &sensing.Camera{Width: 8, Height: 6, FX: 8, FY: 8, CX: 4, CY: 3, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
			media = imageBytes(t, false)
			content = "image/png"
			if mode == "chunked" && sensor == "camera-a" {
				media = largeImageBytes(t)
				sample.Camera.Width = 256
				sample.Camera.Height = 256
				sample.Camera.FX = 256
				sample.Camera.FY = 256
				sample.Camera.CX = 128
				sample.Camera.CY = 128
			}
		case "gas-1":
			sample.Kind = "gas"
			sample.Gas = &sensing.Gas{ValuePPM: 80, UncertaintyPPM: 2, Species: "synthetic-gas"}
			if mode == "no-hazard" {
				sample.Gas.ValuePPM = 1
			}
		case "audio-1":
			sample.Kind = "audio"
			sample.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
			media = audioBytes()
			content = "audio/wav"
		default:
			sample.Kind = "lidar"
			sample.PositionM = []float64{0, 0, 0}
			sample.Lidar = &sensing.Lidar{PointsM: [][3]float64{{0, 0, 4}, {.01, 0, 4}}, MaxRangeM: 6}
		}
		if mode == "lidar" && sample.Camera != nil {
			x, pixel := -1.0, 5
			if sensor == "camera-b" {
				x, pixel = 1, 1
			}
			sample.PositionM = []float64{x, 0, 0}
			sample.Camera.CameraToSite = []float64{1, 0, 0, x, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
			im := image.NewRGBA(image.Rect(0, 0, 8, 6))
			for y := 2; y < 4; y++ {
				for u := pixel; u < pixel+2; u++ {
					im.Set(u, y, color.RGBA{R: 255, A: 255})
				}
			}
			var b bytes.Buffer
			if err := png.Encode(&b, im); err != nil {
				t.Fatal(err)
			}
			media = b.Bytes()
		}
		if media != nil {
			sample.Evidence = &sensing.EvidenceRef{SHA256: digest(media), ContentType: content, Size: int64(len(media))}
		}
		sampleRaw, _ := json.MarshalIndent(sample, "", "  ")
		capture := sensorrecord.Capture{Version: 1, SceneBootID: s.cfg.SceneBootID, CapturedUnixNS: physical.UnixNano(), SimulationAnchorUnixNS: s.cfg.SimulationAnchorUnixNS, SampleJSON: sampleRaw}
		if sensor == "camera-b" && mode != "lidar" {
			media = imageBytes(t, true)
			capture, err = sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{ChannelID: 4, Seq: 9, TsUs: uint64(physical.UnixMicro()), Flags: 1, Payload: media})
			if err != nil {
				t.Fatal(err)
			}
		}
		if media != nil {
			s.media[sensor+"/"+digest(media)] = media
		}
		request, e := sensorrecord.Publication(capture, 3*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		publisher := newPublisher(source.Service, sensorrecord.ComponentType, sensor, 2, 29)
		s.publishers[sensor] = publisher
		proof, e := publisher.Publish(context.Background(), request)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(proof)
		s.proofs[sensor] = raw
		s.original[sensor] = bytes.Clone(raw)
		if sensor == "camera-a" {
			p := newPublisher(source.Service, sensorrecord.ComponentType, sensor, 3, 30)
			next, e := p.Publish(context.Background(), request)
			if e != nil {
				t.Fatal(e)
			}
			s.replacement, _ = json.Marshal(next)
		}
		if sensor == "gas-1" {
			sample.Sequence = 18
			sample.ID = "run/gas-1/18"
			capture.SampleJSON, _ = json.Marshal(sample)
			nextRequest, e := sensorrecord.Publication(capture, 3*time.Second)
			if e != nil {
				t.Fatal(e)
			}
			next, e := publisher.Publish(context.Background(), nextRequest)
			if e != nil {
				t.Fatal(e)
			}
			s.newer, _ = json.Marshal(next)
		}
	}
	base, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "wwf-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "world.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// This server tests app behavior over real Unix gRPC, not kernel peer
	// admission. The broker's Linux suite exercises actual service identity.
	server := grpc.NewServer()
	appv1.RegisterWorldAppServiceServer(server, s)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := liveapp.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return s, appv1.NewWorldAppServiceClient(conn)
}

func TestOrdinaryFusionUsesExactCurrentSourcesAndPreservesProofAndMedia(t *testing.T) {
	for _, mode := range []string{"complete", "newer", "chunked"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setup(t, mode)
			sanity, e := api.Clock(context.Background(), &appv1.ClockRequest{Nonce: make([]byte, 32)})
			if _, decodeErr := appv1.DecodeClockReply(sanity); e != nil || decodeErr != nil {
				t.Fatalf("invalid fixture clock: %v %v %+v", e, decodeErr, sanity)
			}
			out, err := Run(context.Background(), s.cfg, api)
			if err != nil || out.Status != "complete" || !out.LocallyVerified || s.published != 1 || len(out.Inputs) != 4 || len(out.Media) != 3 {
				t.Fatalf("fusion failed: status=%s inputs=%d media=%d published=%d err=%v", out.Status, len(out.Inputs), len(out.Media), s.published, err)
			}
			for _, input := range out.Inputs {
				sensor := input.Reference.Key.Source.Stream
				if !bytes.Equal(input.EnvelopeJSON, s.original[sensor]) || input.Reference.Version.Sequence != 1 || input.Reference.EnvelopeSHA256 != digest(input.EnvelopeJSON) {
					t.Fatal("exact original input replaced or rewritten")
				}
			}
			for _, media := range out.Media {
				if !bytes.Equal(media.Data, s.media[media.Source.Stream+"/"+media.Reference.SHA256]) || s.owners[media.Source.Stream] == 0 {
					t.Fatal("media did not come from exact authorized owner")
				}
			}
			if mode == "chunked" && s.owners["camera-a"] < 2 {
				t.Fatal("large evidence was not read in bounded chunks")
			}
			if out.ProofSHA256 != digest(out.SignedRecordJSON) || len(out.RequestJSON) == 0 || out.Evaluation.Hazard.Inputs[0].Sample.Sequence != 17 {
				t.Fatal("receipt or native sequence lost")
			}
		})
	}
}
func TestOrdinaryFusionRefusesChangedAuthorityMediaAndExpiry(t *testing.T) {
	for _, mode := range []string{"replace", "revoke", "media-forgery", "media-offset", "forged", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setup(t, mode)
			s.cfg.MaxAttempts = 3
			out, err := Run(context.Background(), s.cfg, api)
			if err == nil || out.Status != "failed" || out.LocallyVerified || s.published != 0 || out.Attempts != 1 {
				t.Fatalf("invalid input accepted: status=%s published=%d error=%v", out.Status, s.published, err)
			}
		})
	}
}
func TestOrdinaryFusionNeverRetriesUnknownMutation(t *testing.T) {
	for _, mode := range []string{"uncertain", "wrong-ack"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setup(t, mode)
			s.cfg.MaxAttempts = 3
			out, err := Run(context.Background(), s.cfg, api)
			if !errors.Is(err, ErrUncertain) || out.Status != "unknown_outcome" || s.published != 1 || out.LocallyVerified || len(out.Inputs) != 4 || len(out.Media) != 3 || len(out.RequestJSON) == 0 {
				t.Fatal("unknown mutation retried or prior receipts hidden", err, out.Status, s.published)
			}
			stage, code := "rpc", "Unavailable"
			if mode == "wrong-ack" {
				stage, code = "ack_request_match", "OK"
			}
			publicationFailure(t, out, err, s, stage, code)
		})
	}
}
func TestOrdinaryFusionMissingOrNegativeEvidenceDoesNotPublish(t *testing.T) {
	for _, mode := range []string{"missing", "no-hazard"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setup(t, mode)
			out, err := Run(context.Background(), s.cfg, api)
			want := "insufficient_evidence"
			if mode == "no-hazard" {
				want = "not_detected"
			}
			if err != nil || out.Status != want || s.published != 0 {
				t.Fatal("absence or negative evidence became claim", err, out.Status)
			}
		})
	}
}

func TestOrdinaryFusionRefusesCommissioningMismatch(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"boot":           func(c *Config) { c.SceneBootID = "another-boot" },
		"package":        func(c *Config) { c.PackageSHA256 = strings.Repeat("b", 64) },
		"anchor":         func(c *Config) { c.SimulationAnchorUnixNS++ },
		"frame":          func(c *Config) { c.Frame.Revision = "different-survey" },
		"seed":           func(c *Config) { c.Seed++ },
		"scenario":       func(c *Config) { c.ScenarioVersion = "other-version" },
		"own-generation": func(c *Config) { c.ExpectedGeneration++ },
	} {
		t.Run(name, func(t *testing.T) {
			s, api := setup(t, "complete")
			cfg := s.cfg
			change(&cfg)
			out, err := Run(context.Background(), cfg, api)
			if err == nil || s.published != 0 || out.LocallyVerified {
				t.Fatal("wrong commissioning produced result", name, err)
			}
		})
	}
}

func TestCandidateGateIncludesAudioIntervalWithoutAuthorizingProofs(t *testing.T) {
	s, api := setup(t, "complete")
	// Endpoints are only170ms apart. The250ms audio interval makes the total
	//420ms, so this candidate must be discarded before any media or claim.
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(s.proofs["gas-1"], &signed)
	var record serviceorigin.Record
	_ = json.Unmarshal(signed.Record, &record)
	capture, err := sensorrecord.Decode(record.Publication.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sample, _ := capture.Sample()
	sample.SampleNS += int64(170 * time.Millisecond)
	capture.SampleJSON, _ = json.Marshal(sample)
	record.Publication, err = sensorrecord.Publication(capture, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	signed.Record, _ = json.Marshal(record)
	s.proofs["gas-1"], _ = json.Marshal(signed)
	s.cfg.MaxAttempts = 2
	out, err := Run(context.Background(), s.cfg, api)
	if err != nil || out.Status != "insufficient_evidence" || out.Attempts != 2 || out.Evaluation == nil || len(out.Media) != 0 || s.published != 0 {
		t.Fatalf("incoherent tuple reached verification/media: status=%s err=%v", out.Status, err)
	}
	// A temporally coherent forged proof must still fail cryptographic checks.
	s2, api2 := setup(t, "forged")
	out, err = Run(context.Background(), s2.cfg, api2)
	if err == nil || s2.published != 0 || out.FailureStage != "verify_input" || out.ErrorCode != "signature_or_authority_refused" {
		t.Fatalf("coherent forged proof escaped verification: stage=%s code=%s err=%v", out.FailureStage, out.ErrorCode, err)
	}
}

func TestNewCandidateHardFailureDoesNotRetainPreviousEvaluation(t *testing.T) {
	s, api := setup(t, "complete")
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(s.proofs["gas-1"], &signed)
	var original serviceorigin.Record
	_ = json.Unmarshal(signed.Record, &original)
	capture, _ := sensorrecord.Decode(original.Publication.Payload)
	sample, _ := capture.Sample()
	sample.SampleNS += int64(170 * time.Millisecond)
	capture.SampleJSON, _ = json.Marshal(sample)
	skew := original
	skew.Publication, _ = sensorrecord.Publication(capture, 3*time.Second)
	signed.Record, _ = json.Marshal(skew)
	s.replacement, _ = json.Marshal(signed)
	original.Sequence++
	signed.Record, _ = json.Marshal(original)
	s.proofs["gas-1"], _ = json.Marshal(signed)
	s.mode = "skew-then-forged"
	s.cfg.MaxAttempts = 2
	out, err := Run(context.Background(), s.cfg, api)
	if err == nil || out.Attempts != 2 || out.Evaluation != nil || len(out.Inputs) != 4 || len(out.Media) != 0 || s.published != 0 {
		t.Fatalf("previous evaluation attached to failed new candidate: attempts=%d evaluation=%v err=%v", out.Attempts, out.Evaluation, err)
	}
}
