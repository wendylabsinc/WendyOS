package fusion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func spatialFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.cfg.RequiredSensors = map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio", "lidar-atlas": "lidar"}
	for i := 0; i < 2; i++ {
		s, err := sensing.Decode(f.records[i].Payload)
		if err != nil {
			t.Fatal(err)
		}
		x := -1.
		pixel := 42
		if i == 1 {
			x = 1
			pixel = 22
		}
		s.PositionM = []float64{x, 0, 0}
		s.UncertaintyM = .01
		s.Camera.CameraToSite = []float64{1, 0, 0, x, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
		im := image.NewRGBA(image.Rect(0, 0, 64, 48))
		for y := 0; y < 48; y++ {
			for x := 0; x < 64; x++ {
				im.Set(x, y, color.RGBA{G: 180, A: 255})
			}
		}
		for y := 22; y < 26; y++ {
			for x := pixel - 2; x < pixel+2; x++ {
				im.Set(x, y, color.RGBA{R: 255, A: 255})
			}
		}
		// A disconnected single red pixel must not move the observed centroid.
		im.Set(0, 0, color.RGBA{R: 255, A: 255})
		var buf bytes.Buffer
		if err = png.Encode(&buf, im); err != nil {
			t.Fatal(err)
		}
		data := buf.Bytes()
		hash := sha256.Sum256(data)
		s.Evidence = &sensing.EvidenceRef{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data)), ContentType: "image/png"}
		f.blobs[s.Evidence.SHA256] = data
		f.records[i].Payload, _ = json.Marshal(s)
	}
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/lidar-atlas/1", SiteID: "office", Frame: f.cfg.Frame, SensorID: "lidar-atlas", Kind: "lidar", Sequence: 1, SampleNS: 250000000, RunID: "run", ScenarioVersion: "v1", PackageSHA256: string(bytes.Repeat([]byte("a"), 64)), SensorModel: "ray-fixture", PositionM: []float64{0, 0, 0}, UncertaintyM: .02, CalibrationRevision: "c1", Lidar: &sensing.Lidar{PointsM: [][3]float64{{0, 0, 4}, {.04, 0, 4}, {3, 0, 4}}, MaxRangeM: 6}}
	converter, err := sensing.New(sensing.Config{Tenant: "tenant", Site: "office", RunID: "run", Frame: f.cfg.Frame, Source: world.Source{Device: "lidar-device", App: "sensors", Service: sample.SensorID, Stream: "samples"}, Epoch: 1, Anchor: f.now.Add(-500 * time.Millisecond), TTL: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sample)
	component, err := converter.Convert(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.records = append(f.records, component)
	return f
}

func TestCameraLidarLocalizesFromPixelsAndCalibratedRays(t *testing.T) {
	f := spatialFixture(t)
	r := f.run(t)
	if r.State != HazardDetected || r.Spatial == nil || !r.Spatial.Associated || len(r.Hazard.Inputs) != 5 {
		t.Fatalf("%+v", r)
	}
	p := r.Hazard.PositionM
	if math.Abs(p[0]-.01) > .005 || math.Abs(p[1]) > .005 || math.Abs(p[2]-4) > .005 {
		t.Fatalf("location %v", p)
	}
	if r.Spatial.LidarPointsConsidered != 3 || len(r.Spatial.LidarSupportPoints) != 2 || r.Spatial.UncertaintyM <= .02 || r.Spatial.ReprojectionErrorPX > .2 {
		t.Fatalf("%+v", r.Spatial)
	}
	for _, c := range r.Spatial.Cameras {
		if c.Observation.Pixels != 16 || c.Observation.CentroidPX[1] != 24 {
			t.Fatalf("%+v", c)
		}
	}
	if r.Hazard.LocationBasis == "gas sensor position; leak source is not localized" {
		t.Fatal("retained old location")
	}
}

func TestSpatialAssociationNeedsCurrentAlignedLidarAndBothImages(t *testing.T) {
	cases := map[string]func(*fixture){
		"unrelated returns": func(f *fixture) {
			modifySample(t, &f.records[4], func(s *sensing.Sample) { s.Lidar.PointsM = [][3]float64{{3, 0, 4}} })
		},
		"outside scan range": func(f *fixture) {
			modifySample(t, &f.records[4], func(s *sensing.Sample) { s.Lidar.MaxRangeM = 1 })
		},
		"empty scan": func(f *fixture) {
			modifySample(t, &f.records[4], func(s *sensing.Sample) { s.Lidar.PointsM = [][3]float64{} })
		},
		"parallel rays": func(f *fixture) {
			first, _ := sensing.Decode(f.records[0].Payload)
			modifySample(t, &f.records[1], func(s *sensing.Sample) {
				s.PositionM = []float64{-1, 0, 0}
				s.Camera.CameraToSite[3] = -1
				s.Evidence = first.Evidence
			})
		},
		"expired": func(f *fixture) { f.records[4].TTL = time.Millisecond },
		"skew":    func(f *fixture) { f.records[4].Timing.SampleTime = f.records[4].Timing.SampleTime.Add(-time.Second) },
		"frame":   func(f *fixture) { f.records[4].Frame.Revision = "other" },
		"image integrity": func(f *fixture) {
			s, _ := sensing.Decode(f.records[0].Payload)
			f.blobs[s.Evidence.SHA256] = []byte("not an image")
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := spatialFixture(t)
			change(f)
			r := f.run(t)
			if r.State != InsufficientEvidence || r.Hazard != nil {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestSpatialLocationDoesNotReplaceGasAudioCorroboration(t *testing.T) {
	f := spatialFixture(t)
	modifySample(t, &f.records[2], func(s *sensing.Sample) { s.Gas.ValuePPM = 5 })
	r := f.run(t)
	if r.State != NotDetected || r.Hazard != nil || r.Spatial == nil || !r.Spatial.Associated {
		t.Fatalf("%+v", r)
	}
}

func TestSpatialLocalizationUsesFullCameraRotationAndTranslation(t *testing.T) {
	f := spatialFixture(t)
	transform := func(p []float64) []float64 { return []float64{p[2] + 3, p[1] + 2, -p[0] - 1} }
	for i := range f.records {
		modifySample(t, &f.records[i], func(s *sensing.Sample) {
			s.PositionM = transform(s.PositionM)
			if s.Camera != nil {
				p := s.PositionM
				s.Camera.CameraToSite = []float64{0, 0, 1, p[0], 0, 1, 0, p[1], -1, 0, 0, p[2], 0, 0, 0, 1}
			}
			if s.Lidar != nil {
				for j, p := range s.Lidar.PointsM {
					q := transform(p[:])
					s.Lidar.PointsM[j] = [3]float64{q[0], q[1], q[2]}
				}
			}
		})
	}
	r := f.run(t)
	if r.State != HazardDetected || r.Hazard == nil {
		t.Fatalf("%+v", r)
	}
	want := [3]float64{7, 2, -1.01}
	for i, v := range r.Hazard.PositionM {
		if math.Abs(v-want[i]) > .005 {
			t.Fatalf("rotated location=%v want=%v", r.Hazard.PositionM, want)
		}
	}
}
