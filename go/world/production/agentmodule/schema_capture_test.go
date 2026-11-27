package agentmodule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// These tests exercise schema admission after cryptographic verification. They
// construct VerifiedRecord directly and make no enrollment or socket claim.
func captureSchemaManager(t *testing.T) (*Manager, *moduleClock) {
	t.Helper()
	policy := basePolicy()
	policy.ExecutionDomain, policy.SampleClockDomain = "simulation", "simulation:run-a"
	clock := moduleTestClock()
	m := &Manager{cfg: Config{Policy: policy, CaptureSource: &CaptureSource{
		RunID: "run-a", BootID: "boot-original", PackageSHA256: strings.Repeat("a", 64),
		AnchorUnixNS: moduleTestNow.Add(-time.Hour).UnixNano(), Frame: policy.Frames[0],
	}}, deps: Dependencies{Clock: clock, PolicyCurrent: func(context.Context) error { return nil }}}
	return m, clock
}

func captureSchemaFixture(t *testing.T, kind string) (sensorrecord.Capture, []byte) {
	t.Helper()
	id := map[string]string{"camera": "camera-a", "audio": "audio-a", "gas": "gas-a"}[kind]
	s := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run-a/" + id + "/17", SiteID: "office",
		Frame: basePolicy().Frames[0], SensorID: id, Kind: kind, Sequence: 17,
		SampleNS: int64(250 * time.Millisecond), RunID: "run-a", ScenarioVersion: "mujoco-4-live-v1", Seed: 7,
		PackageSHA256: strings.Repeat("a", 64), SensorModel: "test-" + kind,
		PositionM: []float64{-2, .2, 0}, UncertaintyM: .04, CalibrationRevision: "cal-v1"}
	var data []byte
	switch kind {
	case "camera":
		s.Camera = &sensing.Camera{Width: 4, Height: 3, FX: 4, FY: 4, CX: 2, CY: 1.5,
			CameraToSite:          []float64{1, 0, 0, -2, 0, 1, 0, .2, 0, 0, 1, 0, 0, 0, 0, 1},
			CalibrationValidForNS: int64(time.Minute)}
		var b bytes.Buffer
		if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
			t.Fatal(err)
		}
		data = b.Bytes()
		s.Evidence = captureSchemaReference(data, "image/png")
	case "audio":
		s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
		data = make([]byte, 44+4000)
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
		s.Evidence = captureSchemaReference(data, "audio/wav")
	case "gas":
		s.Gas = &sensing.Gas{ValuePPM: 73.25, UncertaintyPPM: 2.7, Species: "synthetic-test-gas"}
	default:
		t.Fatal("unknown fixture kind")
	}
	c := sensorrecord.Capture{Version: sensorrecord.Version, SceneBootID: "boot-original",
		CapturedUnixNS: moduleTestNow.UnixNano(), SimulationAnchorUnixNS: moduleTestNow.Add(-time.Hour).UnixNano()}
	captureSchemaSetSample(t, &c, s)
	if err := sensorrecord.ValidateMedia(c, data); err != nil {
		t.Fatal(err)
	}
	return c, data
}

func captureSchemaReference(data []byte, mime string) *sensing.EvidenceRef {
	h := sha256.Sum256(data)
	return &sensing.EvidenceRef{SHA256: hex.EncodeToString(h[:]), Size: int64(len(data)), ContentType: mime}
}

func captureSchemaSetSample(t *testing.T, c *sensorrecord.Capture, s sensing.Sample) {
	t.Helper()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberate whitespace detects reconstruction of the original capture JSON.
	c.SampleJSON = append(append([]byte("\n"), raw...), '\n')
}

