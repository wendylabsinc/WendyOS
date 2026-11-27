package adapters

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var sampleAnchor = time.Unix(1_700_000_000, 0)

// sample builds a sample of the given kind that passes sensing's own
// validation, so the conversion is tested against the real contract.
func sample(t *testing.T, kind string) sensing.Sample {
	t.Helper()
	digest := strings.Repeat("ab", 32)
	s := sensing.Sample{
		SchemaVersion: sensing.Schema, ID: "run-1/" + kind + "-a/1", SiteID: "site", Frame: world.FrameRef{ID: "site", Revision: "r1"},
		SensorID: kind + "-a", Kind: kind, Sequence: 1, SampleNS: int64(1500 * time.Millisecond), RunID: "run-1", ScenarioVersion: "v1", Seed: 7,
		PackageSHA256: digest, SensorModel: "model", PositionM: []float64{1, 0.3, 2}, UncertaintyM: 0.05, CalibrationRevision: "cal-1",
	}
	switch kind {
	case "lidar":
		s.Lidar = &sensing.Lidar{MaxRangeM: 6, PointsM: [][3]float64{
			{3, 0.3, 2},   // 2 m along site +X
			{1, 0.4, 3},   // 1 m along site +Z, 10 cm above the sensor
			{1, 0.3, 3.5}, // behind it
			{2, 0, 2},     // floor: below the band
			{-6, 0.3, 2},  // 7 m along −X: beyond the declared range
		}}
	case "gas":
		s.Gas = &sensing.Gas{ValuePPM: 80, UncertaintyPPM: 1.5, Species: "co2"}
	case "camera":
		eye := geom.Vec3{X: 1, Y: 2, Z: 0}
		pose, err := geom.LookAt(eye, geom.Vec3{X: 4, Y: 0, Z: 4}, geom.Vec3{Y: 1})
		if err != nil {
			t.Fatal(err)
		}
		s.PositionM = []float64{eye.X, eye.Y, eye.Z}
		s.Camera = &sensing.Camera{Width: 320, Height: 240, FX: 240, FY: 250, CX: 160, CY: 120, CameraToSite: pose.Matrix4(), CalibrationValidForNS: int64(time.Minute)}
		s.Evidence = &sensing.EvidenceRef{SHA256: digest, ContentType: "image/png", Size: 1024}
	case "audio":
		s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
		s.Evidence = &sensing.EvidenceRef{SHA256: digest, ContentType: "audio/wav", Size: 4044}
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("%s fixture: %v", kind, err)
	}
	return s
}

func TestSampleLidar(t *testing.T) {
	s := sample(t, "lidar")
	out, err := FromSample(Config{}, "tripod", sampleAnchor, s)
	if err != nil {
		t.Fatal(err)
	}
	scan, ok := out.Measurement.(spatial.Scan)
	if !ok || out.Camera != nil || out.Calibration != nil {
		t.Fatalf("lidar output %+v", out)
	}
	if scan.Device != "tripod" || scan.Sensor != "lidar-a" || !scan.Time.Equal(sampleAnchor.Add(1500*time.Millisecond)) || scan.RangeMax != 6 || len(scan.Ranges) != 360 {
		t.Fatalf("scan header or limits: %+v", scan)
	}
	if out.SensorToSite.R != geom.Identity3() || out.SensorToSite.T != (geom.Vec3{X: 1, Y: 0.3, Z: 2}) || out.PositionSigmaM != 0.05 {
		t.Fatalf("scan frame %+v", out.SensorToSite)
	}
	// Placing the hits with the returned pose recovers the site points.
	var placed []geom.Vec2
	for _, p := range scan.Points() {
		placed = append(placed, out.SensorToSite.Apply(p.Lift(0)).XZ())
	}
	// Beam order from −π: +X (yaw 0) before +Z (yaw +π/2).
	if len(placed) != 2 || !near2(placed[0], geom.Vec2{X: 3, Z: 2}) || !near2(placed[1], geom.Vec2{X: 1, Z: 3}) {
		t.Fatalf("placed hits %+v", placed)
	}
	for i := range scan.Ranges {
		if _, _, state := scan.Beam(i); state == spatial.BeamNoReturn {
			t.Fatalf("beam %d claims free space from an empty bin", i)
		}
	}

	wide, err := FromSample(Config{BandLow: -1, BandHigh: 1}, "tripod", sampleAnchor, s)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(wide.Measurement.(spatial.Scan).Points()); n != 2 { // the floor point shares the +X bin, nearer
		t.Fatalf("%d hits in a wide band", n)
	}
	if _, r, _ := wide.Measurement.(spatial.Scan).Beam(180); r != 1 {
		t.Fatalf("a band that reaches the floor reports it at %v m", r)
	}

	s.PositionM = nil
	if _, err := FromSample(Config{}, "tripod", sampleAnchor, s); !errors.Is(err, ErrNoPose) {
		t.Fatalf("lidar without a position: %v", err)
	}
	s = sample(t, "lidar")
	s.Lidar.PointsM[0][1] = math.Inf(1)
	if _, err := FromSample(Config{}, "tripod", sampleAnchor, s); !errors.Is(err, ErrInvalid) {
		t.Fatalf("infinite point: %v", err)
	}
	if _, err := FromSample(Config{}, "tripod", time.Time{}, sample(t, "lidar")); !errors.Is(err, ErrNoTime) {
		t.Fatalf("no anchor: %v", err)
	}
}

