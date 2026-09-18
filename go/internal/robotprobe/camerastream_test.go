package robotprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// countingReader returns a fixed number of frames and reports the window it was asked
// for, so a rate can be asserted exactly.
type countingReader struct {
	frames []([]byte)
	window time.Duration
	// sleep simulates the time a real sampling window occupies. Without it a stub
	// returns instantly and the elapsed span is nanoseconds, which is the case the
	// rate guard exists for.
	sleep time.Duration
	err   error
}

func (c *countingReader) Sample(_ context.Context, _, _ string, window time.Duration, _ int) ([][]byte, error) {
	c.window = window
	time.Sleep(c.sleep)
	if c.err != nil {
		return nil, c.err
	}
	return c.frames, nil
}

func frames(n int, width, height uint32) [][]byte {
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, testImagePayload("camera_color_optical_frame", width, height, "rgb8"))
	}
	return out
}

// The row this whole effort exists to produce: a camera declaring one thing and
// delivering another, with both numbers in the report and the gap named.
func TestCameraStreamAndCameraInfoTogetherProduceTheFinding(t *testing.T) {
	// The camera's intrinsics were calibrated at 640x480; the stream delivers 848x480.
	info := &fakeReader{byTopic: map[string][][]byte{
		"/camera/color/camera_info": {cameraInfoPayload("camera_color_optical_frame", 640, 480, 603.33, 603.67)},
	}}
	stream := &countingReader{frames: frames(10, 848, 480), sleep: 300 * time.Millisecond}

	registry := robotinspect.NewRegistry()
	registry.MustRegister(CameraInfo{Topics: []string{"/camera/color/camera_info"}})
	registry.MustRegister(CameraStream{Topics: []string{"/camera/color/image_raw"}, Window: 300 * time.Millisecond})

	// One env, two transports: the readers are separate here only so the test can
	// drive each probe's payloads independently.
	env := robotinspect.NewEnv().Offer(robotinspect.RequirementDDSDomain, splitReader{info: info, stream: stream})
	doc := robotinspect.Inspect(context.Background(), registry, env, robotinspect.Target{Device: "unitree-g1-nx-2"})

	width := findProperty(t, doc, "camera.color.resolution.width")
	assessment := width.Assess()
	if assessment.Verdict != robotinspect.VerdictDisagree {
		t.Fatalf("resolution verdict = %q, want %q; 640 declared against 848 delivered is the finding",
			assessment.Verdict, robotinspect.VerdictDisagree)
	}
	if !strings.Contains(assessment.Detail, "640") || !strings.Contains(assessment.Detail, "848") {
		t.Errorf("detail %q should carry both numbers", assessment.Detail)
	}
	if doc.Summarise().Disagree != 1 {
		t.Errorf("disagreements = %d, want 1", doc.Summarise().Disagree)
	}

	// And the field of view derived from those intrinsics is only true at the
	// resolution they were taken at, which the report carries as a condition.
	fov := findProperty(t, doc, "camera.color.fov.vertical")
	if got := fov.Observations[0].Conditions["resolution"]; got != "640x480" {
		t.Errorf("field-of-view resolution condition = %q, want 640x480", got)
	}
}

func TestCameraStreamMeasuresTheRateOverTheWindowItSampled(t *testing.T) {
	// A short but real window: ten frames over 400ms is 25 per second.
	reader := &countingReader{frames: frames(10, 848, 480), sleep: 400 * time.Millisecond}
	probe := CameraStream{Topics: []string{"/camera/color/image_raw"}, Window: 400 * time.Millisecond}

	properties, err := probe.Observe(context.Background(), envWith(reader))
	if err != nil {
		t.Fatal(err)
	}
	if reader.window != 400*time.Millisecond {
		t.Errorf("sampled for %s, want the configured 400ms", reader.window)
	}

	rate := findIn(t, properties, "camera.color.rate")
	observation := rate.Observations[0]
	if observation.Kind != robotinspect.Measured {
		t.Errorf("kind = %q, want %q", observation.Kind, robotinspect.Measured)
	}
	if observation.Sampling == nil || observation.Sampling.Samples != 10 {
		t.Fatalf("sampling = %+v, want 10 samples", observation.Sampling)
	}
	// Scheduling slack moves the elapsed window a little, so allow a band.
	if got := observation.Quantity.Value(); got < 15 || got > 30 {
		t.Errorf("rate = %.1f Hz, want about 25", got)
	}
}