func captureSchemaRecord(t *testing.T, m *Manager, c sensorrecord.Capture) serviceorigin.VerifiedRecord {
	t.Helper()
	p, err := sensorrecord.Publication(c, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return serviceorigin.VerifiedRecord{Record: serviceorigin.Record{Version: serviceorigin.Version,
		AuthorityDomain: serviceorigin.AuthorityDomain, Epoch: 29, Sequence: 3,
		Scope: serviceorigin.Scope{Tenant: m.cfg.Policy.Tenant, Site: m.cfg.Policy.Site, Device: m.cfg.Policy.Device,
			App: "com.example.sensors", Service: "capture", DeploymentGeneration: 37, ExecutionDomain: "simulation",
			Publish: []serviceorigin.PublishScope{{ComponentType: p.ComponentType, Stream: p.Stream}}}, Publication: p}}
}

func captureSchemaOwner(m *Manager, v serviceorigin.VerifiedRecord) serviceevidence.Owner {
	s := v.Record.Scope
	return m.owner(serviceaccess.Source{App: s.App, Service: s.Service, ComponentType: v.Record.Publication.ComponentType, Stream: v.Record.Publication.Stream})
}

func captureSchemaStore(t *testing.T, m *Manager) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "media")
	m.evidence, err = serviceevidence.Open(serviceevidence.Config{Directory: dir,
		Tenant: m.cfg.Policy.Tenant, Site: m.cfg.Policy.Site, Device: m.cfg.Policy.Device,
		Clock: m.deps.Clock, QuotaBytes: 1 << 20, MaxBlobBytes: 128 << 10, MaxEntries: 16, MaxRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.evidence.Close(); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func captureSchemaPut(t *testing.T, m *Manager, owner serviceevidence.Owner, ref sensing.EvidenceRef, data []byte, complete bool) {
	t.Helper()
	got, err := m.evidence.Put(context.Background(), owner, serviceevidence.PutRequest{
		SHA256: ref.SHA256, Size: ref.Size, Data: data, ContentType: ref.ContentType})
	if err != nil || got.Complete != complete {
		t.Fatalf("media upload: complete=%v, want=%v, error=%v", got.Complete, complete, err)
	}
}

func TestCaptureSchemaKeepsOriginalSequenceAndBytes(t *testing.T) {
	for _, kind := range []string{"gas", "camera", "audio"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := captureSchemaManager(t)
			capture, data := captureSchemaFixture(t, kind)
			v := captureSchemaRecord(t, m, capture)
			if len(data) > 0 {
				captureSchemaStore(t, m)
				ref, _ := capture.Evidence()
				captureSchemaPut(t, m, captureSchemaOwner(m, v), *ref, data, true)
			}
			if err := m.admitSchema(context.Background(), v); err != nil {
				t.Fatal(err)
			}
			got, err := sensorrecord.FromComponent(v.Component())
			if err != nil {
				t.Fatal(err)
			}
			sample, _ := got.Sample()
			if sample.Sequence != 17 || sample.ID != "run-a/"+sample.SensorID+"/17" || v.Component().Version.Sequence != 3 || v.Component().Version.Epoch != 29 || !bytes.Equal(got.SampleJSON, capture.SampleJSON) || got.CapturedUnixNS != capture.CapturedUnixNS || got.SimulationAnchorUnixNS != capture.SimulationAnchorUnixNS {
				t.Fatal("admission changed original capture identity, bytes, or clocks")
			}
			if !v.Component().Timing.SampleTime.Equal(time.Unix(0, capture.SimulationAnchorUnixNS+sample.SampleNS)) {
				t.Fatal("capture time was rebased to admission time")
			}
		})
	}
}

func TestCaptureSchemaRequiresCommissionedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Manager)
	}{
		{"no-binding", func(m *Manager) { m.cfg.CaptureSource = nil }},
		{"wrong-boot", func(m *Manager) { m.cfg.CaptureSource.BootID = "boot-restarted" }},
		{"missing-boot", func(m *Manager) { m.cfg.CaptureSource.BootID = "" }},
		{"wrong-run", func(m *Manager) { m.cfg.CaptureSource.RunID = "run-b" }},
		{"missing-run", func(m *Manager) { m.cfg.CaptureSource.RunID = "" }},
		{"wrong-package", func(m *Manager) { m.cfg.CaptureSource.PackageSHA256 = strings.Repeat("b", 64) }},
		{"missing-package", func(m *Manager) { m.cfg.CaptureSource.PackageSHA256 = "" }},
		{"wrong-anchor", func(m *Manager) { m.cfg.CaptureSource.AnchorUnixNS++ }},
		{"missing-anchor", func(m *Manager) { m.cfg.CaptureSource.AnchorUnixNS = 0 }},
		{"physical-execution", func(m *Manager) { m.cfg.Policy.ExecutionDomain = "physical" }},
		{"wrong-clock-domain", func(m *Manager) { m.cfg.Policy.SampleClockDomain = "simulation:run-b" }},
		{"uncommissioned-frame", func(m *Manager) { m.cfg.Policy.Frames[0].Revision = "survey-2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := captureSchemaManager(t)
			capture, _ := captureSchemaFixture(t, "gas")
			v := captureSchemaRecord(t, m, capture)
			tc.change(m)
			if err := m.admitSchema(context.Background(), v); !errors.Is(err, ErrDenied) {
				t.Fatalf("uncommissioned capture admitted: %v", err)
			}
		})
	}
}

func TestCaptureSchemaPausedSampleStillExpiresPhysically(t *testing.T) {
	m, clock := captureSchemaManager(t)
	capture, _ := captureSchemaFixture(t, "gas")
	v := captureSchemaRecord(t, m, capture)
	fixed := v.Component().Timing.SampleTime
	if err := m.admitSchema(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	clock.mu.Lock()
	clock.window.Earliest = moduleTestNow.Add(5*time.Second - time.Millisecond)
	clock.window.Latest = moduleTestNow.Add(5 * time.Second)
	clock.window.Monotonic += 5 * time.Second
	clock.mu.Unlock()
	if err := m.admitSchema(context.Background(), v); err != nil {
		t.Fatalf("capture at exact physical bound rejected: %v", err)
	}
	clock.mu.Lock()
	clock.window.Latest = clock.window.Latest.Add(time.Nanosecond)
	clock.window.Monotonic++
	clock.mu.Unlock()
	if err := m.admitSchema(context.Background(), v); !errors.Is(err, ErrDenied) {
		t.Fatalf("paused original capture survived physical expiry: %v", err)
	}
	if !v.Component().Timing.SampleTime.Equal(fixed) {
		t.Fatal("paused source time changed")
	}
	// A genuinely new physical capture at the same paused source instant can
	// be admitted. Reusing the old capture cannot extend its physical age.
	capture.CapturedUnixNS = moduleTestNow.Add(5 * time.Second).UnixNano()
	fresh := captureSchemaRecord(t, m, capture)
	if err := m.admitSchema(context.Background(), fresh); err != nil || !fresh.Component().Timing.SampleTime.Equal(fixed) {
		t.Fatalf("fresh paused capture: %v", err)
	}
}

func TestCaptureSchemaRequiresExactSceneFrameAmongApprovedFrames(t *testing.T) {
	m, _ := captureSchemaManager(t)
	other := m.cfg.Policy.Frames[0]
	other.ID = "mezzanine"
	other.Revision = "survey-other"
	m.cfg.Policy.Frames = append(m.cfg.Policy.Frames, other)
	if err := m.cfg.Policy.validate(); err != nil {
		t.Fatalf("test requires two valid commissioned frames: %v", err)
	}
	capture, _ := captureSchemaFixture(t, "gas")
	sample, _ := capture.Sample()
	sample.Frame = other
	captureSchemaSetSample(t, &capture, sample)
	v := captureSchemaRecord(t, m, capture)
	if err := m.admitSchema(context.Background(), v); !errors.Is(err, ErrDenied) {
		t.Fatalf("capture from another approved frame bypassed exact scene binding: %v", err)
	}
	// The frame is generally permitted, but becomes eligible for this source
	// only when the scene itself has been commissioned with that frame.
	m.cfg.CaptureSource.Frame = other
	if err := m.admitSchema(context.Background(), v); err != nil {
		t.Fatalf("capture with exact commissioned frame was rejected: %v", err)
	}
}

func TestCaptureSchemaRejectsFutureAndUntrustedPhysicalTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*sensorrecord.Capture, *moduleClock)
	}{
		{"future-capture", func(c *sensorrecord.Capture, _ *moduleClock) {
			c.CapturedUnixNS = moduleTestNow.Add(2 * time.Millisecond).UnixNano()
		}},
		{"uncertainty-exceeds-age", func(_ *sensorrecord.Capture, clock *moduleClock) {
			clock.window.Latest = moduleTestNow.Add(5*time.Second + time.Nanosecond)
		}},
		{"clock-unavailable", func(_ *sensorrecord.Capture, clock *moduleClock) { clock.err = trustedtime.ErrUntrusted }},
		{"clock-not-authenticated", func(_ *sensorrecord.Capture, clock *moduleClock) { clock.window.Authenticated = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, clock := captureSchemaManager(t)
			c, _ := captureSchemaFixture(t, "gas")
			tc.change(&c, clock)
			if err := m.admitSchema(context.Background(), captureSchemaRecord(t, m, c)); !errors.Is(err, ErrDenied) {
				t.Fatalf("capture admitted without bounded physical freshness: %v", err)
			}
		})
	}
}

