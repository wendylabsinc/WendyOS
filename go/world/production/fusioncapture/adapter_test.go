package fusioncapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type clock struct{ interval trustedtime.Interval }

func (c *clock) Now() (trustedtime.Interval, error) { return c.interval, nil }

type clockFunc func() (trustedtime.Interval, error)

func (f clockFunc) Now() (trustedtime.Interval, error) { return f() }

func advancePhysical(c *clock, duration time.Duration) {
	c.interval.Earliest = c.interval.Earliest.Add(duration)
	c.interval.Latest = c.interval.Latest.Add(duration)
	c.interval.Monotonic += duration
}

type fixture struct {
	cfg         fusion.Config
	proofs      [][]byte
	media       map[string][]byte
	verify      VerifyFunc
	batchVerify VerifyBatchFunc
	allowed     bool
	clock       *clock
}

func hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func imageBytes(t *testing.T, jpegImage bool) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			im.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var b bytes.Buffer
	var err error
	if jpegImage {
		err = jpeg.Encode(&b, im, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&b, im)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func wavBytes() []byte {
	const count = 2000
	raw := make([]byte, 44+count*2)
	copy(raw, "RIFF")
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	copy(raw[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(raw[16:], 16)
	binary.LittleEndian.PutUint16(raw[20:], 1)
	binary.LittleEndian.PutUint16(raw[22:], 1)
	binary.LittleEndian.PutUint32(raw[24:], 8000)
	binary.LittleEndian.PutUint32(raw[28:], 16000)
	binary.LittleEndian.PutUint16(raw[32:], 2)
	binary.LittleEndian.PutUint16(raw[34:], 16)
	copy(raw[36:], "data")
	binary.LittleEndian.PutUint32(raw[40:], count*2)
	for i := range count {
		binary.LittleEndian.PutUint16(raw[44+i*2:], uint16(int16(10000*math.Sin(2*math.Pi*880*float64(i)/8000))))
	}
	return raw
}

func setup(t *testing.T) *fixture { return setupLidar(t, false) }

func setupLidar(t *testing.T, withLidar bool) *fixture {
	t.Helper()
	physical := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	identity := testidentity.NewWith(t, physical, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: identity.Tenant, Roots: [][]byte{identity.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Unix(1700000000, 0).UTC()
	f := &fixture{allowed: true, media: map[string][]byte{}, clock: &clock{trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "physical-test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "explicit-test-time"}}}
	f.cfg = fusion.Config{Tenant: identity.Tenant, Site: "synthetic-office", RunID: "run-a", ClockDomain: "simulation:run-a", Frame: world.FrameRef{ID: "floor", Revision: "frame-v1"}, Sources: map[string]world.Source{}, Now: func() time.Time { return anchor.Add(500 * time.Millisecond) }}
	scopes := map[string]serviceorigin.Scope{}
	sensorIDs := []string{"camera-a", "camera-b", "gas-1", "audio-1"}
	if withLidar {
		sensorIDs = append(sensorIDs, "lidar-a", "lidar-b", "lidar-c")
		f.cfg.RequiredSensors = map[string]string{}
	}
	for _, sensorID := range sensorIDs {
		sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run-a/" + sensorID + "/17", SiteID: f.cfg.Site, Frame: f.cfg.Frame, SensorID: sensorID, Sequence: 17, SampleNS: int64(250 * time.Millisecond), RunID: f.cfg.RunID, ScenarioVersion: "mujoco-live-v1", Seed: 19, PackageSHA256: strings.Repeat("a", 64), SensorModel: "explicit-development-fixture", PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "cal-1"}
		var media []byte
		contentType := ""
		switch sensorID {
		case "camera-a", "camera-b":
			sample.Kind = "camera"
			sample.Camera = &sensing.Camera{Width: 8, Height: 6, FX: 8, FY: 8, CX: 4, CY: 3, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
			media, contentType = imageBytes(t, false), "image/png"
		case "gas-1":
			sample.Kind, sample.Gas = "gas", &sensing.Gas{ValuePPM: 80, UncertaintyPPM: 2, Species: "synthetic-gas"}
		case "audio-1":
			sample.Kind, sample.Audio = "audio", &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
			media, contentType = wavBytes(), "audio/wav"
		default:
			sample.Kind = "lidar"
			sample.PositionM = []float64{0, 0, 0}
			sample.Lidar = &sensing.Lidar{MaxRangeM: 6, PointsM: make([][3]float64, 144)}
			for i := range sample.Lidar.PointsM {
				sample.Lidar.PointsM[i] = [3]float64{float64(i%12-6) * .01, float64(i/12-6) * .01, 4.000000000000001}
			}
		}
		if withLidar {
			f.cfg.RequiredSensors[sensorID] = sample.Kind
			if sample.Camera != nil {
				x, pixel := -1.0, 5
				if sensorID == "camera-b" {
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
		}
		if media != nil {
			sample.Evidence = &sensing.EvidenceRef{SHA256: hash(media), ContentType: contentType, Size: int64(len(media))}
		}
		sampleJSON, _ := json.MarshalIndent(sample, "", "  ")
		capture := sensorrecord.Capture{Version: sensorrecord.Version, SceneBootID: "scene-boot-a", CapturedUnixNS: physical.UnixNano(), SimulationAnchorUnixNS: anchor.UnixNano(), SampleJSON: sampleJSON}
		if sensorID == "camera-b" && !withLidar {
			media = imageBytes(t, true)
			capture, err = sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{ChannelID: 9, Seq: 99, TsUs: uint64(physical.UnixMicro()), Flags: 1, Payload: media})
			if err != nil {
				t.Fatal(err)
			}
		}
		if media != nil {
			f.media[hash(media)] = media
		}
		request, err := sensorrecord.Publication(capture, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		scope := serviceorigin.Scope{Tenant: identity.Tenant, Site: f.cfg.Site, Device: identity.Principal, App: "com.wendy.world.multimodal", Service: "reader-" + sensorID, DeploymentGeneration: 2, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: sensorID}}}
		scopes[scope.Service] = scope
		publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 29, Lifetime: 10 * time.Minute, Clock: f.clock, Certificates: certificates, ChainDER: identity.ChainDER, DeviceKey: identity.Key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = publisher.Close() })
		signed, err := publisher.Publish(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		// Preserve the original, noncompact envelope too; signatures cover the
		// base64-decoded immutable bytes, input proof hashes cover this envelope.
		proof, _ := json.MarshalIndent(signed, "", " ")
		f.proofs = append(f.proofs, proof)
		f.cfg.Sources[sensorID] = world.Source{Device: scope.Device, App: scope.App, Service: scope.Service, Stream: sensorID}
	}
	authorize := func(_ context.Context, claims serviceorigin.DelegationClaims, device wendycert.DeviceIdentity) error {
		if !f.allowed || !reflect.DeepEqual(claims.Scope, scopes[claims.Scope.Service]) || device.Principal != identity.Principal || claims.Epoch != 29 {
			return serviceorigin.ErrDenied
		}
		return nil
	}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: identity.Tenant, Site: f.cfg.Site, ExecutionDomain: "simulation", Certificates: certificates, Clock: f.clock, Authorize: authorize})
	if err != nil {
		t.Fatal(err)
	}
	f.verify = verifier.Verify
	batch, err := serviceorigin.NewBatchVerifier(serviceorigin.BatchVerifierConfig{Tenant: identity.Tenant, Site: f.cfg.Site, ExecutionDomain: "simulation", Certificates: certificates, Clock: f.clock, Authorize: func(ctx context.Context, batch serviceorigin.BatchAuthorization) error {
		if len(batch.Claims) != len(sensorIDs) || len(batch.Devices) != len(sensorIDs) {
			return serviceorigin.ErrDenied
		}
		for i, claim := range batch.Claims {
			if err := authorize(ctx, claim, batch.Devices[i]); err != nil {
				return err
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.batchVerify = batch.VerifyBatch
	return f
}

func (f *fixture) load(_ context.Context, ref sensing.EvidenceRef) ([]byte, error) {
	data, ok := f.media[ref.SHA256]
	if !ok {
		return nil, errors.New("missing fixture media")
	}
	return bytes.Clone(data), nil
}

func TestActualAgentProofsPreserveSourceNativeSequenceAndWireEvidence(t *testing.T) {
	f := setup(t)
	result, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load)
	if err != nil || result.State != fusion.HazardDetected {
		t.Fatalf("fusion: %v %+v", err, result)
	}
	proofs := map[string]bool{}
	for _, proof := range f.proofs {
		proofs[hash(proof)] = true
	}
	for _, input := range result.Hazard.Inputs {
		if input.Source != f.cfg.Sources[input.SensorID] || input.Version != (world.Version{Epoch: 29, Sequence: 1}) || input.Sample.Sequence != 17 || input.Capture == nil || !proofs[input.Capture.ProofSHA256] || input.Capture.SceneBootID != "scene-boot-a" {
			t.Fatalf("lost proof or agent/native identity: %+v", input)
		}
		if input.SensorID == "camera-b" {
			if input.Evidence.ContentType != "image/jpeg" || input.Capture.OriginalEvidence == nil || input.Capture.OriginalEvidence.ContentType != "image/png" || input.Evidence.SHA256 == input.Capture.OriginalEvidence.SHA256 {
				t.Fatal("received JPEG confused with original PNG", input)
			}
		} else if input.Capture.OriginalEvidence != nil {
			t.Fatal("invented a source conversion for direct capture")
		}
	}
	derived, err := result.Component(world.Source{Device: "fusion", App: "fusion", Service: "fusion", Stream: "hazards"}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if derived.TTL > 2*time.Second || len(derived.Provenance.Inputs) != 4 || derived.Provenance.Origin != world.Inferred {
		t.Fatal("derived lifetime or ancestry changed")
	}
	for _, input := range derived.Provenance.Inputs {
		if !proofs[strings.TrimPrefix(input.ID, "sha256:")] || input.Origin != world.Simulated {
			t.Fatal("input does not identify exact original device proof", input)
		}
	}
}

func TestPrepareRequiresExactImmutableRecordAndDoesNotRewriteComponent(t *testing.T) {
	f := setup(t)
	records, decode, err := Prepare(context.Background(), Options{Physical: f.clock}, f.proofs, f.verify)
	if err != nil {
		t.Fatal(err)
	}
	for i, component := range records {
		before, _ := json.Marshal(component)
		value, err := decode(component)
		if err != nil || value.ProofSHA256 != hash(f.proofs[i]) {
			t.Fatal("exact proof not retained", err)
		}
		after, _ := json.Marshal(component)
		if !bytes.Equal(before, after) {
			t.Fatal("decoder rewrote component")
		}
		value.Sample.PositionM[0]++
		unchanged, _ := decode(component)
		if unchanged.Sample.PositionM[0] != 1 {
			t.Fatal("decoder exposed mutable capture cache")
		}
	}
	for name, change := range map[string]func(*world.Component){
		"native sequence substituted": func(c *world.Component) { c.Version.Sequence = 17 },
		"source":                      func(c *world.Component) { c.Key.Source.Service += "-other" },
		"clock":                       func(c *world.Component) { c.Timing.SampleTime = c.Timing.SampleTime.Add(time.Nanosecond) },
		"age":                         func(c *world.Component) { c.Timing.AgeHint++ },
		"payload":                     func(c *world.Component) { c.Payload = append(bytes.Clone(c.Payload), ' ') },
	} {
		t.Run(name, func(t *testing.T) {
			bad := records[0]
			change(&bad)
			if _, err := decode(bad); err == nil {
				t.Fatal("accepted rewritten proof projection")
			}
		})
	}
	wrong := func(ctx context.Context, s serviceorigin.SignedRecord) (serviceorigin.VerifiedRecord, error) {
		verified, err := f.verify(ctx, s)
		verified.Record.Sequence++
		return verified, err
	}
	if _, _, err := Prepare(context.Background(), Options{Physical: f.clock}, f.proofs, wrong); err == nil {
		t.Fatal("accepted verifier projection inconsistent with immutable proof")
	}
}

func TestWrapperRefusesTamperingRevocationExpiryAndMissingMedia(t *testing.T) {
	t.Run("signature", func(t *testing.T) {
		f := setup(t)
		var signed serviceorigin.SignedRecord
		_ = json.Unmarshal(f.proofs[0], &signed)
		signed.Signature[0] ^= 1
		f.proofs[0], _ = json.Marshal(signed)
		if _, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load); err == nil {
			t.Fatal("accepted forged proof")
		}
	})
	t.Run("revocation", func(t *testing.T) {
		f := setup(t)
		f.allowed = false
		if _, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load); err == nil {
			t.Fatal("accepted revoked service authority")
		}
	})
	t.Run("revocation during media retrieval", func(t *testing.T) {
		f := setup(t)
		load := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
			f.allowed = false
			return f.load(ctx, ref)
		}
		if _, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, load); err == nil {
			t.Fatal("returned derived result after authority changed during media retrieval")
		}
	})
	t.Run("expiry", func(t *testing.T) {
		f := setup(t)
		f.clock.interval.Earliest = f.clock.interval.Earliest.Add(11 * time.Minute)
		f.clock.interval.Latest = f.clock.interval.Latest.Add(11 * time.Minute)
		f.clock.interval.Monotonic += 11 * time.Minute
		if _, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load); err == nil {
			t.Fatal("accepted expired proof authority")
		}
	})
	t.Run("media missing", func(t *testing.T) {
		f := setup(t)
		f.media = nil
		_, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load)
		if !errors.Is(err, ErrCaptureMedia) {
			t.Fatalf("missing evidence must remain terminal: %v", err)
		}
	})
	t.Run("source scope", func(t *testing.T) {
		f := setup(t)
		source := f.cfg.Sources["camera-a"]
		source.Service = "other"
		f.cfg.Sources["camera-a"] = source
		result, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load)
		if err != nil || result.State != fusion.InsufficientEvidence {
			t.Fatalf("wrong scope outcome: %v %+v", err, result)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		f := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := Prepare(ctx, Options{Physical: f.clock}, f.proofs, f.verify); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled query continued", err)
		}
	})
}

func TestStrictOriginalProofShapeAndBounds(t *testing.T) {
	f := setup(t)
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(f.proofs[0], &signed)
	canonical, _ := json.Marshal(signed)
	for name, raw := range map[string][]byte{
		"duplicate":  bytes.Replace(canonical, []byte(`"record":`), []byte(`"record":"e30=","record":`), 1),
		"case alias": bytes.Replace(canonical, []byte(`"record":`), []byte(`"Record":`), 1),
		"null":       bytes.Replace(canonical, []byte(`"chain_der":[`), []byte(`"chain_der":null,"extra":[`), 1),
		"unknown":    append(append([]byte{}, canonical[:len(canonical)-1]...), []byte(`,"other":1}`)...),
		"trailing":   append(append([]byte{}, canonical...), []byte(` {}`)...),
		"oversize":   bytes.Repeat([]byte(" "), MaxProofBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Prepare(context.Background(), Options{Physical: f.clock}, [][]byte{raw}, f.verify); err == nil {
				t.Fatal("accepted malformed original proof")
			}
		})
	}
	if _, _, err := Prepare(context.Background(), Options{Physical: f.clock}, f.proofs, nil); err == nil {
		t.Fatal("allowed unverified source proofs")
	}
	if _, _, err := Prepare(context.Background(), Options{Physical: f.clock}, append(f.proofs, canonical), f.verify); err == nil {
		t.Fatal("accepted different original wrapper for same agent component")
	}
}

func TestPausedSimulationCannotPreserveCaptureThroughMediaDelay(t *testing.T) {
	f := setup(t)
	advancePhysical(f.clock, 4900*time.Millisecond)
	sourceBefore := f.cfg.Now()
	changed := false
	load := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		if !changed {
			advancePhysical(f.clock, 200*time.Millisecond)
			changed = true
		}
		return f.load(ctx, ref)
	}
	if _, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, load); err == nil {
		t.Fatal("paused simulation kept a physically old capture live")
	}
	if !f.cfg.Now().Equal(sourceBefore) {
		t.Fatal("test advanced the simulation clock")
	}
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(f.proofs[0], &signed)
	if _, err := f.verify(context.Background(), signed); err != nil {
		t.Fatal("test expired signing authority instead of capture age", err)
	}
}

