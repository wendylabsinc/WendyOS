package peoplereader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fixture struct {
	appv1.UnimplementedWorldAppServiceServer
	mu              sync.Mutex
	cfg             Config
	w               trustedtime.Interval
	now             time.Time
	raw             []byte
	generation      uint64
	mode            string
	queries         int
	deniedRemaining int
	clocks          int
	queryHook       func(int)
	fresh           func()
}

func (f *fixture) Now() (trustedtime.Interval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, nil
}
func (f *fixture) Clock(_ context.Context, q *appv1.ClockRequest) (*appv1.ClockReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clocks++
	return &appv1.ClockReply{Nonce: bytes.Clone(q.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: f.w.Earliest.UnixNano(), LatestUnixNs: f.w.Latest.UnixNano(), BootId: f.w.BootID, MonotonicNs: int64(f.w.Monotonic), Source: f.w.Source}, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Device: f.cfg.Device, App: f.cfg.App, Service: f.cfg.Service, ExecutionDomain: "simulation", DeploymentGeneration: f.generation, ServerBootId: strings.Repeat("c", 32), SampleClockDomain: "simulation:" + f.cfg.RunID, SampleUnixNs: f.now.UnixNano()}, nil
}
func (f *fixture) PhysicalClock(ctx context.Context, q *appv1.ClockRequest) (*appv1.PhysicalClockReply, error) {
	c, e := f.Clock(ctx, q)
	return &appv1.PhysicalClockReply{Nonce: c.Nonce, Physical: c.Physical, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration, ServerBootId: c.ServerBootId}, e
}
func (f *fixture) Query(_ context.Context, q *appv1.QueryRequest) (*appv1.QueryReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.queryHook != nil {
		f.queryHook(f.queries)
	}
	if len(q.Sources) != 1 || q.Sources[0].Device != f.cfg.Source.Device || q.Sources[0].App != f.cfg.Source.App || q.Sources[0].Service != f.cfg.Source.Service || q.Sources[0].Stream != f.cfg.Source.Stream || q.Sources[0].ComponentType != resultrecord.ComponentType || q.Limit != serviceaccessLimit || q.After != "" {
		return nil, status.Error(codes.PermissionDenied, "source")
	}
	if f.mode == "denied" {
		return nil, status.Error(codes.PermissionDenied, "current peer absent")
	}
	if f.deniedRemaining > 0 {
		f.deniedRemaining--
		return nil, status.Error(codes.Unavailable, "startup")
	}
	r := &appv1.QueryReply{}
	if len(f.raw) > 0 {
		r.SignedRecordJson = [][]byte{bytes.Clone(f.raw)}
	}
	if f.mode == "cursor" {
		r.Next = "partial"
	}
	return r, nil
}

const serviceaccessLimit = 8
const startupSpacing = 200 * time.Millisecond
const startupMaxAttempts = 32

func TestConfiguredRequestTimeoutKeepsClockProtocolBound(t *testing.T) {
	f, api := readerFixture(t)
	for _, ms := range []int64{3000, 5000} {
		c := f.cfg
		c.RequestTimeoutMS = ms
		normalized, err := c.normalized()
		if err != nil || normalized.RequestTimeoutMS != ms {
			t.Fatalf("configured timeout %d refused: %v", ms, err)
		}
		// Successful construction also checks compatibility with appclock's
		// existing one-second MaxRoundTrip ceiling.
		s, err := New(c, api)
		if err != nil {
			t.Fatalf("clock incompatible with timeout %d: %v", ms, err)
		}
		s.Close()
	}
	c := f.cfg
	c.RequestTimeoutMS = 5001
	if _, err := c.normalized(); err == nil {
		t.Fatal("accepted timeout above five seconds")
	}
	if s, err := New(c, api); err == nil {
		s.Close()
		t.Fatal("session accepted oversized timeout")
	}
}