func TestSampleGas(t *testing.T) {
	s := sample(t, "gas")
	out, err := FromSample(Config{}, "sniffer", sampleAnchor, s)
	if err != nil {
		t.Fatal(err)
	}
	want := spatial.Scalar{Header: spatial.Header{Device: "sniffer", Sensor: "gas-a", Time: spatial.Stamp(sampleAnchor.UnixNano() + s.SampleNS)}, Quantity: "co2", Value: 80, Unit: "ppm", Sigma: 1.5}
	if out.Measurement != spatial.Measurement(want) || out.SensorToSite.T != (geom.Vec3{X: 1, Y: 0.3, Z: 2}) {
		t.Fatalf("gas output %+v", out)
	}
	s.Gas.Species = "carbon dioxide"
	if _, err := FromSample(Config{}, "sniffer", sampleAnchor, s); !errors.Is(err, ErrInvalid) {
		t.Fatalf("species that is not an identifier: %v", err)
	}
}

func TestSampleCamera(t *testing.T) {
	s := sample(t, "camera")
	out, err := FromSample(Config{}, "cam-1", sampleAnchor, s)
	if err != nil {
		t.Fatal(err)
	}
	if out.Measurement != nil {
		t.Fatal("a camera sample without detections must not yield an ingestible measurement")
	}
	f := out.Camera
	if f == nil || f.Width != 320 || f.Height != 240 || f.PixelSigma != 1 || f.Detections != nil || f.Sensor != "camera-a" || f.Validate() != nil {
		t.Fatalf("frame header %+v", f)
	}
	cal := out.Calibration
	if cal == nil || cal.Validate() != nil {
		t.Fatalf("calibration prior %+v", cal)
	}
	if cal.State != spatial.CalibrationEstimating || cal.Parent != spatial.ParentSite || cal.Revision != "cal-1" || cal.TranslationSigmaM != 0.05 || cal.RotationSigmaRad != 0.02 ||
		*cal.Intrinsics != (geom.Intrinsics{FX: 240, FY: 250, CX: 160, CY: 120, Width: 320, Height: 240}) {
		t.Fatalf("calibration prior %+v", cal)
	}
	// The camera looks from (1, 2, 0) toward (4, 0, 4): the point it looks at
	// projects onto the principal point, and its view yaw is atan2(4, 3).
	if px, ok := cal.Intrinsics.ProjectWorld(cal.SensorToParent, geom.Vec3{X: 4, Y: 0, Z: 4}); !ok || px.Dist(geom.Pixel{U: 160, V: 120}) > 1e-6 {
		t.Fatalf("look-at point projects to %+v", px)
	}
	if yaw, ok := out.SensorToSite.ViewYaw(); !ok || math.Abs(yaw-math.Atan2(4, 3)) > tol {
		t.Fatalf("view yaw %v", yaw)
	}

	undeclared := sample(t, "camera")
	undeclared.Camera.CameraToSite = nil
	out, err = FromSample(Config{}, "cam-1", sampleAnchor, undeclared)
	if err != nil || out.Camera == nil || out.Calibration != nil {
		t.Fatalf("camera without a declared pose: %+v %v", out, err)
	}
	skewed := sample(t, "camera")
	skewed.Camera.CameraToSite[0] *= 1.1
	if _, err := FromSample(Config{}, "cam-1", sampleAnchor, skewed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-rigid camera transform: %v", err)
	}

	linked := sample(t, "camera")
	linked.SensorLink = &sensing.SensorLink{AnchorUnixUS: sampleAnchor.UnixMicro(), ClockErrorUS: 2500}
	out, err = FromSample(Config{}, "cam-1", sampleAnchor, linked)
	if err != nil || out.Calibration.TimeOffsetSigma != 2500*time.Microsecond {
		t.Fatalf("SensorLink clock error: %v", err)
	}
	if _, err := FromSample(Config{}, "cam-1", sampleAnchor.Add(time.Second), linked); !errors.Is(err, ErrInvalid) {
		t.Fatalf("anchor that differs from the SensorLink's: %v", err)
	}
}

func TestSampleWithoutMapping(t *testing.T) {
	if _, err := FromSample(Config{}, "mic-1", sampleAnchor, sample(t, "audio")); !errors.Is(err, ErrUnmapped) {
		t.Fatalf("mono audio: %v", err)
	}
	s := sample(t, "gas")
	s.Kind = "thermal"
	if _, err := FromSample(Config{}, "x", sampleAnchor, s); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := FromSample(Config{RangeMin: -1}, "x", sampleAnchor, sample(t, "gas")); !errors.Is(err, ErrConfig) {
		t.Fatalf("bad config: %v", err)
	}
}
