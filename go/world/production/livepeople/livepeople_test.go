package livepeople

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fixtureClock struct {
	mu sync.Mutex
	w  trustedtime.Interval
}

func (c *fixtureClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w, nil
}
func (c *fixtureClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Earliest = c.w.Earliest.Add(d)
	c.w.Latest = c.w.Latest.Add(d)
	c.w.Monotonic += d
}

type peopleFixture struct {
	appv1.UnimplementedWorldAppServiceServer
	mu                          sync.Mutex
	cfg                         Config
	clock                       *fixtureClock
	sampleNS                    int64
	generation                  uint64
	proofs                      map[string][]byte
	media                       map[string][]byte
	publishers                  map[string]*serviceorigin.Publisher
	claim                       *serviceorigin.Publisher
	replacement                 []byte
	published                   []serviceorigin.PublishRequest
	mode                        string
	queries, evidence, inferred int
	inferHook                   func()
	queryHook                   func(int)
	image                       []byte
}

func (f *peopleFixture) Clock(_ context.Context, q *appv1.ClockRequest) (*appv1.ClockReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, _ := f.clock.Now()
	return &appv1.ClockReply{Nonce: bytes.Clone(q.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, SampleClockDomain: "simulation:" + f.cfg.RunID, SampleUnixNs: f.cfg.SimulationAnchorUnixNS + f.sampleNS, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Device: f.cfg.Device, App: f.cfg.App, Service: f.cfg.Service, ExecutionDomain: "simulation", DeploymentGeneration: f.generation, ServerBootId: strings.Repeat("a", 32)}, nil
}
func (f *peopleFixture) PhysicalClock(ctx context.Context, q *appv1.ClockRequest) (*appv1.PhysicalClockReply, error) {
	c, e := f.Clock(ctx, q)
	if e != nil {
		return nil, e
	}
	return &appv1.PhysicalClockReply{Nonce: c.Nonce, Physical: c.Physical, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration, ServerBootId: c.ServerBootId}, nil
}
func (f *peopleFixture) Query(_ context.Context, q *appv1.QueryRequest) (*appv1.QueryReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.queryHook != nil {
		f.queryHook(f.queries)
	}
	if len(q.Sources) != len(f.cfg.Sources) || q.Limit != uint32(len(q.Sources)) || q.After != "" {
		return nil, status.Error(codes.InvalidArgument, "query")
	}
	r := &appv1.QueryReply{}
	for i, source := range q.Sources {
		expected := f.cfg.Sources[i]
		if source.App != expected.App || source.Service != expected.Service || source.Stream != expected.SensorID || source.ComponentType != sensorrecord.ComponentType || source.Device != f.cfg.Device {
			return nil, status.Error(codes.PermissionDenied, "scope")
		}
		if raw := f.proofs[source.Stream]; len(raw) > 0 {
			owned := bytes.Clone(raw)
			if f.mode == "forged-source" {
				var signed serviceorigin.SignedRecord
				_ = json.Unmarshal(owned, &signed)
				signed.Signature[0] ^= 1
				owned, _ = json.Marshal(signed)
			}
			r.SignedRecordJson = append(r.SignedRecordJson, owned)
		}
	}
	return r, nil
}
func (f *peopleFixture) GetEvidence(_ context.Context, q *appv1.GetEvidenceRequest) (*appv1.GetEvidenceReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evidence++
	if q.Source == nil {
		return nil, status.Error(codes.PermissionDenied, "owner")
	}
	ok := false
	for _, source := range f.cfg.Sources {
		if q.Source.App == source.App && q.Source.Service == source.Service && q.Source.Stream == source.SensorID && q.Source.Device == f.cfg.Device && q.Source.ComponentType == sensorrecord.ComponentType {
			ok = true
		}
	}
	data := f.media[q.Source.Stream+"/"+q.Sha256]
	if !ok || q.Offset < 0 || q.Length < 1 || q.Offset+int64(q.Length) > int64(len(data)) {
		return nil, status.Error(codes.PermissionDenied, "media")
	}
	part := bytes.Clone(data[q.Offset : q.Offset+int64(q.Length)])
	if f.mode == "corrupt-media" {
		part[0] ^= 1
	}
	w, _ := f.clock.Now()
	retention := w.Earliest.Add(time.Hour)
	if f.mode == "expired-ack-media" {
		retention = w.Earliest.Add(2 * time.Second)
	}
	return &appv1.GetEvidenceReply{Sha256: q.Sha256, Offset: q.Offset, Size: int64(len(data)), Data: part, ContentType: "image/png", RetainUntilUnixMs: retention.UnixMilli()}, nil
}
func (f *peopleFixture) Publish(ctx context.Context, q *appv1.PublishRequest) (*appv1.PublishReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, e := appv1.DecodePublish(q)
	if e != nil {
		return nil, e
	}
	f.published = append(f.published, p)
	r, e := f.claim.Publish(ctx, p)
	if e != nil {
		return nil, e
	}
	if f.mode == "uncertain" {
		return nil, status.Error(codes.Unavailable, "ack lost after signing")
	}
	if f.mode == "expired-ack-sample" {
		f.sampleNS = int64(3 * time.Second)
	}
	if f.mode == "expired-ack-physical" {
		f.clock.advance(6 * time.Second)
	}
	if f.mode == "expired-ack-media" {
		f.clock.advance(3 * time.Second)
	}
	raw, _ := json.Marshal(r)
	return &appv1.PublishReply{Accepted: true, SignedRecordJson: raw}, nil
}
func (f *peopleFixture) infer(_ context.Context, raw []byte) (pose.ImageResult, error) {
	f.mu.Lock()
	f.inferred++
	hook := f.inferHook
	f.inferHook = nil
	f.mu.Unlock()
	if !bytes.Equal(raw, f.image) {
		return pose.ImageResult{}, errors.New("inference did not receive exact original PNG")
	}
	if hook != nil {
		hook()
	}
	out := pose.ImageResult{ModelID: f.cfg.ModelID, ModelVersion: f.cfg.ModelVersion, ModelSHA256: f.cfg.ModelSHA256, ImageSHA256: digest(raw), ImageWidth: 320, ImageHeight: 240, Detections: []pose.Person{{Score: .95, LeftAnkle: pose.Keypoint{X: 150, Y: 140, Confidence: .9}, RightAnkle: pose.Keypoint{X: 170, Y: 140, Confidence: .9}}}}
	if f.mode == "wrong-image" {
		out.ImageSHA256 = strings.Repeat("f", 64)
	}
	return out, nil
}

func setupPeople(t *testing.T) (*peopleFixture, appv1.WorldAppServiceClient) {
	t.Helper()
	physical := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, physical, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	f := &peopleFixture{clock: &fixtureClock{w: trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "people-test-clock", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-issued-time"}}, sampleNS: int64(500 * time.Millisecond), generation: 7, proofs: map[string][]byte{}, media: map[string][]byte{}, publishers: map[string]*serviceorigin.Publisher{}}
	f.cfg = Config{Version: 1, Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.example.people", Service: "tracking", ExpectedGeneration: 7, RootsDER: [][]byte{id.RootDER}, RunID: "people-test", SceneBootID: "scene-boot", PackageSHA256: strings.Repeat("a", 64), SimulationAnchorUnixNS: time.Unix(1700000000, 0).UnixNano(), Frame: world.FrameRef{ID: "office", Revision: "v1"}, ScenarioVersion: "people-v1", Seed: 19, ResultStream: "tracks", DurationMS: 10000, MaxTicks: 8, AnkleHeightM: .08, ModelID: "fixture-image-model", ModelVersion: "1", ModelSHA256: strings.Repeat("b", 64)}
	newPublisher := func(service, component, stream string, generation, epoch uint64) *serviceorigin.Publisher {
		p, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: f.cfg.Tenant, Site: f.cfg.Site, Device: f.cfg.Device, App: f.cfg.App, Service: service, DeploymentGeneration: generation, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: component, Stream: stream}}}, Epoch: epoch, Lifetime: 10 * time.Minute, Clock: f.clock, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	f.claim = newPublisher(f.cfg.Service, resultrecord.ComponentType, f.cfg.ResultStream, 7, 77)
	im := image.NewNRGBA(image.Rect(0, 0, 320, 240))
	im.Set(10, 10, color.NRGBA{R: 128, A: 255})
	var b bytes.Buffer
	if e := png.Encode(&b, im); e != nil {
		t.Fatal(e)
	}
	f.image = b.Bytes()
	for _, sensor := range []string{"people-front", "people-oblique"} {
		f.cfg.Sources = append(f.cfg.Sources, Source{SensorID: sensor, App: f.cfg.App, Service: sensor})
		f.publishers[sensor] = newPublisher(sensor, sensorrecord.ComponentType, sensor, 2, 29)
		f.setCapture(t, sensor, 17, 250*time.Millisecond)
	}
	replacement := newPublisher("people-front", sensorrecord.ComponentType, "people-front", 3, 30)
	_, r, e := decodeEnvelope(f.proofs["people-front"])
	if e != nil {
		t.Fatal(e)
	}
	signed, e := replacement.Publish(context.Background(), r.Publication)
	if e != nil {
		t.Fatal(e)
	}
	f.replacement, _ = json.Marshal(signed)
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	appv1.RegisterWorldAppServiceServer(server, f)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, e := grpc.NewClient("passthrough:///people-fixture", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return f, appv1.NewWorldAppServiceClient(conn)
}
func (f *peopleFixture) setCapture(t *testing.T, sensor string, sequence uint64, at time.Duration) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	w, _ := f.clock.Now()
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("%s/%s/%d", f.cfg.RunID, sensor, sequence), SiteID: f.cfg.Site, Frame: f.cfg.Frame, SensorID: sensor, Kind: "camera", Sequence: sequence, SampleNS: int64(at), RunID: f.cfg.RunID, ScenarioVersion: f.cfg.ScenarioVersion, Seed: f.cfg.Seed, PackageSHA256: f.cfg.PackageSHA256, SensorModel: "fixture-camera", PositionM: []float64{0, 2, 0}, UncertaintyM: .02, CalibrationRevision: "cal-v1", Camera: &sensing.Camera{Width: 320, Height: 240, FX: 200, FY: 200, CX: 160, CY: 120, CameraToSite: []float64{1, 0, 0, 0, 0, 0, -1, 2, 0, 1, 0, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}, Evidence: &sensing.EvidenceRef{SHA256: digest(f.image), Size: int64(len(f.image)), ContentType: "image/png"}}
	raw, _ := json.MarshalIndent(sample, "", "  ")
	capture := sensorrecord.Capture{Version: 1, SceneBootID: f.cfg.SceneBootID, CapturedUnixNS: w.Earliest.UnixNano(), SimulationAnchorUnixNS: f.cfg.SimulationAnchorUnixNS, SampleJSON: raw}
	request, e := sensorrecord.Publication(capture, 3*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	signed, e := f.publishers[sensor].Publish(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	f.proofs[sensor], _ = json.Marshal(signed)
	f.media[sensor+"/"+digest(f.image)] = bytes.Clone(f.image)
}

func TestSignedRPCPublishesOriginalCameraLineage(t *testing.T) {
	f, api := setupPeople(t)
	s, e := New(f.cfg, api, f.infer)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := s.Step(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "complete" || len(r.Tracks) != 1 || len(r.Publications) != 1 || !r.Publications[0].LocallyVerified || f.inferred != 2 {
		t.Fatalf("unexpected cycle %+v infer=%d", r, f.inferred)
	}
	if len(r.Tracks[0].Members) != 2 {
		t.Fatalf("camera members %+v", r.Tracks[0])
	}
	for _, input := range r.Inputs {
		if !bytes.Equal(input.EnvelopeJSON, f.proofs[input.Reference.Key.Source.Stream]) || input.Reference.Version.Sequence != 1 {
			t.Fatal("original proof bytes or agent sequence changed")
		}
	}
	p := f.published[0]
	if p.ComponentType != resultrecord.ComponentType || p.Stream != "tracks" || p.TTL > 1250*time.Millisecond {
		t.Fatalf("publication bounds %+v", p)
	}
	var body resultrecord.Result
	if e = json.Unmarshal(p.Payload, &body); e != nil {
		t.Fatal(e)
	}
	if len(body.Inputs) != 2 || !bytes.Contains(body.ResultJSON, []byte(f.cfg.ModelSHA256)) || !bytes.Contains(body.ResultJSON, []byte("cal-v1")) {
		t.Fatalf("missing image/model/calibration lineage: %s", body.ResultJSON)
	}
	first := r.Tracks[0]
	f.mu.Lock()
	f.sampleNS = int64(750 * time.Millisecond)
	f.mu.Unlock()
	r, e = s.Step(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Publications) != 0 || f.inferred != 2 || len(r.Tracks) != 1 || r.Tracks[0].ID != first.ID || !r.Tracks[0].ValidUntil.Equal(first.ValidUntil) {
		t.Fatal("duplicate capture renewed or republished track")
	}
	f.setCapture(t, "people-front", 18, 750*time.Millisecond)
	f.setCapture(t, "people-oblique", 18, 750*time.Millisecond)
	f.mu.Lock()
	f.sampleNS = int64(time.Second)
	f.mu.Unlock()
	r, e = s.Step(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Publications) != 1 || r.Tracks[0].ID != first.ID || len(r.Inputs) != 4 || f.inferred != 4 {
		t.Fatalf("new captures and previous support lost: %+v", r)
	}
	var next resultrecord.Result
	if e = json.Unmarshal(f.published[1].Payload, &next); e != nil {
		t.Fatal(e)
	}
	if len(next.Inputs) != 4 || !bytes.Contains(next.ResultJSON, []byte(`"motion_known":true`)) {
		t.Fatalf("motion lacks two freshly verified groups: %s", next.ResultJSON)
	}
}

func TestPartialCameraAndSilenceDoNotRenewTracks(t *testing.T) {
	f, api := setupPeople(t)
	s, e := New(f.cfg, api, f.infer)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	delete(f.proofs, "people-oblique")
	f.sampleNS = int64(time.Second)
	f.mu.Unlock()
	f.setCapture(t, "people-front", 18, 750*time.Millisecond)
	r, e := s.Step(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Publications) != 1 || len(r.Tracks) != 1 || len(r.Tracks[0].Members) != 1 {
		t.Fatalf("one current camera cannot publish position: %+v", r)
	}
	until := r.Tracks[0].ValidUntil
	f.mu.Lock()
	f.proofs = map[string][]byte{}
	f.sampleNS = int64(1250 * time.Millisecond)
	f.mu.Unlock()
	r, e = s.Step(context.Background())
	if e != nil || r.Status != "waiting" || len(r.Tracks) != 1 || len(r.Publications) != 0 || !r.Tracks[0].ValidUntil.Equal(until) {
		t.Fatalf("coast %+v %v", r, e)
	}
	f.mu.Lock()
	f.sampleNS = until.UnixNano() - f.cfg.SimulationAnchorUnixNS
	f.mu.Unlock()
	r, e = s.Step(context.Background())
	if e != nil || len(r.Tracks) != 0 || len(r.Publications) != 0 {
		t.Fatalf("expired original still visible %+v %v", r, e)
	}
}

func TestBlockingInferenceRechecksAuthorityAndBothClocks(t *testing.T) {
	for _, mode := range []string{"replacement", "revocation", "sample-expiry", "physical-expiry", "reader-generation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setupPeople(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.inferHook = func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				switch mode {
				case "replacement":
					f.proofs["people-front"] = bytes.Clone(f.replacement)
				case "revocation":
					delete(f.proofs, "people-front")
				case "sample-expiry":
					f.sampleNS = int64(4 * time.Second)
				case "physical-expiry":
					f.clock.advance(6 * time.Second)
				case "reader-generation":
					f.generation++
				case "cancel":
					cancel()
				}
			}
			s, e := New(f.cfg, api, f.infer)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			r, e := s.Step(ctx)
			if mode == "sample-expiry" {
				if e != nil || r.Status != "waiting" || len(r.Tracks) != 0 || len(f.published) != 0 {
					t.Fatalf("aged person observations did not expire quietly: %+v %v", r, e)
				}
				return
			}
			if e == nil || len(f.published) != 0 || len(r.Publications) != 0 {
				t.Fatalf("published after %s: %+v %v", mode, r, e)
			}
		})
	}
}

func TestQueryableCameraPastPersonLifetimeDoesNotFaultTracking(t *testing.T) {
	f, api := setupPeople(t)
	s, e := New(f.cfg, api, f.infer)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
	// Both original camera records remain within their raw three-second TTL.
	// Only front receives a fresh image; oblique is still returned by Query.
	f.setCapture(t, "people-front", 18, 1500*time.Millisecond)
	f.mu.Lock()
	f.sampleNS = int64(1600 * time.Millisecond)
	f.mu.Unlock()
	r, e := s.Step(context.Background())
	if e != nil || len(r.Tracks) != 1 || len(r.Publications) != 1 || len(r.Inputs) != 1 || len(r.Tracks[0].Members) != 1 || r.Tracks[0].Members[0].Source != "people-front" || f.inferred != 3 {
		t.Fatalf("aged queried camera blocked fresh peer: %+v %v", r, e)
	}
	f.mu.Lock()
	f.sampleNS = int64(2800 * time.Millisecond)
	f.mu.Unlock()
	r, e = s.Step(context.Background())
	if e != nil || r.Status != "waiting" || len(r.Tracks) != 0 || len(r.Publications) != 0 || f.inferred != 3 || len(f.proofs) != 2 {
		t.Fatalf("retained raw cameras did not become no-input person tick: %+v %v", r, e)
	}
}

func TestBadEvidenceAndUncertainPublishAreNotRetried(t *testing.T) {
	for _, mode := range []string{"forged-source", "corrupt-media", "wrong-image", "uncertain", "expired-ack-sample", "expired-ack-physical", "expired-ack-media"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setupPeople(t)
			f.mode = mode
			s, e := New(f.cfg, api, f.infer)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			r, e := s.Step(context.Background())
			if e == nil {
				t.Fatal("accepted failure")
			}
			want := 0
			if mode == "uncertain" || mode == "expired-ack-sample" || mode == "expired-ack-physical" || mode == "expired-ack-media" {
				want = 1
				if !errors.Is(e, ErrUncertain) || r.Status != "unknown_outcome" {
					t.Fatalf("lost ambiguity: %+v %v", r, e)
				}
			}
			if len(f.published) != want {
				t.Fatal("unexpected publication")
			}
			if mode == "forged-source" && (f.evidence != 0 || f.inferred != 0) {
				t.Fatal("forged source reached media/inference")
			}
			if mode == "uncertain" {
				duplicate, err := s.Step(context.Background())
				if err != nil || len(duplicate.Publications) != 0 || len(f.published) != want {
					t.Fatal("unknown publication retried its original observations")
				}
			} else if _, e = s.Step(context.Background()); !errors.Is(e, ErrClosed) || len(f.published) != want {
				t.Fatal("faulted operation retried")
			}
		})
	}
}

func TestConfigAndBoundedRun(t *testing.T) {
	f, api := setupPeople(t)
	for _, change := range []func(*Config){func(c *Config) { c.Sources = nil }, func(c *Config) { c.Sources = append(c.Sources, c.Sources[0]) }, func(c *Config) { c.ModelSHA256 = "bad" }, func(c *Config) { c.ExpectedGeneration = 8 }} {
		c := f.cfg
		c.Sources = append([]Source(nil), c.Sources...)
		change(&c)
		s, e := New(c, api, f.infer)
		if e == nil {
			_, e = s.Step(context.Background())
			s.Close()
		}
		if e == nil {
			t.Fatal("accepted invalid configuration/reader pin")
		}
	}
	c := f.cfg
	c.MaxTicks = 2
	c.PollIntervalMS = 50
	var receipts []Result
	r, e := Run(context.Background(), c, api, f.infer, func(r Result) error { receipts = append(receipts, r); return nil })
	if e != nil || r.Status != "complete" || r.Ticks != 2 || r.Publications != 1 || len(receipts) != 2 {
		t.Fatalf("bounded run %+v %v", r, e)
	}
}