func TestPreparationChecksPhysicalAgeAfterAllProofWork(t *testing.T) {
	f := setup(t)
	advancePhysical(f.clock, 4900*time.Millisecond)
	calls := 0
	verify := func(ctx context.Context, signed serviceorigin.SignedRecord) (serviceorigin.VerifiedRecord, error) {
		calls++
		if calls == len(f.proofs) {
			advancePhysical(f.clock, 200*time.Millisecond)
		}
		return f.verify(ctx, signed)
	}
	if _, _, err := Prepare(context.Background(), Options{Physical: f.clock}, f.proofs, verify); err == nil {
		t.Fatal("prepared an expired earlier input after later proof work")
	}
	if calls != len(f.proofs) {
		t.Fatal("test did not cross the full proof batch")
	}
}

func TestPhysicalCaptureGuardFaultAndRegressionDuringMedia(t *testing.T) {
	for _, fault := range []string{"unavailable", "unauthenticated", "boot", "monotonic", "earliest", "latest"} {
		t.Run(fault, func(t *testing.T) {
			f := setup(t)
			physical := f.clock.interval
			physical.Earliest = physical.Earliest.Add(4 * time.Second)
			physical.Latest = physical.Latest.Add(4 * time.Second)
			physical.Monotonic += 4 * time.Second
			changed := false
			provider := clockFunc(func() (trustedtime.Interval, error) {
				if changed && fault == "unavailable" {
					return trustedtime.Interval{}, errors.New("lost independent time")
				}
				w := physical
				if changed {
					switch fault {
					case "unauthenticated":
						w.Authenticated = false
					case "boot":
						w.BootID += "-changed"
					case "monotonic":
						w.Monotonic--
					case "earliest":
						w.Earliest = w.Earliest.Add(-time.Nanosecond)
					case "latest":
						w.Latest = w.Latest.Add(-time.Nanosecond)
					}
				}
				return w, nil
			})
			load := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
				changed = true
				return f.load(ctx, ref)
			}
			if _, err := Evaluate(context.Background(), f.cfg, Options{Physical: provider}, f.proofs, f.verify, load); err == nil {
				t.Fatal("capture time failure did not fail closed")
			}
			var signed serviceorigin.SignedRecord
			_ = json.Unmarshal(f.proofs[0], &signed)
			if _, err := f.verify(context.Background(), signed); err != nil {
				t.Fatal("independent verifier should remain valid in this test", err)
			}
		})
	}
	f := setup(t)
	if _, _, err := Prepare(context.Background(), Options{}, f.proofs, f.verify); err == nil {
		t.Fatal("physical time silently omitted")
	}
}

