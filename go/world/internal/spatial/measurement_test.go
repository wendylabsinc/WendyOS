package spatial

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

var t0 = Stamp(1_800_000_000_000_000_000)

func header(sensor SensorID) Header { return Header{Device: "robot-1", Sensor: sensor, Time: t0} }

// sampleMeasurements returns one valid value of every measurement type.
func sampleMeasurements() []Measurement {
	return []Measurement{
		Scan{Header: header("lidar"), AngleMin: -math.Pi / 2, AngleIncrement: math.Pi / 4, RangeMin: 0.1, RangeMax: 12, Ranges: []float64{2, RangeNoReturn, 1.5, RangeInvalid, 3}, RangeSigma: 0.015},
		Odometry{Header: header("odom"), Pose: geom.Pose2{X: 1, Z: -2, Yaw: 0.3}, ElevationKnown: true, Elevation: 0.2, Noise: OdometryNoise{TransPerMetre: 0.02, YawPerMetre: 0.01, YawPerRadian: 0.02}},
		Imu{Header: header("imu"), YawRate: 0.1, Accel: geom.Vec3{Y: 9.81}, GyroSigma: 0.002, AccelSigma: 0.05},
		Baro{Header: header("baro"), PressurePa: 101325, Altitude: 3.1, AltitudeSigma: 0.15},
		GnssFix{Header: header("gnss"), LatitudeDegrees: 52.37, LongitudeDegrees: 4.89, AltitudeMetres: 3, HorizontalAccuracy: 1.5, VerticalAccuracy: 3},
		CameraFrame{Header: header("cam"), Width: 640, Height: 480, PixelSigma: 0.5, Detections: []Detection{
			{Kind: DetectFiducial, ID: "marker-robot-2", Index: 3, Pixel: geom.Pixel{U: 100.5, V: 200.25}},
			{Kind: DetectTargetCorner, ID: "board-1", Index: 17, Pixel: geom.Pixel{U: 320, V: 240}},
			{Kind: DetectObject, Class: "person", Confidence: 0.9, Pixel: geom.Pixel{U: 300, V: 400}, BBox: &BBox{Min: geom.Pixel{U: 280, V: 200}, Max: geom.Pixel{U: 320, V: 400}}},
		}},
		Range{Header: header("uwb"), Peer: "beacon-3", Distance: 4.2, Sigma: 0.1, LineOfSight: new(bool)},
		Bearing{Header: header("mic"), Azimuth: 0.7, Sigma: 0.05, Source: "alarm-1"},
		Scalar{Header: header("gas"), Quantity: "co2", Value: 612, Unit: "ppm", Sigma: 15},
		SensorManifest{Header: header("cam"), Modality: KindCamera, Model: "sim-pinhole-640", Intrinsics: &geom.Intrinsics{FX: 600, FY: 600, CX: 320, CY: 240, Width: 640, Height: 480}, Attachment: Attachment{Mode: AttachMounted, Carrier: "robot-1"}},
	}
}

func TestMeasurementInterface(t *testing.T) {
	wantKinds := []SensorKind{KindLidar2D, KindOdometry, KindImu, KindBaro, KindGnss, KindCamera, KindUwb, KindMic, KindGas, KindCamera}
	for i, m := range sampleMeasurements() {
		if err := m.Validate(); err != nil {
			t.Fatalf("%T: %v", m, err)
		}
		device, sensor := m.From()
		if device != "robot-1" || sensor == "" || !m.When().Equal(t0) {
			t.Fatalf("%T: header accessors returned %q %q %v", m, device, sensor, m.When())
		}
		if m.Kind() != wantKinds[i] || !m.Kind().Valid() {
			t.Fatalf("%T: kind %q, want %q", m, m.Kind(), wantKinds[i])
		}
		kind := string(m.Kind())
		if _, ok := m.(SensorManifest); ok {
			kind = "manifest"
		}
		if id := InputID(m); !strings.HasPrefix(id, "robot-1/"+string(sensor)+"/"+kind+"/1800000000000000000") {
			t.Fatalf("%T: input id %q", m, id)
		}
	}
	if SensorKind("radar").Valid() || len(SensorKinds()) != 9 {
		t.Fatal("sensor kind registry")
	}
	if !Stamp(5).Equal(Stamp(5)) || Stamp(5) != Stamp(5) || Stamp(5).UnixNano() != 5 {
		t.Fatal("Stamp must be comparable with == and exact")
	}
}