type captureSchemaClockFunc func() (trustedtime.Interval, error)

func (f captureSchemaClockFunc) Now() (trustedtime.Interval, error) { return f() }

func TestCaptureSchemaRechecksPhysicalFreshnessAfterMedia(t *testing.T) {
	for _, fault := range []bool{false, true} {
		name := "capture-expired"
		if fault {
			name = "physical-clock-failed"
		}
		t.Run(name, func(t *testing.T) {
			m, clock := captureSchemaManager(t)
			captureSchemaStore(t, m)
			c, data := captureSchemaFixture(t, "camera")
			v := captureSchemaRecord(t, m, c)
			ref, _ := c.Evidence()
			captureSchemaPut(t, m, captureSchemaOwner(m, v), *ref, data, true)
			before, _ := clock.Now()
			calls := 0
			// The policy and initial capture checks observe fresh time. The
			// independent retention store remains valid while admission crosses
			// the physical capture bound during media validation.
			m.deps.Clock = captureSchemaClockFunc(func() (trustedtime.Interval, error) {
				calls++
				if calls <= 2 {
					return before, nil
				}
				if fault {
					return trustedtime.Interval{}, trustedtime.ErrUntrusted
				}
				after := before
				after.Earliest, after.Latest = before.Earliest.Add(5*time.Second), before.Latest.Add(5*time.Second)
				after.Monotonic += 5 * time.Second
				return after, nil
			})
			if err := m.admitSchema(context.Background(), v); !errors.Is(err, ErrDenied) {
				t.Fatalf("capture admitted after physical freshness was lost during media validation: %v", err)
			}
		})
	}
}

