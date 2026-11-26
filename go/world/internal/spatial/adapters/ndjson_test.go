package adapters

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func header(sensor spatial.SensorID, offset time.Duration) spatial.Header {
	return spatial.Header{Device: "rover", Sensor: sensor, Time: spatial.Stamp(1_700_000_000_123_456_789 + int64(offset))}
}

// everyType is one valid measurement of each of the ten record types.
func everyType() []spatial.Measurement {
	clear := true
	return []spatial.Measurement{
		spatial.SensorManifest{Header: header("camera", 0), Modality: spatial.KindCamera, Model: "cam-x", Intrinsics: &geom.Intrinsics{FX: 800, FY: 800, CX: 640, CY: 360, K1: -0.1, Width: 1280, Height: 720}, Attachment: spatial.Attachment{Mode: spatial.AttachMounted, Carrier: "rover"}},
		spatial.Scan{Header: header("lidar", 0), AngleMin: -math.Pi, AngleIncrement: math.Pi / 2, RangeMin: 0.1, RangeMax: 10, Ranges: []float64{1, spatial.RangeNoReturn, spatial.RangeInvalid, 2.5}, RangeSigma: 0.02},
		spatial.Odometry{Header: header("odom", 0), Pose: geom.Pose2{X: 1, Z: 2, Yaw: 0.3}, ElevationKnown: true, Elevation: 0.3, Noise: spatial.OdometryNoise{TransPerMetre: 0.02, YawPerMetre: 0.002, YawPerRadian: 0.01}},
		spatial.Imu{Header: header("imu", 0), YawRate: 0.1, Accel: geom.Vec3{Y: 9.81}, GyroSigma: 0.005, AccelSigma: 0.05},
		spatial.Baro{Header: header("baro", 0), PressurePa: 101325, Altitude: 1.5, AltitudeSigma: 0.1},
		spatial.GnssFix{Header: header("gnss", 0), LatitudeDegrees: 52.37, LongitudeDegrees: 4.89, AltitudeMetres: 3, HorizontalAccuracy: 1.5, VerticalAccuracy: 2},
		spatial.CameraFrame{Header: header("camera", 0), Width: 1280, Height: 720, PixelSigma: 0.5, Detections: []spatial.Detection{
			{Kind: spatial.DetectFiducial, ID: "tag-1", Index: 2, Pixel: geom.Pixel{U: 10.25, V: 20.5}},
			{Kind: spatial.DetectTargetCorner, ID: "board", Index: 47, Pixel: geom.Pixel{U: 1, V: 2}},
			{Kind: spatial.DetectObject, Pixel: geom.Pixel{U: 5, V: 6}, Class: "person", Confidence: 0.9, BBox: &spatial.BBox{Min: geom.Pixel{U: 1, V: 2}, Max: geom.Pixel{U: 9, V: 10}}},
		}},
		spatial.Range{Header: header("uwb", 0), Peer: "beacon-01", PeerSensor: "uwb", Distance: 3.2, Sigma: 0.1, LineOfSight: &clear},
		spatial.Range{Header: header("uwb", 0), Peer: "beacon-02", Distance: 7.5, Sigma: 0.5}, // same round, same stamp
		spatial.Bearing{Header: header("mic", 0), Azimuth: 0.4, Sigma: 0.05, Source: "alarm"},
		spatial.Scalar{Header: header("gas", 0), Quantity: "co2", Value: 430, Unit: "ppm", Sigma: 5},
	}
}

func readAll(t *testing.T, r *Reader) []spatial.Measurement {
	t.Helper()
	var out []spatial.Measurement
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
}

func TestRecordRoundTripEveryType(t *testing.T) {
	want := everyType()
	types := map[reflect.Type]bool{}
	var log bytes.Buffer
	for _, m := range want {
		types[reflect.TypeOf(m)] = true
		if err := WriteRecord(&log, m); err != nil {
			t.Fatal(err)
		}
	}
	if len(types) != 10 {
		t.Fatalf("fixture covers %d measurement types, want all 10", len(types))
	}
	if n := bytes.Count(log.Bytes(), []byte("\n")); n != len(want) {
		t.Fatalf("%d lines for %d records", n, len(want))
	}
	if got := readAll(t, NewReader(&log, ReaderOptions{})); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the measurements:\n got %+v\nwant %+v", got, want)
	}
	if err := WriteRecord(io.Discard, spatial.Scalar{Header: header("gas", 0)}); err == nil {
		t.Fatal("an invalid measurement was written")
	}
}