func TestMeasurementRoundTrip(t *testing.T) {
	for _, m := range sampleMeasurements() {
		line, err := EncodeMeasurement(m)
		if err != nil {
			t.Fatalf("%T: %v", m, err)
		}
		if strings.ContainsRune(string(line), '\n') {
			t.Fatalf("%T: record spans lines", m)
		}
		back, err := DecodeMeasurement(line)
		if err != nil {
			t.Fatalf("%T: %v\n%s", m, err, line)
		}
		if !reflect.DeepEqual(back, m) {
			t.Fatalf("%T round trip:\n got %+v\nwant %+v", m, back, m)
		}
	}
}

func TestDecodeMeasurementRejects(t *testing.T) {
	good, err := EncodeMeasurement(sampleMeasurements()[6])
	if err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"unknown type":    `{"type":"radar","data":{}}`,
		"unknown field":   strings.Replace(string(good), `"distance_m"`, `"extra":1,"distance_m"`, 1),
		"duplicate field": strings.Replace(string(good), `"distance_m"`, `"sigma_m":1,"distance_m"`, 1),
		"invalid value":   strings.Replace(string(good), `"distance_m":4.2`, `"distance_m":-1`, 1),
		"missing header":  `{"type":"range","data":{"peer":"b","distance_m":1,"sigma_m":0.1}}`,
		"trailing data":   string(good) + `{}`,
		"not json":        `scan`,
	} {
		if _, err := DecodeMeasurement([]byte(line)); err == nil {
			t.Fatalf("%s: accepted %s", name, line)
		}
	}
	if _, err := EncodeMeasurement(nil); err == nil {
		t.Fatal("nil measurement encoded")
	}
	bad := sampleMeasurements()[0].(Scan)
	bad.Ranges = nil
	if _, err := EncodeMeasurement(bad); err == nil {
		t.Fatal("invalid measurement encoded")
	}
	ptr := sampleMeasurements()[0].(Scan)
	if _, err := EncodeMeasurement(&ptr); err == nil {
		t.Fatal("pointer measurement must be rejected: records hold values")
	}
}