func readerFixture(t *testing.T) (*fixture, appv1.WorldAppServiceClient) {
	t.Helper()
	physical := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	source := "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/22222222-2222-4222-8222-222222222222"
	reader := "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/33333333-3333-4333-8333-333333333333"
	id := testidentity.NewWith(t, physical, source)
	f := &fixture{w: trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "reader-test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test"}, now: time.Unix(1700000000, 0).Add(500 * time.Millisecond), generation: 9}
	f.cfg = Config{Version: 1, Tenant: id.Tenant, Site: "office", Device: reader, App: "com.example.people", Service: "reader", ExpectedGeneration: 9, RootsDER: [][]byte{id.RootDER}, RunID: "people", Frame: world.FrameRef{ID: "office", Revision: "v1"}, Source: Source{Device: source, App: "com.example.people", Service: "people", Stream: "tracks"}, DurationMS: 10000, MaxTicks: 4}
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	pub := func(service, component, stream string) *serviceorigin.Publisher {
		p, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: id.Tenant, Site: f.cfg.Site, Device: source, App: f.cfg.App, Service: service, DeploymentGeneration: 7, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: component, Stream: stream}}}, Epoch: 17, Lifetime: time.Minute, Clock: f, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { p.Close() })
		return p
	}
	camera := pub("camera", sensorrecord.ComponentType, "camera")
	var imageBytes bytes.Buffer
	if e = png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 320, 240))); e != nil {
		t.Fatal(e)
	}
	data := imageBytes.Bytes()
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "people/camera/1", SiteID: f.cfg.Site, Frame: f.cfg.Frame, SensorID: "camera", Kind: "camera", Sequence: 1, SampleNS: int64(250 * time.Millisecond), RunID: f.cfg.RunID, ScenarioVersion: "v1", Seed: 1, PackageSHA256: strings.Repeat("a", 64), SensorModel: "fixture", PositionM: []float64{0, 2, 0}, UncertaintyM: .02, CalibrationRevision: "cal", Camera: &sensing.Camera{Width: 320, Height: 240, FX: 200, FY: 200, CX: 160, CY: 120, CameraToSite: []float64{1, 0, 0, 0, 0, 0, -1, 2, 0, 1, 0, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}, Evidence: &sensing.EvidenceRef{SHA256: hash(data), ContentType: "image/png", Size: int64(len(data))}}
	sampleRaw, _ := json.Marshal(sample)
	capture := sensorrecord.Capture{Version: 1, SceneBootID: "scene", CapturedUnixNS: physical.UnixNano(), SimulationAnchorUnixNS: time.Unix(1700000000, 0).UnixNano(), SampleJSON: sampleRaw}
	request, e := sensorrecord.Publication(capture, 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	signed, e := camera.Publish(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(signed)
	verifier, e := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: id.Tenant, Site: f.cfg.Site, ExecutionDomain: "simulation", Certificates: certs, Clock: f, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, d wendycert.DeviceIdentity) error {
		if c.Scope.Service != "camera" || d.Principal != source {
			return ErrRefused
		}
		return nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	verified, e := verifier.Verify(context.Background(), signed)
	if e != nil {
		t.Fatal(e)
	}
	model := strings.Repeat("b", 64)
	tracker, e := trackcapture.New(trackcapture.Config{Tenant: id.Tenant, Site: f.cfg.Site, Tracking: tracking.Config{TrackerID: "fixture", RunID: f.cfg.RunID, ClockDomain: "simulation:" + f.cfg.RunID, Frame: f.cfg.Frame, MaxMiss: 1250 * time.Millisecond}, Sources: map[string]world.Source{"camera": verified.Component().Key.Source}, ModelID: "fixture", ModelVersion: "1", ModelSHA256: model, Projection: pose.Config{AnkleHeightM: .08}})
	if e != nil {
		t.Fatal(e)
	}
	snapshot, e := tracker.Step(f.now, f.w, []trackcapture.Input{{Envelope: raw, Verified: verified, Image: data, Poses: pose.ImageResult{ModelID: "fixture", ModelVersion: "1", ModelSHA256: model, ImageSHA256: hash(data), ImageWidth: 320, ImageHeight: 240, Detections: []pose.Person{{Score: .9, LeftAnkle: pose.Keypoint{X: 150, Y: 140, Confidence: .9}, RightAnkle: pose.Keypoint{X: 170, Y: 140, Confidence: .9}}}}}})
	if e != nil || len(snapshot.Tracks) != 1 {
		t.Fatalf("track fixture %v", e)
	}
	request, _, e = snapshot.BuildPublication(snapshot.Tracks[0].ID, "tracks", physical.Add(time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	results := pub("people", resultrecord.ComponentType, "tracks")
	result, e := results.Publish(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	f.raw, _ = json.Marshal(result)
	f.fresh = func() {
		// Generate new camera and result originals under the same source
		// incarnation, using the advanced sample and physical clocks.
		sample.Sequence++
		sample.SampleNS = f.now.UnixNano() - capture.SimulationAnchorUnixNS
		sample.ID = fmt.Sprintf("%s/%s/%d", sample.RunID, sample.SensorID, sample.Sequence)
		capture.SampleJSON, _ = json.Marshal(sample)
		capture.CapturedUnixNS = f.w.Earliest.UnixNano()
		request, err := sensorrecord.Publication(capture, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := camera.Publish(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := verifier.Verify(context.Background(), signed)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(signed)
		snapshot, err := tracker.Step(f.now, f.w, []trackcapture.Input{{Envelope: raw, Verified: verified, Image: data, Poses: pose.ImageResult{ModelID: "fixture", ModelVersion: "1", ModelSHA256: model, ImageSHA256: hash(data), ImageWidth: 320, ImageHeight: 240, Detections: []pose.Person{{Score: .9, LeftAnkle: pose.Keypoint{X: 150, Y: 140, Confidence: .9}, RightAnkle: pose.Keypoint{X: 170, Y: 140, Confidence: .9}}}}}})
		if err != nil || len(snapshot.Tracks) != 1 {
			t.Fatalf("fresh track fixture: count=%d err=%v", len(snapshot.Tracks), err)
		}
		request, _, err = snapshot.BuildPublication(snapshot.Tracks[0].ID, "tracks", f.w.Earliest.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		result, err := results.Publish(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		f.raw, _ = json.Marshal(result)
	}
	l := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	appv1.RegisterWorldAppServiceServer(server, f)
	go func() { _ = server.Serve(l) }()
	t.Cleanup(server.Stop)
	conn, e := grpc.NewClient("passthrough:///reader-fixture", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Close() })
	return f, appv1.NewWorldAppServiceClient(conn)
}
func TestExactRemoteOriginReadAndAbsence(t *testing.T) {
	f, api := readerFixture(t)
	s, e := New(f.cfg, api)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := s.Step(context.Background())
	if e != nil || len(r.Tracks) != 1 {
		t.Fatalf("read %+v %v", r, e)
	}
	if r.Status != "ready" || !bytes.Equal(r.Tracks[0].EnvelopeJSON, f.raw) || !r.OriginAttestedInference || r.CameraInputsIndependentlyVerified || r.ReaderGeneration != 9 {
		t.Fatal("reader rewrote proof or overstated verification")
	}
	id := r.Tracks[0].Payload.TrackID
	f.mu.Lock()
	f.raw = nil
	f.now = f.now.Add(2 * time.Second)
	f.mu.Unlock()
	r, e = s.Step(context.Background())
	if e != nil || len(r.Tracks) != 0 || len(r.AbsentTrackIDs) != 1 || r.AbsentTrackIDs[0] != id {
		t.Fatalf("absence %+v %v", r, e)
	}
}

func TestStartupWaitingIsBoundedAndNeverAbsence(t *testing.T) {
	f, api := readerFixture(t)
	f.raw = nil
	f.deniedRemaining = 2
	c := f.cfg
	c.MaxTicks = 1
	var receipts []Result
	start := time.Now()
	summary, err := Run(context.Background(), c, api, func(r Result) error { receipts = append(receipts, r); return nil })
	if err != nil || summary.Status != "complete" || summary.StartupWaits != 2 || summary.Ticks != 1 || len(receipts) != 3 || time.Since(start) < 2*startupSpacing {
		t.Fatalf("startup loop %+v %v", summary, err)
	}
	for i, r := range receipts[:2] {
		raw, _ := json.Marshal(r)
		if r.Status != "waiting_for_source" || r.RPCCode != "Unavailable" || r.StartupAttempt != i+1 || r.OriginAttestedInference || bytes.Contains(raw, []byte("absent_track_ids")) {
			t.Fatalf("waiting claimed absence: %s", raw)
		}
	}
	if receipts[2].Status != "ready" || len(receipts[2].Tracks) != 0 || f.clocks != 4 {
		t.Fatalf("ready/fresh clocks %+v clocks=%d", receipts[2], f.clocks)
	}
	// A successful empty Query ends startup waiting. Later loss of the peer
	// cannot be converted into a second startup or a successful empty read.
	f, api = readerFixture(t)
	f.raw = nil
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, e := s.Step(context.Background()); e != nil || r.Status != "ready" {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.mode = "denied"
	f.mu.Unlock()
	r, err := s.Step(context.Background())
	if err == nil || r.Status != "failed" {
		t.Fatal("post-read denial entered startup retry")
	}
}

func TestStartupAttemptTimeAndIdentityBounds(t *testing.T) {
	for _, mode := range []string{"attempts", "identity", "malformed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, api := readerFixture(t)
			f.mode = "denied"
			s, err := New(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			switch mode {
			case "attempts":
				for i := 1; i <= startupMaxAttempts; i++ {
					r, e := s.Step(context.Background())
					if e == nil || r.StartupAttempt != i {
						t.Fatalf("attempt %d: %+v %v", i, r, e)
					}
					if (i < startupMaxAttempts) != (r.Status == "waiting_for_source") {
						t.Fatal("attempt bound differs")
					}
				}
				if f.queries != startupMaxAttempts {
					t.Fatal("extra startup Query")
				}
			case "identity":
				f.generation++
				r, e := s.Step(context.Background())
				if e == nil || r.Status != "failed" || f.queries != 0 {
					t.Fatal("reader identity change retried")
				}
			case "malformed":
				f.mode = "cursor"
				r, e := s.Step(context.Background())
				if e == nil || r.Status != "failed" {
					t.Fatal("malformed Query retried")
				}
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r, e := s.Step(ctx)
				if e == nil || r.Status != "failed" || f.queries != 0 {
					t.Fatal("canceled startup retried")
				}
			}
			if _, e := s.Step(context.Background()); e == nil {
				t.Fatal("terminal startup reopened")
			}
		})
	}
}
func TestRefusalIsNotAbsence(t *testing.T) {
	for _, mode := range []string{"denied", "cursor", "signature", "reader-replaced"} {
		t.Run(mode, func(t *testing.T) {
			f, api := readerFixture(t)
			s, e := New(f.cfg, api)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if _, e = s.Step(context.Background()); e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			switch mode {
			case "signature":
				var proof serviceorigin.SignedRecord
				_ = json.Unmarshal(f.raw, &proof)
				proof.Signature[0] ^= 1
				f.raw, _ = json.Marshal(proof)
			case "reader-replaced":
				f.generation++
			default:
				f.mode = mode
			}
			f.mu.Unlock()
			if _, e = s.Step(context.Background()); e == nil {
				t.Fatal("accepted refused current read")
			}
			if _, e = s.Step(context.Background()); e == nil {
				t.Fatal("faulted reader reused")
			}
		})
	}
}