func TestFinalSourceClockReadCannotHidePhysicalCaptureExpiry(t *testing.T) {
	f := setup(t)
	advancePhysical(f.clock, 4900*time.Millisecond)
	source := f.cfg.Now()
	calls := 0
	f.cfg.Now = func() time.Time {
		calls++
		if calls == 4 { // final source read, after the second proof batch
			advancePhysical(f.clock, 200*time.Millisecond)
		}
		return source
	}
	if _, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load); err == nil {
		t.Fatal("blocking final source read returned a physically expired capture")
	}
	if calls != 4 {
		t.Fatal("test did not reach final source-clock read", calls)
	}
}

func TestPhysicalDiagnosticsDistinguishFutureStaleAndBoundedPhases(t *testing.T) {
	f := setup(t)
	base := f.clock.interval.Latest
	for name, capture := range map[string]sensorrecord.Capture{"future": {CapturedUnixNS: base.Add(time.Second).UnixNano()}, "stale": {CapturedUnixNS: base.Add(-6 * time.Second).UnixNano()}} {
		t.Run(name, func(t *testing.T) {
			d := &Diagnostics{}
			g := physicalGuard{provider: f.clock, diagnostics: d, phase: "initial_prepare", started: time.Now()}
			if g.check([]sensorrecord.Capture{capture}) == nil || len(d.Checks) != 1 {
				t.Fatal("age rejection missing")
			}
			p := d.Checks[0]
			if p.Phase != "initial_prepare" || p.Future != (name == "future") || p.Stale != (name == "stale") || p.LatestUnixNS != base.UnixNano() || p.MinCaptureUnixNS != capture.CapturedUnixNS || p.MaxCaptureUnixNS != capture.CapturedUnixNS || p.Failure != "capture_age_refused" {
				t.Fatalf("wrong physical rejection diagnostic: %+v", p)
			}
			for i := 0; i < 12; i++ {
				_ = g.check(nil)
			}
			if len(d.Checks) != 8 {
				t.Fatal("physical diagnostic bound changed")
			}
		})
	}
}
func TestPhysicalDiagnosticsIdentifyFirstAndSecondPreparation(t *testing.T) {
	for _, round := range []int{1, 2} {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			f := setup(t)
			d := &Diagnostics{}
			calls := 0
			loads := 0
			verify := func(ctx context.Context, signed serviceorigin.SignedRecord) (serviceorigin.VerifiedRecord, error) {
				v, e := f.verify(ctx, signed)
				calls++
				if calls == round*4 {
					f.clock.interval.Earliest = f.clock.interval.Earliest.Add(6 * time.Second)
					f.clock.interval.Latest = f.clock.interval.Latest.Add(6 * time.Second)
					f.clock.interval.Monotonic += 6 * time.Second
				}
				return v, e
			}
			_, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock, Diagnostics: d}, f.proofs, verify, func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) { loads++; return f.load(ctx, ref) })
			if err == nil || len(d.Checks) == 0 {
				t.Fatal("expired capture succeeded")
			}
			last := d.Checks[len(d.Checks)-1]
			want := "initial_prepare"
			if round == 2 {
				want = "final_prepare"
			}
			if last.Phase != want || !last.Stale || last.Future || last.CaptureCount != 4 {
				t.Fatalf("wrong expired batch checkpoint: %+v", last)
			}
			if round == 1 && loads != 0 || round == 2 && loads != 3 {
				t.Fatal("phase label disagrees with evidence work")
			}
		})
	}
}