func TestScanBeams(t *testing.T) {
	s := Scan{Header: header("lidar"), AngleMin: 0, AngleIncrement: math.Pi / 2, RangeMin: 0.2, RangeMax: 10, Ranges: []float64{2, 3, RangeNoReturn, RangeInvalid, 0.05, 11, 0}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	want := []BeamState{BeamHit, BeamHit, BeamNoReturn, BeamInvalid, BeamInvalid, BeamNoReturn, BeamInvalid}
	for i, w := range want {
		if _, _, got := s.Beam(i); got != w {
			t.Fatalf("beam %d: state %d, want %d", i, got, w)
		}
	}
	pts := s.Points()
	if len(pts) != 2 {
		t.Fatalf("points: %v", pts)
	}
	// Beam 0 lies along sensor +X; beam 1, a quarter turn of positive yaw on, along +Z.
	if pts[0].Dist(geom.Vec2{X: 2}) > 1e-12 || pts[1].Dist(geom.Vec2{Z: 3}) > 1e-12 {
		t.Fatalf("points do not follow the yaw convention: %v", pts)
	}
	if a, r, _ := s.Beam(1); a != math.Pi/2 || r != 3 || s.Angle(1) != a {
		t.Fatalf("beam 1: angle %g range %g", a, r)
	}
}

func mutated[T any](v T, f func(*T)) T { f(&v); return v }

func TestMeasurementValidation(t *testing.T) {
	ms := sampleMeasurements()
	scan, odom, imu, baro, gnss := ms[0].(Scan), ms[1].(Odometry), ms[2].(Imu), ms[3].(Baro), ms[4].(GnssFix)
	frame, rng, bearing, scalar, manifest := ms[5].(CameraFrame), ms[6].(Range), ms[7].(Bearing), ms[8].(Scalar), ms[9].(SensorManifest)
	nan := math.NaN()
	for name, m := range map[string]Measurement{
		"no device":           mutated(scan, func(s *Scan) { s.Device = "" }),
		"device with slash":   mutated(scan, func(s *Scan) { s.Device = "a/b" }),
		"zero time":           mutated(scan, func(s *Scan) { s.Time = time.Time{} }),
		"no beams":            mutated(scan, func(s *Scan) { s.Ranges = nil }),
		"zero increment":      mutated(scan, func(s *Scan) { s.AngleIncrement = 0 }),
		"max below min":       mutated(scan, func(s *Scan) { s.RangeMax = 0.05 }),
		"NaN range":           mutated(scan, func(s *Scan) { s.Ranges = []float64{nan} }),
		"too many beams":      mutated(scan, func(s *Scan) { s.Ranges = make([]float64, MaxScanBeams+1) }),
		"odom NaN":            mutated(odom, func(o *Odometry) { o.Pose.Yaw = nan }),
		"odom negative noise": mutated(odom, func(o *Odometry) { o.Noise.TransPerMetre = -1 }),
		"imu inf":             mutated(imu, func(m *Imu) { m.Accel.Y = math.Inf(1) }),
		"baro zero pressure":  mutated(baro, func(b *Baro) { b.PressurePa = 0 }),
		"gnss latitude":       mutated(gnss, func(g *GnssFix) { g.LatitudeDegrees = 91 }),
		"gnss no accuracy":    mutated(gnss, func(g *GnssFix) { g.HorizontalAccuracy = 0 }),
		"frame size":          mutated(frame, func(f *CameraFrame) { f.Width = 0 }),
		"fiducial without id": mutated(frame, func(f *CameraFrame) {
			f.Detections = []Detection{{Kind: DetectFiducial, Pixel: geom.Pixel{U: 1, V: 1}}}
		}),
		"fiducial with class": mutated(frame, func(f *CameraFrame) { f.Detections = []Detection{{Kind: DetectFiducial, ID: "m", Class: "person"}} }),
		"object without bbox": mutated(frame, func(f *CameraFrame) {
			f.Detections = []Detection{{Kind: DetectObject, Class: "person", Confidence: 0.5}}
		}),
		"object confidence": mutated(frame, func(f *CameraFrame) { d := f.Detections[2]; d.Confidence = 1.5; f.Detections = []Detection{d} }),
		"object inverted bbox": mutated(frame, func(f *CameraFrame) {
			d := f.Detections[2]
			d.BBox = &BBox{Min: d.BBox.Max, Max: d.BBox.Min}
			f.Detections = []Detection{d}
		}),
		"detection kind": mutated(frame, func(f *CameraFrame) { f.Detections = []Detection{{Kind: "blob"}} }),
		"detection NaN pixel": mutated(frame, func(f *CameraFrame) {
			f.Detections = []Detection{{Kind: DetectFiducial, ID: "m", Pixel: geom.Pixel{U: nan}}}
		}),
		"range without peer":  mutated(rng, func(r *Range) { r.Peer = "" }),
		"range negative":      mutated(rng, func(r *Range) { r.Distance = -1 }),
		"range without sigma": mutated(rng, func(r *Range) { r.Sigma = 0 }),
		"bearing sigma":       mutated(bearing, func(b *Bearing) { b.Sigma = 0 }),
		"scalar unit":         mutated(scalar, func(s *Scalar) { s.Unit = "" }),
		"manifest kind":       mutated(manifest, func(m *SensorManifest) { m.Modality = "radar" }),
		"manifest model":      mutated(manifest, func(m *SensorManifest) { m.Model = "" }),
		"mounted no carrier":  mutated(manifest, func(m *SensorManifest) { m.Attachment.Carrier = "" }),
		"fixed with carrier":  mutated(manifest, func(m *SensorManifest) { m.Attachment.Mode = AttachFixed }),
		"attachment mode":     mutated(manifest, func(m *SensorManifest) { m.Attachment.Mode = "" }),
		"lidar intrinsics":    mutated(manifest, func(m *SensorManifest) { m.Modality = KindLidar2D }),
		"bad intrinsics":      mutated(manifest, func(m *SensorManifest) { m.Intrinsics = &geom.Intrinsics{} }),
	} {
		if m.Validate() == nil {
			t.Fatalf("%s: accepted %+v", name, m)
		}
	}
}

// A manifest must not be able to smuggle in a pose: the registration feature
// has to estimate it.
func TestManifestCarriesNoPose(t *testing.T) {
	raw, err := json.Marshal(sampleMeasurements()[9])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		switch name {
		case "device", "sensor", "time", "kind", "model", "intrinsics", "attachment":
		default:
			t.Fatalf("manifest has unexpected field %q", name)
		}
	}
	var attachment map[string]json.RawMessage
	if err := json.Unmarshal(fields["attachment"], &attachment); err != nil || len(attachment) != 2 {
		t.Fatalf("attachment is a mode and a carrier only: %s", fields["attachment"])
	}
}