func TestCaptureSchemaRefusesMissingOrMismatchedMedia(t *testing.T) {
	for _, name := range []string{"no-store", "missing", "incomplete", "wrong-owner", "wrong-size", "wrong-content-type", "wrong-digest", "corrupt-retained-bytes", "malformed-image", "wrong-image-dimensions", "malformed-audio", "wrong-audio-interval"} {
		t.Run(name, func(t *testing.T) {
			m, _ := captureSchemaManager(t)
			kind := "camera"
			if name == "malformed-audio" || name == "wrong-audio-interval" {
				kind = "audio"
			}
			c, data := captureSchemaFixture(t, kind)
			s, _ := c.Sample()
			if name == "malformed-image" || name == "malformed-audio" {
				data = []byte("bytes that match their digest but are not the declared media")
				s.Evidence = captureSchemaReference(data, s.Evidence.ContentType)
			}
			if name == "wrong-image-dimensions" {
				s.Camera.Width++
			}
			if name == "wrong-audio-interval" {
				s.Audio.DurationNS = int64(125 * time.Millisecond)
			}
			captureSchemaSetSample(t, &c, s)
			v := captureSchemaRecord(t, m, c)
			if name != "no-store" {
				dir := captureSchemaStore(t, m)
				owner := captureSchemaOwner(m, v)
				ref := *s.Evidence
				switch name {
				case "wrong-owner":
					owner.Service = "another-capture-service"
				case "wrong-content-type":
					ref.ContentType = "image/jpeg"
				}
				if name == "incomplete" {
					captureSchemaPut(t, m, owner, ref, data[:len(data)/2], false)
				} else if name != "missing" {
					captureSchemaPut(t, m, owner, ref, data, true)
				}
				if name == "corrupt-retained-bytes" {
					if err := os.WriteFile(filepath.Join(dir, "blobs", ref.SHA256+".blob"), bytes.Repeat([]byte("x"), len(data)), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "wrong-size" || name == "wrong-digest" {
				if name == "wrong-size" {
					s.Evidence.Size++
				} else {
					s.Evidence.SHA256 = strings.Repeat("c", 64)
				}
				captureSchemaSetSample(t, &c, s)
				v = captureSchemaRecord(t, m, c)
			}
			if err := m.admitSchema(context.Background(), v); !errors.Is(err, ErrDenied) {
				t.Fatalf("unusable media admitted: %v", err)
			}
		})
	}
}

func TestCaptureSchemaKeepsCalibrationAndSensorLinkLineage(t *testing.T) {
	m, _ := captureSchemaManager(t)
	captureSchemaStore(t, m)
	original, _ := captureSchemaFixture(t, "camera")
	sample, _ := original.Sample()
	firstCalibration, err := sensorrecord.CalibrationDigest(sample)
	if err != nil {
		t.Fatal(err)
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"cal-v1", "cal-v2"} {
		t.Run(revision, func(t *testing.T) {
			if revision == "cal-v2" {
				sample.CalibrationRevision = revision
				sample.PositionM[0], sample.Camera.CameraToSite[3] = -1.5, -1.5
				sample.Camera.FX = 4.5
			}
			captureSchemaSetSample(t, &original, sample)
			wire, err := sensorrecord.BindSensorLink(original, sensorrecord.SensorLinkFrame{ChannelID: 4, Seq: 91, TsUs: uint64(original.CapturedUnixNS / 1000), Payload: jpegBytes.Bytes()})
			if err != nil {
				t.Fatal(err)
			}
			v := captureSchemaRecord(t, m, wire)
			ref, _ := wire.Evidence()
			captureSchemaPut(t, m, captureSchemaOwner(m, v), *ref, jpegBytes.Bytes(), true)
			if err := m.admitSchema(context.Background(), v); err != nil {
				t.Fatal(err)
			}
			got, err := sensorrecord.FromComponent(v.Component())
			if err != nil {
				t.Fatal(err)
			}
			decoded, _ := got.Sample()
			if !reflect.DeepEqual(decoded, sample) || !bytes.Equal(got.SampleJSON, original.SampleJSON) || got.SensorLink.CaptureSequence != 17 || got.SensorLink.FrameSequence != 91 || v.Record.Sequence != 3 || got.SensorLink.TimestampUS != uint64(original.CapturedUnixNS/1000) || got.SensorLink.Evidence.SHA256 == decoded.Evidence.SHA256 {
				t.Fatal("calibration or separate native, wire, and agent lineage was lost")
			}
			if revision == "cal-v2" && got.SensorLink.CalibrationSHA256 == firstCalibration {
				t.Fatal("changed calibration reused its old digest")
			}
		})
	}
}
