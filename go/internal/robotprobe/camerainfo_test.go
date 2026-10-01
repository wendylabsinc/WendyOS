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

// fakeReader replays recorded payloads per topic, so a probe is exercised without a
// robot, a network or a DDS domain.
type fakeReader struct {
	byTopic map[string][][]byte
	err     error
	asked   []string
}

func (f *fakeReader) Sample(_ context.Context, topic, typeName string, _ time.Duration, _ int) ([][]byte, error) {
	f.asked = append(f.asked, topic+" "+typeName)
	if f.err != nil {
		return nil, f.err
	}
	return f.byTopic[topic], nil
}

func envWith(reader TopicReader) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementDDSDomain, reader)
}

// A horizontal and a vertical field of view are both correct and always differ. Putting
// them on one property would report every working camera as self-contradictory, so each
// axis is its own property — and the incomparable verdict stays reserved for sources
// that disagree about which axis they are quoting.
func TestCameraInfoKeepsTheTwoAxesOnSeparateProperties(t *testing.T) {
	reader := &fakeReader{byTopic: map[string][][]byte{
		"/camera/color/camera_info": {cameraInfoPayload("camera_color_optical_frame", 640, 480, 603.33, 603.67)},
	}}
	probe := CameraInfo{Topics: []string{"/camera/color/camera_info"}}

	properties, err := probe.Observe(context.Background(), envWith(reader))
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]robotinspect.Property{}
	for _, p := range properties {
		byID[p.ID] = p
	}
	horizontal, ok := byID["camera.color.fov.horizontal"]
	if !ok {
		t.Fatalf("no horizontal field of view; got %v", propertyIDs(properties))
	}
	vertical, ok := byID["camera.color.fov.vertical"]
	if !ok {
		t.Fatalf("no vertical field of view; got %v", propertyIDs(properties))
	}

	for name, p := range map[string]robotinspect.Property{"horizontal": horizontal, "vertical": vertical} {
		if got := p.Assess().Verdict; got != robotinspect.VerdictSingle {
			t.Errorf("%s verdict = %q, want %q; one derived value is not a disagreement",
				name, got, robotinspect.VerdictSingle)
		}
		if got := p.Observations[0].Kind; got != robotinspect.Derived {
			t.Errorf("%s kind = %q, want %q; the angle is computed from published focal lengths",
				name, got, robotinspect.Derived)
		}
		if got := p.Observations[0].Conditions["resolution"]; got != "640x480" {
			t.Errorf("%s resolution condition = %q, want 640x480; the angle is only true there", name, got)
		}
	}

	if got := vertical.Observations[0].Quantity.Value(); math.Abs(got-43.4) > 0.05 {
		t.Errorf("vertical = %.2f, want 43.4 (the figure measured on the robot)", got)
	}
	if got := horizontal.Observations[0].Quantity.Value(); math.Abs(got-55.9) > 0.05 {
		t.Errorf("horizontal = %.2f, want 55.9", got)
	}
	if got := vertical.Observations[0].Quantity.Axis(); got != robotinspect.AxisVertical {
		t.Errorf("axis = %q, want %q", got, robotinspect.AxisVertical)
	}

	if len(reader.asked) != 1 || !strings.Contains(reader.asked[0], rosmsg.TypeCameraInfo) {
		t.Errorf("sampled %v, want one read of the CameraInfo type", reader.asked)
	}
}

func TestCameraInfoReportsUncalibratedInsteadOfAnAngle(t *testing.T) {
	reader := &fakeReader{byTopic: map[string][][]byte{
		"/camera/depth/camera_info": {cameraInfoPayload("camera_depth_optical_frame", 848, 480, 0, 0)},
	}}
	probe := CameraInfo{Topics: []string{"/camera/depth/camera_info"}}

	properties, err := probe.Observe(context.Background(), envWith(reader))
	if err != nil {
		t.Fatalf("an uncalibrated camera is a finding, not a probe failure: %v", err)
	}

	var sawUnknown bool
	for _, p := range properties {
		if !strings.HasPrefix(p.ID, "camera.depth.fov") {
			continue
		}
		sawUnknown = true
		assessment := p.Assess()
		if assessment.Verdict != robotinspect.VerdictUnknown {
			t.Errorf("%s verdict = %q, want %q", p.ID, assessment.Verdict, robotinspect.VerdictUnknown)
		}
		if p.Unknown == nil || p.Unknown.Reason != robotinspect.ReasonUncalibrated {
			t.Errorf("%s should report %q, got %+v", p.ID, robotinspect.ReasonUncalibrated, p.Unknown)
		}
	}
	if !sawUnknown {
		t.Fatalf("the uncalibrated camera left no row at all; got %v", propertyIDs(properties))
	}
	// The resolution it claims is still worth having even with no usable intrinsics.
	if !contains(propertyIDs(properties), "camera.depth.resolution") {
		t.Errorf("dropped the declared resolution; got %v", propertyIDs(properties))
	}
}

func TestCameraInfoReportsAnAbsentPublisher(t *testing.T) {
	probe := CameraInfo{Topics: []string{"/camera/color/camera_info"}}
	properties, err := probe.Observe(context.Background(), envWith(&fakeReader{}))
	if err != nil {
		t.Fatalf("a topic nobody publishes on is a finding, not a failure: %v", err)
	}
	if len(properties) == 0 {
		t.Fatal("an absent publisher left no row")
	}
	for _, p := range properties {
		if p.Unknown == nil || p.Unknown.Reason != robotinspect.ReasonSourceAbsent {
			t.Errorf("%s should report %q, got %+v", p.ID, robotinspect.ReasonSourceAbsent, p.Unknown)
		}
	}
}