func TestGeometry(t *testing.T) {
	board := TargetGeometry{ID: "board-1", Rows: 6, Cols: 9, Spacing: 0.03}
	if err := board.Validate(); err != nil {
		t.Fatal(err)
	}
	if board.Count() != 54 {
		t.Fatalf("count %d", board.Count())
	}
	c, ok := board.Corner(9*2 + 4) // row 2, column 4
	if !ok || c.Dist(geom.Vec3{X: 0.12, Y: 0.06}) > 1e-12 {
		t.Fatalf("corner: %+v %v", c, ok)
	}
	if _, ok := board.Corner(54); ok {
		t.Fatal("corner index out of range accepted")
	}
	if _, ok := board.Corner(-1); ok {
		t.Fatal("negative corner index accepted")
	}
	for _, bad := range []TargetGeometry{{ID: "b", Rows: 1, Cols: 9, Spacing: 0.03}, {ID: "b", Rows: 6, Cols: 9}, {Rows: 6, Cols: 9, Spacing: 0.03}, {ID: "b", Rows: 100, Cols: 100, Spacing: 0.03}} {
		if bad.Validate() == nil {
			t.Fatalf("invalid target accepted: %+v", bad)
		}
	}
	marker := FiducialGeometry{ID: "marker-robot-2", Carrier: "robot-2", Points: []geom.Vec3{{X: 0.1, Y: 0.3}, {X: -0.1, Y: 0.3}, {Y: 0.5, Z: 0.1}, {Y: 0.2, Z: -0.1}}}
	if err := marker.Validate(); err != nil {
		t.Fatal(err)
	}
	wall := FiducialGeometry{ID: "wall-7", Points: []geom.Vec3{{}}}
	if err := wall.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []FiducialGeometry{{ID: "m"}, {Points: []geom.Vec3{{}}}, {ID: "m", Points: []geom.Vec3{{X: math.NaN()}}}, {ID: "m", Carrier: "a b", Points: []geom.Vec3{{}}}} {
		if bad.Validate() == nil {
			t.Fatalf("invalid fiducial accepted: %+v", bad)
		}
	}
}
