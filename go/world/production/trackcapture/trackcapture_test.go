package trackcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type testClock struct{ w trustedtime.Interval }

func (c *testClock) Now() (trustedtime.Interval, error) { return c.w, nil }

type fixture struct {
	cfg        Config
	clock      *testClock
	anchor     time.Time
	publishers map[string]*serviceorigin.Publisher
	verifier   *serviceorigin.Verifier
	png, jpeg  []byte
	allowed    bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	physical := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, physical, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{clock: &testClock{trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "track-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-issued-authenticated-time"}}, anchor: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), publishers: map[string]*serviceorigin.Publisher{}, allowed: true}
	f.cfg = Config{Tenant: id.Tenant, Site: "studio-01", Tracking: tracking.Config{TrackerID: "people-session", RunID: "scene", ClockDomain: "simulation:scene", Frame: world.FrameRef{ID: "floor", Revision: "1"}}, Projection: pose.Config{AnkleHeightM: .08, FloorUncertaintyM: .08}, Sources: map[string]world.Source{}, ModelID: "test-pose", ModelVersion: "1", ModelSHA256: strings.Repeat("a", 64)}
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 320, 240)))
	f.png = bytes.Clone(b.Bytes())
	b.Reset()
	_ = jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 320, 240)), nil)
	f.jpeg = bytes.Clone(b.Bytes())
	scopes := map[string]serviceorigin.Scope{}
	for _, sensor := range []string{"front", "oblique"} {
		scope := serviceorigin.Scope{Tenant: id.Tenant, Site: f.cfg.Site, Device: id.Principal, App: "world-people", Service: "capture-" + sensor, DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: sensor}}}
		scopes[scope.Service] = scope
		pub, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 7, Lifetime: 10 * time.Minute, Clock: f.clock, Certificates: certificates, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = pub.Close() })
		f.publishers[sensor] = pub
		f.cfg.Sources[sensor] = world.Source{Device: scope.Device, App: scope.App, Service: scope.Service, Stream: sensor}
	}
	f.verifier, err = serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: id.Tenant, Site: f.cfg.Site, ExecutionDomain: "simulation", Certificates: certificates, Clock: f.clock, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, d wendycert.DeviceIdentity) error {
		if !f.allowed || !reflect.DeepEqual(c.Scope, scopes[c.Scope.Service]) || c.Epoch != 7 || d.Principal != id.Principal {
			return serviceorigin.ErrDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) input(t *testing.T, sensor string, native uint64, at time.Duration, wire bool) Input {
	t.Helper()
	x := 0.
	if sensor == "oblique" {
		x = 1
	}
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("scene/%s/%d", sensor, native), SiteID: f.cfg.Site, Frame: f.cfg.Tracking.Frame, SensorID: sensor, Kind: "camera", Sequence: native, SampleNS: int64(at), RunID: "scene", ScenarioVersion: "scene-v1", Seed: 7, PackageSHA256: strings.Repeat("b", 64), SensorModel: "mujoco-rgb", PositionM: []float64{x, 2, 0}, UncertaintyM: .02, CalibrationRevision: "cal-3", Camera: &sensing.Camera{Width: 320, Height: 240, FX: 200, FY: 200, CX: 160, CY: 120, CameraToSite: []float64{1, 0, 0, x, 0, 0, -1, 2, 0, 1, 0, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}, Evidence: &sensing.EvidenceRef{SHA256: digest(f.png), ContentType: "image/png", Size: int64(len(f.png))}}
	raw, _ := json.MarshalIndent(sample, "", "  ")
	capture := sensorrecord.Capture{Version: 1, SceneBootID: "scene-boot", CapturedUnixNS: f.clock.w.Earliest.Add(-100 * time.Millisecond).UnixNano(), SimulationAnchorUnixNS: f.anchor.UnixNano(), SampleJSON: raw}
	data := f.png
	var err error
	if wire {
		capture, err = sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{ChannelID: 1, Seq: 19, TsUs: uint64(capture.CapturedUnixNS / 1000), Payload: f.jpeg})
		if err != nil {
			t.Fatal(err)
		}
		data = f.jpeg
	}
	request, err := sensorrecord.Publication(capture, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := f.publishers[sensor].Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := f.verifier.Verify(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := json.MarshalIndent(signed, "", " ")
	poses := pose.ImageResult{ModelID: f.cfg.ModelID, ModelVersion: f.cfg.ModelVersion, ModelSHA256: f.cfg.ModelSHA256, ImageSHA256: digest(data), ImageWidth: 320, ImageHeight: 240, Detections: []pose.Person{{Score: .9, LeftAnkle: pose.Keypoint{X: 150 - 100*x, Y: 140, Confidence: .9}, RightAnkle: pose.Keypoint{X: 170 - 100*x, Y: 140, Confidence: .9}}}}
	return Input{Envelope: envelope, Verified: verified, Image: bytes.Clone(data), Poses: poses}
}
func (f *fixture) session(t *testing.T) *Session {
	t.Helper()
	s, e := New(f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func (f *fixture) step(t *testing.T, s *Session, at time.Duration, inputs ...Input) Snapshot {
	t.Helper()
	out, e := s.Step(f.anchor.Add(at), f.clock.w, inputs)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func (f *fixture) reverify(t *testing.T, in Input) Input {
	t.Helper()
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(in.Envelope, &signed)
	v, e := f.verifier.Verify(context.Background(), signed)
	if e != nil {
		t.Fatal(e)
	}
	in.Verified = v
	return in
}
func (f *fixture) build(t *testing.T, s Snapshot) (serviceorigin.PublishRequest, Payload, []resultrecord.ValidatedInput) {
	t.Helper()
	if len(s.Tracks) == 0 {
		t.Fatal("missing track")
	}
	p, inputs, e := s.BuildPublication(s.Tracks[0].ID, "tracks", f.clock.w.Latest.Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	body, e := ValidatePublication(p)
	if e != nil {
		t.Fatal(e)
	}
	return p, body, inputs
}

func TestSignedCapturePublicationPreservesOriginalProofAndCameraLineage(t *testing.T) {
	f := setup(t)
	a, b := f.input(t, "front", 17, 250*time.Millisecond, false), f.input(t, "oblique", 41, 250*time.Millisecond, true)
	snap := f.step(t, f.session(t), 500*time.Millisecond, a, b)
	request, body, inputs := f.build(t, snap)
	if len(snap.Tracks) != 1 || len(body.Current) != 2 || body.MotionKnown || len(inputs) != 2 {
		t.Fatalf("bad current observation %+v", body)
	}
	if _, e := resultrecord.ValidatePublication(request, inputs); e != nil {
		t.Fatal(e)
	}
	for i, in := range []Input{a, b} {
		m := body.Current[i]
		var signed serviceorigin.SignedRecord
		_ = json.Unmarshal(in.Envelope, &signed)
		if m.Proof.EnvelopeSHA256 != digest(in.Envelope) || m.Proof.RecordSHA256 != digest(signed.Record) || m.Proof.Version != (world.Version{Epoch: 7, Sequence: 1}) || m.Proof.Key.Source != f.cfg.Sources[m.SensorID] || m.ModelSHA256 != f.cfg.ModelSHA256 || m.CalibrationRevision != "cal-3" || m.SampleID != snap.Tracks[0].Members[i].SampleID || !m.SampleTime.Equal(f.anchor.Add(250*time.Millisecond)) {
			t.Fatal("original lineage changed")
		}
	}
	if body.Current[0].CaptureSequence != 17 || body.Current[1].CaptureSequence != 41 || body.Current[1].OriginalImage == nil || body.Current[1].Image.SHA256 != digest(f.jpeg) || body.Current[1].OriginalImage.SHA256 != digest(f.png) {
		t.Fatal("native/agent version or SensorLink image lineage lost")
	}
	if !request.Timing.SampleTime.Equal(f.anchor.Add(250*time.Millisecond)) || !body.ValidUntil.Equal(f.anchor.Add(1500*time.Millisecond)) || request.TTL != DefaultLifetime {
		t.Fatal("source timestamps or deadline restamped")
	}
	if body.PhysicalValidUntil().After(f.clock.w.Earliest.Add(4900 * time.Millisecond)) {
		t.Fatal("physical age extended")
	}
	if bytes.Contains(request.Payload, []byte("left_ankle")) {
		t.Fatal("pose pixels leaked")
	}
}

func TestTwoObservationMotionRequiresFreshExactPreviousProofs(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	first := f.step(t, s, 500*time.Millisecond, f.input(t, "front", 17, 250*time.Millisecond, false), f.input(t, "oblique", 17, 250*time.Millisecond, false))
	_, initial, _ := f.build(t, first)
	support := s.Support(f.anchor.Add(750 * time.Millisecond))
	if len(support) != 2 {
		t.Fatal("missing bounded support")
	}
	for i := range support {
		support[i] = f.reverify(t, support[i])
	}
	a, b := f.input(t, "front", 18, 750*time.Millisecond, false), f.input(t, "oblique", 18, 750*time.Millisecond, false)
	for _, in := range []*Input{&a, &b} {
		in.Poses.Detections[0].LeftAnkle.X += 10
		in.Poses.Detections[0].RightAnkle.X += 10
	}
	second := f.step(t, s, 750*time.Millisecond, append([]Input{a, b}, support...)...)
	request, body, inputs := f.build(t, second)
	if body.TrackID != initial.TrackID || !body.MotionKnown || body.VelocityXZ == nil || body.VelocityXZ[0] <= 0 || len(body.Previous) != 2 || len(inputs) != 4 {
		t.Fatalf("missing two-observation motion %+v", body)
	}
	if !body.ValidUntil.Equal(initial.ValidUntil) || !request.Timing.SampleTime.Equal(initial.ObservedAt) {
		t.Fatal("motion extended preceding evidence lifetime")
	}
	if _, e := resultrecord.ValidatePublication(request, inputs); e != nil {
		t.Fatal(e)
	}
	// A new position remains usable when previous originals were not supplied.
	third := f.step(t, s, time.Second, f.input(t, "front", 19, time.Second, false), f.input(t, "oblique", 19, time.Second, false))
	_, without, _ := f.build(t, third)
	if without.MotionKnown || without.VelocityXZ != nil || len(without.Previous) != 0 {
		t.Fatal("cached authority or old filter velocity published")
	}
}

func TestDuplicateCoastAndExpiredSupportCannotRenewEvidence(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	a := f.input(t, "front", 17, 250*time.Millisecond, false)
	first := f.step(t, s, 500*time.Millisecond, a)
	id := first.Tracks[0].ID
	duplicate := f.step(t, s, time.Second, a)
	if duplicate.Tracks[0].State != "coasting" {
		t.Fatal("duplicate refreshed")
	}
	if _, _, e := duplicate.BuildPublication(id, "tracks", f.clock.w.Latest.Add(time.Second)); e == nil {
		t.Fatal("coasting published as observed")
	}
	if out := f.step(t, s, 1500*time.Millisecond); len(out.Tracks) != 0 {
		t.Fatal("equal expiry survived")
	}
	// Old support that expires after the runner's verification is discarded.
	old := own(a)
	old.SupportOnly = true
	now := f.step(t, s, 1600*time.Millisecond, f.input(t, "front", 18, 1600*time.Millisecond, false), old)
	_, body, _ := f.build(t, now)
	if body.MotionKnown {
		t.Fatal("expired support became motion")
	}
}

func TestSupportPhysicalExpiryKeepsFreshPositionWithoutMotion(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	f.step(t, s, 500*time.Millisecond, f.input(t, "front", 17, 250*time.Millisecond, false))
	support := s.Support(f.anchor.Add(750 * time.Millisecond))
	if len(support) != 1 {
		t.Fatal("missing support")
	}
	f.clock.w.Earliest = f.clock.w.Earliest.Add(5 * time.Second)
	f.clock.w.Latest = f.clock.w.Latest.Add(5 * time.Second)
	f.clock.w.Monotonic += 5 * time.Second
	current := f.input(t, "front", 18, 750*time.Millisecond, false)
	support[0] = f.reverify(t, support[0])
	snap := f.step(t, s, 750*time.Millisecond, current, support[0])
	_, body, inputs := f.build(t, snap)
	if body.MotionKnown || len(inputs) != 1 || !body.ObservedAt.Equal(f.anchor.Add(750*time.Millisecond)) {
		t.Fatal("expired previous capture prevented current-only result")
	}
	if _, e := f.session(t).Step(f.anchor.Add(time.Second), f.clock.w, []Input{support[0]}); e != nil {
		t.Fatal("expired support should be omitted", e)
	}
	support[0].SupportOnly = false
	if _, e := f.session(t).Step(f.anchor.Add(time.Second), f.clock.w, []Input{support[0]}); e == nil {
		t.Fatal("expired current capture accepted")
	}
}

func TestPublishedPositionDoesNotBorrowRecursiveFilterHistory(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	f.step(t, s, 500*time.Millisecond, f.input(t, "front", 17, 250*time.Millisecond, false))
	in := f.input(t, "front", 18, 750*time.Millisecond, false)
	in.Poses.Detections[0].LeftAnkle.X += 20
	in.Poses.Detections[0].RightAnkle.X += 20
	out := f.step(t, s, 750*time.Millisecond, in)
	_, body, _ := f.build(t, out)
	if body.PositionXZ != body.Current[0].PositionXZ || body.Covariance != body.Current[0].Covariance || body.MotionKnown {
		t.Fatal("current publication contains historical filter state")
	}
}

func TestRefusesChangedOriginalModelImageAndScope(t *testing.T) {
	f := setup(t)
	original := f.input(t, "front", 17, 250*time.Millisecond, false)
	for name, change := range map[string]func(*Input){"verified bytes": func(in *Input) { in.Verified.Bytes[0] = ' ' }, "verified source": func(in *Input) { in.Verified.Record.Scope.Service = "impostor" }, "image": func(in *Input) { in.Image[0] ^= 1 }, "pose digest": func(in *Input) { in.Poses.ImageSHA256 = strings.Repeat("d", 64) }, "model": func(in *Input) { in.Poses.ModelSHA256 = strings.Repeat("e", 64) }, "frame": func(in *Input) { in.Verified.Record.Publication.Frame.Revision = "different" }} {
		t.Run(name, func(t *testing.T) {
			in := own(original)
			change(&in)
			if _, e := f.session(t).Step(f.anchor.Add(500*time.Millisecond), f.clock.w, []Input{in}); e == nil {
				t.Fatal("accepted changed original")
			}
		})
	}
	f.allowed = false
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(original.Envelope, &signed)
	if _, e := f.verifier.Verify(context.Background(), signed); e == nil {
		t.Fatal("test verifier ignored current source denial")
	}
}

func TestTypedPayloadRejectsPrivacyExtensionsAndRewrittenBounds(t *testing.T) {
	f := setup(t)
	snap := f.step(t, f.session(t), 500*time.Millisecond, f.input(t, "front", 17, 250*time.Millisecond, false))
	request, _, _ := f.build(t, snap)
	for _, name := range []string{"bbox", "appearance", "short position", "missing motion flag", "deadline", "proof", "image", "physical"} {
		t.Run(name, func(t *testing.T) {
			p := request
			r, e := resultrecord.Decode(p.Payload)
			if e != nil {
				t.Fatal(e)
			}
			var body map[string]any
			_ = json.Unmarshal(r.ResultJSON, &body)
			switch name {
			case "bbox":
				body["bbox"] = []int{0, 0, 20, 30}
			case "appearance":
				body["embedding"] = []float64{.1, .2}
			case "short position":
				body["position_xz"] = []float64{1}
			case "missing motion flag":
				delete(body, "motion_known")
			case "deadline":
				body["valid_until"] = f.anchor.Add(2 * time.Second).Format(time.RFC3339Nano)
			case "proof":
				body["current"].([]any)[0].(map[string]any)["proof"].(map[string]any)["envelope_sha256"] = strings.Repeat("f", 64)
			case "image":
				body["current"].([]any)[0].(map[string]any)["image"].(map[string]any)["content_type"] = "application/octet-stream"
			case "physical":
				body["current"].([]any)[0].(map[string]any)["physical_until"] = f.clock.w.Latest.Add(time.Hour).Format(time.RFC3339Nano)
			}
			r.ResultJSON, _ = json.Marshal(body)
			p.Payload, e = resultrecord.Encode(r)
			if e != nil {
				t.Fatal(e)
			}
			if _, e := ValidatePublication(p); e == nil {
				t.Fatal("accepted invalid track payload")
			}
		})
	}
}