// Sampling can end early on cancellation or a closed participant. A few frames over a
// near-zero window reads as megahertz, so the rate is refused rather than reported.
func TestCameraStreamRefusesARateFromTooShortAWindow(t *testing.T) {
	reader := &countingReader{frames: frames(25, 848, 480)} // returns instantly
	probe := CameraStream{Topics: []string{"/camera/color/image_raw"}, Window: 5 * time.Second}

	properties, err := probe.Observe(context.Background(), envWith(reader))
	if err != nil {
		t.Fatal(err)
	}
	rate := findIn(t, properties, "camera.color.rate")
	if rate.Unknown == nil || rate.Unknown.Reason != robotinspect.ReasonWindowTooShort {
		t.Fatalf("rate = %+v, want an unknown with %q", rate.Unknown, robotinspect.ReasonWindowTooShort)
	}
	if len(rate.Observations) != 0 {
		t.Error("a rate was reported from a window too short to measure one")
	}
	// The resolution seen in those frames is still perfectly good.
	width := findIn(t, properties, "camera.color.resolution.width")
	if len(width.Observations) != 1 {
		t.Error("the measured resolution was discarded along with the rate")
	}
}

func TestCameraStreamUsesADefaultWindowWhenUnset(t *testing.T) {
	reader := &countingReader{frames: frames(3, 640, 480)}
	if _, err := (CameraStream{Topics: []string{"/camera/color/image_raw"}}).Observe(context.Background(), envWith(reader)); err != nil {
		t.Fatal(err)
	}
	if reader.window != DefaultStreamWindow {
		t.Errorf("window = %s, want the %s default", reader.window, DefaultStreamWindow)
	}
}

// A camera publishing nothing is the most important thing this probe can find. Zero
// frames per second would imply it was measured; it was not.
func TestCameraStreamReportsSilenceAsUnknownNotZero(t *testing.T) {
	probe := CameraStream{Topics: []string{"/camera/color/image_raw"}, Window: time.Second}
	properties, err := probe.Observe(context.Background(), envWith(&countingReader{}))
	if err != nil {
		t.Fatal(err)
	}
	rate := findIn(t, properties, "camera.color.rate")
	if rate.Unknown == nil || rate.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Fatalf("rate = %+v, want an unknown with %q", rate.Unknown, robotinspect.ReasonSourceAbsent)
	}
	if len(rate.Observations) != 0 {
		t.Error("a silent camera produced an observation")
	}
}

// A stream that switches resolution mid-window must not be averaged into a number no
// frame ever had.
func TestCameraStreamNamesEveryResolutionItSaw(t *testing.T) {
	mixed := append(frames(3, 848, 480), frames(2, 640, 480)...)
	probe := CameraStream{Topics: []string{"/camera/color/image_raw"}, Window: 300 * time.Millisecond}

	properties, err := probe.Observe(context.Background(),
		envWith(&countingReader{frames: mixed, sleep: 300 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	width := findIn(t, properties, "camera.color.resolution.width")
	conditions := width.Observations[0].Conditions
	if got := conditions["resolutions"]; !strings.Contains(got, "848x480") || !strings.Contains(got, "640x480") {
		t.Errorf("resolutions condition = %q, want both resolutions named", got)
	}
}

func TestCameraStreamSurfacesATransportError(t *testing.T) {
	reader := &countingReader{err: errors.New("participant closed")}
	probe := CameraStream{Topics: []string{"/camera/color/image_raw"}}
	if _, err := probe.Observe(context.Background(), envWith(reader)); err == nil {
		t.Error("a transport failure was swallowed")
	}
}

func TestCameraStreamIsPassive(t *testing.T) {
	if got := (CameraStream{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("Class() = %q, want %q; subscribing to a topic must never be able to actuate", got, robotinspect.ClassPassive)
	}
}

// splitReader routes each probe to its own payloads, standing in for one DDS domain
// carrying both topics.
type splitReader struct {
	info   TopicReader
	stream TopicReader
}

func (s splitReader) Sample(ctx context.Context, topic, typeName string, window time.Duration, max int) ([][]byte, error) {
	if typeName == rosmsg.TypeImage {
		return s.stream.Sample(ctx, topic, typeName, window, max)
	}
	return s.info.Sample(ctx, topic, typeName, window, max)
}

func findProperty(t *testing.T, doc robotinspect.Document, id string) robotinspect.Property {
	t.Helper()
	return findIn(t, doc.Properties, id)
}

func findIn(t *testing.T, properties []robotinspect.Property, id string) robotinspect.Property {
	t.Helper()
	for _, p := range properties {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no property %q in %v", id, propertyIDs(properties))
	return robotinspect.Property{}
}

func testImagePayload(frameID string, width, height uint32, encoding string) []byte {
	var buf []byte
	align := func(n int) {
		for len(buf)%n != 0 {
			buf = append(buf, 0)
		}
	}
	u32 := func(v uint32) { align(4); buf = binary.LittleEndian.AppendUint32(buf, v) }
	str := func(s string) {
		u32(uint32(len(s) + 1))
		buf = append(buf, s...)
		buf = append(buf, 0)
	}

	u32(1758207600)
	u32(250000000)
	str(frameID)
	u32(height)
	u32(width)
	str(encoding)
	buf = append(buf, 0)
	u32(width * 3)
	u32(width * height * 3)
	buf = append(buf, make([]byte, 32)...)
	return append([]byte{0x00, 0x01, 0x00, 0x00}, buf...)
}

var _ = math.Abs
