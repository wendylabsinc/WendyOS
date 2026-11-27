package peoplecamera

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

func fixture(t *testing.T) (Capture, liveapp.Config) {
	t.Helper()
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	media := imageBytes.Bytes()
	hash := sha256.Sum256(media)
	frame := world.FrameRef{ID: "floor", Revision: "revision1"}
	s := sensing.Sample{SchemaVersion: "wendy.sensor.sample.v1", ID: "run/camera-a/1", SiteID: "studio-01", Frame: frame,
		SensorID: "camera-a", Kind: "camera", Sequence: 1, SampleNS: 250000000, RunID: "run", ScenarioVersion: "people-annex-v1", Seed: 7,
		PackageSHA256: strings.Repeat("a", 64), SensorModel: "mujoco-4.0.0-rgb-pinhole", PositionM: []float64{0, 2, 0}, UncertaintyM: .02,
		CalibrationRevision: "camera1", Camera: &sensing.Camera{Width: 2, Height: 2, FX: 2, FY: 2, CX: 1, CY: 1, CameraToSite: []float64{1, 0, 0, 0, 0, 1, 0, 2, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: 60000000000},
		Evidence: &sensing.EvidenceRef{SHA256: hex.EncodeToString(hash[:]), ContentType: "image/png", Size: int64(len(media))}}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UnixNano()
	return Capture{Capture: sensorrecord.Capture{Version: 1, SceneBootID: "boot", CapturedUnixNS: anchor, SimulationAnchorUnixNS: anchor, SampleJSON: raw}, Media: media}, liveapp.Config{RunID: "run", Site: "studio-01", Frame: frame, SensorID: "camera-a", SimulationAnchorUnixNS: anchor}
}

func TestCapturePreservesBytesAndBindsCommission(t *testing.T) {
	for _, sensor := range []string{"camera-a", "camera-b", "people-front", "people-oblique"} {
		t.Run(sensor, func(t *testing.T) {
			f, c := namedFixture(t, sensor)
			raw, _ := json.Marshal(f)
			got, err := Decode(raw, c)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Capture.SampleJSON, f.Capture.SampleJSON) || !bytes.Equal(got.Media, f.Media) {
				t.Fatal("source bytes changed")
			}
			c.SensorID = "camera-a"
			if sensor == "camera-a" {
				c.SensorID = "people-front"
			}
			if _, err = Decode(raw, c); err == nil {
				t.Fatal("foreign camera accepted")
			}
			c.SensorID = sensor
			c.SimulationAnchorUnixNS++
			if _, err = Decode(raw, c); err == nil {
				t.Fatal("new anchor accepted")
			}
		})
	}
}

func namedFixture(t *testing.T, sensor string) (Capture, liveapp.Config) {
	t.Helper()
	f, c := fixture(t)
	sample, err := f.Capture.Sample()
	if err != nil {
		t.Fatal(err)
	}
	sample.SensorID, sample.ID = sensor, "run/"+sensor+"/1"
	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	f.Capture.SampleJSON = append(append([]byte("\n "), raw...), '\n')
	c.SensorID = sensor
	return f, c
}

func TestCaptureRejectsChangedPixels(t *testing.T) {
	f, c := fixture(t)
	f.Media[len(f.Media)-1] ^= 1
	raw, _ := json.Marshal(f)
	if _, err := Decode(raw, c); err == nil {
		t.Fatal("changed image accepted")
	}
}

func TestCameraImageEnvelopeLargerThanSignedMetadataBudget(t *testing.T) {
	f, c := fixture(t)
	pixels := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	random := rand.New(rand.NewSource(7))
	for i := range pixels.Pix {
		pixels.Pix[i] = byte(random.Intn(256))
	}
	for i := 3; i < len(pixels.Pix); i += 4 {
		pixels.Pix[i] = 255
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, pixels); err != nil {
		t.Fatal(err)
	}
	f.Media = encoded.Bytes()
	s, err := f.Capture.Sample()
	if err != nil {
		t.Fatal(err)
	}
	s.Camera.Width, s.Camera.Height = 256, 256
	hash := sha256.Sum256(f.Media)
	s.Evidence.SHA256, s.Evidence.Size = hex.EncodeToString(hash[:]), int64(len(f.Media))
	f.Capture.SampleJSON, _ = json.Marshal(s)
	raw, _ := json.Marshal(f)
	if len(raw) <= 64<<10 {
		t.Fatal("fixture must exceed the signed metadata budget")
	}
	if _, err = Decode(raw, c); err != nil {
		t.Fatal("bounded camera image refused", len(raw), err)
	}
}

func TestRecordedMuJoCoCapture(t *testing.T) {
	checkRecordedCaptures(t, "WENDY_PEOPLE_CAPTURE_DIR", []string{"camera-a", "camera-b"})
}

func TestSharedMuJoCoCapture(t *testing.T) {
	checkRecordedCaptures(t, "WORLD_SHARED_CAPTURE_DIR", []string{"camera-a", "camera-b", "people-front", "people-oblique"})
}

func checkRecordedCaptures(t *testing.T, environment string, cameras []string) {
	t.Helper()
	dir := os.Getenv(environment)
	if dir == "" {
		t.Skip("set " + environment + " for original renderer captures")
	}
	for _, camera := range cameras {
		raw, err := os.ReadFile(filepath.Join(dir, camera+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var original Capture
		if err = json.Unmarshal(raw, &original); err != nil {
			t.Fatal(err)
		}
		s, err := original.Capture.Sample()
		if err != nil {
			t.Fatal(err)
		}
		cfg := liveapp.Config{RunID: s.RunID, Site: s.SiteID, Frame: s.Frame, SensorID: camera, SimulationAnchorUnixNS: original.Capture.SimulationAnchorUnixNS}
		got, err := Decode(raw, cfg)
		if err != nil {
			t.Fatal(camera, err)
		}
		if !bytes.Equal(got.Media, original.Media) || !bytes.Equal(got.Capture.SampleJSON, original.Capture.SampleJSON) {
			t.Fatal("original camera bytes changed")
		}
		t.Logf("%s: decoded %d envelope bytes and %d original PNG bytes", camera, len(raw), len(got.Media))
	}
}