// The simulator's stream is what the estimators consume today; a recording of
// it must replay to the same values, in order, through the strict reader.
func TestRecordRoundTripSimulatedStream(t *testing.T) {
	site, err := spatialsim.SingleRoom(3, spatialsim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var want []spatial.Measurement
	var log bytes.Buffer
	kinds := map[string]bool{}
	for m := range site.Stream(site.At(10*time.Second), site.At(12*time.Second), spatialsim.Filter{}) {
		want = append(want, m)
		kinds[reflect.TypeOf(m).Name()] = true
		if err := WriteRecord(&log, m); err != nil {
			t.Fatal(err)
		}
	}
	if len(want) < 100 || len(kinds) < 6 {
		t.Fatalf("thin stream: %d measurements of %d types", len(want), len(kinds))
	}
	r := NewReader(&log, ReaderOptions{})
	if got := readAll(t, r); !reflect.DeepEqual(got, want) || r.Line() != len(want) {
		t.Fatalf("replay differs from the stream (%d of %d records, line %d)", len(got), len(want), r.Line())
	}
}

func encode(t *testing.T, m spatial.Measurement) string {
	t.Helper()
	line, err := spatial.EncodeMeasurement(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func TestReaderIsStrict(t *testing.T) {
	good := encode(t, spatial.Scalar{Header: header("gas", 0), Quantity: "co2", Value: 430, Unit: "ppm", Sigma: 5})
	for name, bad := range map[string]string{
		"not JSON":        `{"type":"scalar","data":`,
		"unknown type":    `{"type":"sonar","data":{}}`,
		"unknown field":   strings.Replace(good, `"unit"`, `"units"`, 1),
		"invalid value":   strings.Replace(good, `"sigma":5`, `"sigma":-5`, 1),
		"trailing record": good + good,
	} {
		// Blank lines and CRLF endings are tolerated and still counted.
		r := NewReader(strings.NewReader(good+"\r\n\n"+good+"\n"+bad+"\n"+good+"\n"), ReaderOptions{})
		for i := 0; i < 2; i++ {
			if _, err := r.Next(); err != nil {
				t.Fatalf("%s: good record %d: %v", name, i, err)
			}
		}
		_, err := r.Next()
		var located *RecordError
		if !errors.As(err, &located) || located.Line != 4 {
			t.Fatalf("%s: error %v, want a RecordError at line 4", name, err)
		}
		if _, again := r.Next(); !errors.Is(again, err) {
			t.Fatalf("%s: the stream continued past a bad record: %v", name, again)
		}
	}

	r := NewReader(strings.NewReader(good+"\n"+good+"\n"), ReaderOptions{MaxLineBytes: len(good)})
	if got := readAll(t, r); len(got) != 2 {
		t.Fatalf("lines of exactly the limit: %d records", len(got))
	}
	for _, input := range []string{good + "\n" + good + " \n", good + "\n" + strings.Repeat("x", 1<<16)} {
		r = NewReader(strings.NewReader(input), ReaderOptions{MaxLineBytes: len(good)})
		if _, err := r.Next(); err != nil {
			t.Fatal(err)
		}
		_, err := r.Next()
		var located *RecordError
		if !errors.Is(err, bufio.ErrTooLong) || !errors.As(err, &located) || located.Line != 2 {
			t.Fatalf("overlong line: %v", err)
		}
	}
}

func TestReaderTimeOrder(t *testing.T) {
	odometry := func(sensor spatial.SensorID, offset time.Duration) string {
		return encode(t, spatial.Odometry{Header: header(sensor, offset)}) + "\n"
	}
	// Two sensors interleaved late, and a repeated stamp: all in order.
	ordered := odometry("odom", 0) + odometry("odom", time.Second) + odometry("other", 0) + odometry("odom", time.Second)
	if got := readAll(t, NewReader(strings.NewReader(ordered), ReaderOptions{})); len(got) != 4 {
		t.Fatalf("%d of 4 ordered records", len(got))
	}

	reversed := odometry("odom", time.Second) + odometry("odom", 0) + odometry("odom", time.Second/2) + odometry("odom", 2*time.Second)
	r := NewReader(strings.NewReader(reversed), ReaderOptions{})
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	_, err := r.Next()
	var located *RecordError
	if !errors.Is(err, ErrOutOfOrder) || !errors.As(err, &located) || located.Line != 2 {
		t.Fatalf("out of order: %v", err)
	}
	r = NewReader(strings.NewReader(reversed), ReaderOptions{AllowOutOfOrder: true})
	if got := readAll(t, r); len(got) != 4 || r.OutOfOrder() != 2 {
		t.Fatalf("tolerant read: %d records, %d out of order, want 4 and 2", len(got), r.OutOfOrder())
	}
}

func TestProvenanceIsNeverSimulationForRealInputs(t *testing.T) {
	m := everyType()[1]
	measured := MeasuredInput(m)
	live, err := spatial.InferredProvenance("localize", "1", "app-1", []world.Input{measured})
	if err != nil {
		t.Fatal(err)
	}
	if measured.ID != spatial.InputID(m) || measured.Origin != world.Measured || measured.Simulation != nil || live.Synthetic() {
		t.Fatalf("measured input %+v", measured)
	}

	replayed := Config{}.ReplayedInput(m)
	replay, err := spatial.InferredProvenance("localize", "1", "app-1", []world.Input{replayed})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Origin != world.Replayed || replayed.Simulation != nil || !replayed.OriginalCaptureTime.Equal(m.When()) || replayed.OriginalClockDomain != DefaultClockDomain || !replay.Synthetic() {
		t.Fatalf("replayed input %+v", replayed)
	}
	if in := (Config{ClockDomain: "trial-7"}).ReplayedInput(m); in.OriginalClockDomain != "trial-7" {
		t.Fatalf("clock domain %q", in.OriginalClockDomain)
	}

	// A sample that arrived in a simulated component stays simulated, and one
	// that arrived in a measured component stays measured.
	sim := world.SimulationContext{RunID: "run-1", ScenarioVersion: "v1", Seed: 7, ClockDomain: "simulation:run-1"}
	in, err := InputFrom(m, world.Provenance{Origin: world.Simulated, AppVersion: "world-sensor-v1", Simulation: &sim})
	if err != nil || in.Origin != world.Simulated || *in.Simulation != sim {
		t.Fatalf("simulated source: %+v %v", in, err)
	}
	in, err = InputFrom(m, world.Provenance{Origin: world.Measured, AppVersion: "bridge-1"})
	if err != nil || in != measured {
		t.Fatalf("measured source: %+v %v", in, err)
	}
	if _, err = InputFrom(m, live); !errors.Is(err, ErrUnmapped) {
		t.Fatalf("an inferred component as a leaf input: %v", err)
	}
	if _, err = InputFrom(m, world.Provenance{Origin: world.Measured}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid source provenance: %v", err)
	}
}