func TestCameraInfoSurfacesATransportError(t *testing.T) {
	reader := &fakeReader{err: errors.New("no participant on domain 0")}
	probe := CameraInfo{Topics: []string{"/camera/color/camera_info"}}
	if _, err := probe.Observe(context.Background(), envWith(reader)); err == nil {
		t.Error("a transport failure was swallowed")
	}
}

func TestCameraInfoRefusesAnEnvironmentWithoutADDSHandle(t *testing.T) {
	probe := CameraInfo{Topics: []string{"/camera/color/camera_info"}}
	if _, err := probe.Observe(context.Background(), robotinspect.NewEnv()); err == nil {
		t.Error("the probe ran without a DDS handle")
	}
	wrong := robotinspect.NewEnv().Offer(robotinspect.RequirementDDSDomain, "not a reader")
	if _, err := probe.Observe(context.Background(), wrong); err == nil {
		t.Error("the probe accepted a handle that is not a TopicReader")
	}
}

// End to end through the registry: the probe is selected only when a DDS domain is on
// offer, and the document it produces states that nothing was commanded.
func TestCameraInfoRunsThroughInspectAndStaysPassive(t *testing.T) {
	reader := &fakeReader{byTopic: map[string][][]byte{
		"/camera/color/camera_info": {cameraInfoPayload("camera_color_optical_frame", 640, 480, 603.33, 603.67)},
	}}
	probe := CameraInfo{Topics: []string{"/camera/color/camera_info"}}

	registry := robotinspect.NewRegistry()
	registry.MustRegister(probe)

	// No DDS on offer: the probe is skipped with a reason, and the wanted properties
	// come back unknown rather than missing.
	bare := robotinspect.Inspect(context.Background(), registry, robotinspect.NewEnv(),
		robotinspect.Target{Device: "unitree-g1-nx-2", Want: probe.Provides()})
	if got := bare.Skipped[probe.ID()].Reason; got != robotinspect.ReasonRequirementUnmet {
		t.Errorf("skip reason = %q, want %q", got, robotinspect.ReasonRequirementUnmet)
	}
	if got := bare.Summarise().Unknown; got != len(probe.Provides()) {
		t.Errorf("unknowns = %d, want %d", got, len(probe.Provides()))
	}

	doc := robotinspect.Inspect(context.Background(), registry, envWith(reader),
		robotinspect.Target{Device: "unitree-g1-nx-2", VendorKind: "unitree-g1", Want: probe.Provides()})
	if !doc.PassiveOnly {
		t.Error("PassiveOnly = false for a plan containing only this probe")
	}
	if got := doc.Summarise().Findings(); got != 0 {
		t.Errorf("findings = %d, want 0; a correctly working camera must not read as a problem", got)
	}
	if !contains(propertyIDs(doc.Properties), "camera.color.fov.vertical") {
		t.Errorf("document is missing the vertical field of view; got %v", propertyIDs(doc.Properties))
	}
	if _, err := doc.CanonicalJSON(); err != nil {
		t.Errorf("document does not serialise: %v", err)
	}
}

func TestStreamNameReducesATopicToItsStream(t *testing.T) {
	for topic, want := range map[string]string{
		"/camera/color/camera_info":     "color",
		"/camera/depth/camera_info":     "depth",
		"camera/infra1/camera_info":     "infra1",
		"/front_camera/rgb/camera_info": "front_camera.rgb",
		"/camera_info":                  "default",
		"":                              "default",
	} {
		if got := streamName(topic); got != want {
			t.Errorf("streamName(%q) = %q, want %q", topic, got, want)
		}
	}
}

// cameraInfoPayload encodes a CameraInfo through the K matrix, which is all the decoder
// reads. Alignment is paid explicitly; getting it wrong misreads every later field.
func cameraInfoPayload(frameID string, width, height uint32, fx, fy float64) []byte {
	var buf []byte
	align := func(n int) {
		for len(buf)%n != 0 {
			buf = append(buf, 0)
		}
	}
	u32 := func(v uint32) { align(4); buf = binary.LittleEndian.AppendUint32(buf, v) }
	f64 := func(v float64) { align(8); buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(v)) }
	str := func(s string) {
		u32(uint32(len(s) + 1))
		buf = append(buf, s...)
		buf = append(buf, 0)
	}

	u32(1758207600) // header.stamp.sec
	u32(250000000)  // header.stamp.nanosec
	str(frameID)
	u32(height)
	u32(width)
	str("plumb_bob")
	u32(5) // distortion coefficients
	for i := 0; i < 5; i++ {
		f64(0)
	}
	for _, v := range []float64{fx, 0, float64(width) / 2, 0, fy, float64(height) / 2, 0, 0, 1} {
		f64(v)
	}
	return append([]byte{0x00, 0x01, 0x00, 0x00}, buf...)
}

func propertyIDs(properties []robotinspect.Property) []string {
	ids := make([]string, 0, len(properties))
	for _, p := range properties {
		ids = append(ids, p.ID)
	}
	return ids
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
