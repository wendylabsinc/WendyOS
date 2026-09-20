package robotprobe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

type fakeCameras struct {
	devices   []CameraDevice
	frames    map[string][]CameraFrame
	sampleErr map[string]error
	listErr   error
	dwell     time.Duration
}

func (f fakeCameras) Cameras(context.Context) ([]CameraDevice, error) {
	return f.devices, f.listErr
}

func (f fakeCameras) SampleCamera(_ context.Context, stableID string, _ time.Duration, _ int, _ CameraMode) ([]CameraFrame, error) {
	if f.dwell > 0 {
		time.Sleep(f.dwell)
	}
	if err := f.sampleErr[stableID]; err != nil {
		return nil, err
	}
	return f.frames[stableID], nil
}

func cameraEnv(source CameraSource) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementCameraTransport, source)
}

func rawFrames(n int, width, height uint32, age time.Duration) []CameraFrame {
	now := time.Now()
	out := make([]CameraFrame, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, CameraFrame{
			Width: width, Height: height, Fourcc: "YUYV", Codec: "RAW",
			TimestampNs: uint64(now.Add(-age).UnixNano()),
			ReceivedAt:  now,
		})
	}
	return out
}

// The point of this probe: a camera with no ROS node anywhere is still reported. This is
// the case an arm on a USB cable presents, and the DDS probes cannot see it at all.
func TestCameraReportsAUSBCameraWithNoROSInvolved(t *testing.T) {
	source := fakeCameras{
		dwell: 300 * time.Millisecond,
		devices: []CameraDevice{{
			StableID: "usb-046d_081b-video0", Name: "C310", Path: "/dev/video0",
			Model: "Logitech C310", Driver: "uvcvideo", Transport: "USB", Online: true,
		}},
		frames: map[string][]CameraFrame{
			"usb-046d_081b-video0": rawFrames(9, 640, 480, 40*time.Millisecond),
		},
	}

	properties, err := Camera{Window: 300 * time.Millisecond}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}

	if got := findIn(t, properties, "camera.count").Observations[0].Quantity.Value(); got != 1 {
		t.Errorf("camera.count = %v, want 1", got)
	}
	prefix := "camera.video0"
	if got := findIn(t, properties, prefix+".transport").Observations[0].Text; got != "USB" {
		t.Errorf("transport = %q, want USB", got)
	}
	if got := findIn(t, properties, prefix+".resolution").Observations[0].Text; got != "640x480" {
		t.Errorf("resolution = %q, want 640x480", got)
	}
	if got := findIn(t, properties, prefix+".rate").Observations[0].Quantity.Value(); got < 15 || got > 45 {
		t.Errorf("rate = %.1f Hz, want about 30", got)
	}
	age := findIn(t, properties, prefix+".frame_age").Observations[0]
	if got := age.Quantity.Value(); math.Abs(got-40) > 15 {
		t.Errorf("frame_age = %.1f ms, want about 40", got)
	}
	if age.Kind != robotinspect.Measured {
		t.Errorf("frame_age kind = %q, want measured", age.Kind)
	}
}

// Two cameras must never share an identifier — the lesson from the DDS stream-name
// collision, applied here before it can bite.
func TestCameraKeepsEveryDeviceOnItsOwnIdentifier(t *testing.T) {
	source := fakeCameras{
		dwell: 300 * time.Millisecond,
		devices: []CameraDevice{
			{StableID: "realsense-color", Path: "/dev/video4", Model: "D435i", Transport: "USB", Online: true},
			{StableID: "realsense-depth", Path: "/dev/video0", Model: "D435i", Transport: "USB", Online: true},
		},
		frames: map[string][]CameraFrame{
			"realsense-color": rawFrames(5, 848, 480, 30*time.Millisecond),
			"realsense-depth": rawFrames(5, 1280, 720, 30*time.Millisecond),
		},
	}

	properties, err := Camera{Window: 300 * time.Millisecond}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "camera.video4.resolution").Observations[0].Text; got != "848x480" {
		t.Errorf("color resolution = %q", got)
	}
	if got := findIn(t, properties, "camera.video0.resolution").Observations[0].Text; got != "1280x720" {
		t.Errorf("depth resolution = %q", got)
	}
	seen := map[string]int{}
	for _, p := range properties {
		seen[p.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("property %q appeared %d times", id, count)
		}
	}
}

// The EBUSY case from the G1: another app holds the camera. That is an answer with a
// reason, not a probe failure, and the rest of the camera's facts survive.
func TestCameraReportsAHeldDeviceWithItsReason(t *testing.T) {
	source := fakeCameras{
		devices: []CameraDevice{{StableID: "realsense-color", Path: "/dev/video4", Transport: "USB", Model: "D435i", Online: true}},
		sampleErr: map[string]error{
			"realsense-color": fmt.Errorf("%w: camera /dev/video4 is already in use by another application", ErrDeviceBusy),
		},
	}

	properties, err := Camera{Window: time.Second}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatalf("a held camera is a finding, not a failure: %v", err)
	}
	resolution := findIn(t, properties, "camera.video4.resolution")
	if resolution.Unknown == nil || resolution.Unknown.Reason != robotinspect.ReasonHeldByOther {
		t.Fatalf("resolution = %+v, want %q", resolution.Unknown, robotinspect.ReasonHeldByOther)
	}
	if !strings.Contains(resolution.Unknown.Detail, "already in use") {
		t.Errorf("detail %q should carry the agent's reason", resolution.Unknown.Detail)
	}
	// The inventory is still reported.
	findIn(t, properties, "camera.video4.model")
}

func TestCameraReportsAnOfflineDeviceWithoutSamplingIt(t *testing.T) {
	source := fakeCameras{
		devices: []CameraDevice{{StableID: "ipcam-lobby", Name: "ipcam-lobby", Transport: "IP", Online: false}},
	}
	properties, err := Camera{}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}
	rate := findIn(t, properties, "camera.ipcam-lobby.rate")
	if rate.Unknown == nil || rate.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("rate = %+v, want an unknown", rate.Unknown)
	}
}

// An encoded stream often carries no frame geometry. Inferring a resolution from a codec
// would not be measuring it.
func TestCameraReportsUnknownResolutionForAnEncodedStream(t *testing.T) {
	now := time.Now()
	source := fakeCameras{
		dwell:   300 * time.Millisecond,
		devices: []CameraDevice{{StableID: "csi-cam0", Path: "/dev/video0", Transport: "CSI", Online: true}},
		frames: map[string][]CameraFrame{
			"csi-cam0": {
				{Codec: "H264", TimestampNs: uint64(now.UnixNano()), ReceivedAt: now},
				{Codec: "H264", TimestampNs: uint64(now.UnixNano()), ReceivedAt: now},
			},
		},
	}
	properties, err := Camera{Window: 300 * time.Millisecond}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}
	resolution := findIn(t, properties, "camera.video0.resolution")
	if resolution.Unknown == nil || resolution.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Fatalf("resolution = %+v, want an unknown", resolution.Unknown)
	}
	// The rate is still perfectly measurable from an encoded stream.
	findIn(t, properties, "camera.video0.rate")
}

// A robot with no cameras says so with a zero, which only works because a real zero is
// representable.
func TestCameraReportsNoneAsZeroRatherThanOmittingTheRow(t *testing.T) {
	properties, err := Camera{}.Observe(context.Background(), cameraEnv(fakeCameras{}))
	if err != nil {
		t.Fatal(err)
	}
	count := findIn(t, properties, "camera.count")
	if len(count.Observations) != 1 || count.Observations[0].Quantity.Value() != 0 {
		t.Fatalf("camera.count = %+v, want a recorded zero", count)
	}
}

// A stream that would not start for some other reason must not be reported as held by
// another application. Naming a cause we cannot establish is the failure mode this whole
// command exists to avoid.
func TestCameraDoesNotGuessThatAnUnexplainedFailureMeansBusy(t *testing.T) {
	source := fakeCameras{
		devices: []CameraDevice{{StableID: "cam", Path: "/dev/video9", Transport: "USB", Online: true}},
		sampleErr: map[string]error{
			"cam": errors.New("failed to configure video device"),
		},
	}
	properties, err := Camera{Window: time.Second}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}
	rate := findIn(t, properties, "camera.video9.rate")
	if rate.Unknown == nil || rate.Unknown.Reason != robotinspect.ReasonProbeFailed {
		t.Errorf("rate = %+v, want %q rather than a guess at HELD_BY_OTHER",
			rate.Unknown, robotinspect.ReasonProbeFailed)
	}
}

func TestCameraSurfacesAnEnumerationFailure(t *testing.T) {
	if _, err := (Camera{}).Observe(context.Background(),
		cameraEnv(fakeCameras{listErr: errors.New("agent unreachable")})); err == nil {
		t.Error("an enumeration failure was swallowed")
	}
}

func TestCameraIsPassive(t *testing.T) {
	if got := (Camera{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("class = %q, want passive", got)
	}
}

func TestSanitiseKeyKeepsIdentifiersDiffable(t *testing.T) {
	for value, want := range map[string]string{
		"/dev/video0":           "dev_video0",
		"usb-046d_081b-video0":  "usb-046d_081b-video0",
		"Intel RealSense D435i": "intel_realsense_d435i",
		"":                      "unknown",
		"///":                   "unknown",
	} {
		if got := sanitiseKey(value); got != want {
			t.Errorf("sanitiseKey(%q) = %q, want %q", value, got, want)
		}
	}
}

// The campaign's shape: a configuration asserting one thing and a robot delivering
// another. The agent has no RPC for what a camera is set to, so the request is the claim
// — recorded with origin "requested" so nobody can mistake it for something the camera
// said about itself.
func TestCameraComparesWhatWasAskedForAgainstWhatArrived(t *testing.T) {
	source := fakeCameras{
		dwell:   900 * time.Millisecond,
		devices: []CameraDevice{{StableID: "rs", Path: "/dev/video4", Transport: "USB", Online: true}},
		frames: map[string][]CameraFrame{
			// Asked for 1280x720 at 30; the camera delivers 848x480 at about 5.5.
			"rs": rawFrames(5, 848, 480, 30*time.Millisecond),
		},
	}
	probe := Camera{
		Window: 900 * time.Millisecond,
		Mode:   CameraMode{Width: 1280, Height: 720, Framerate: 30},
	}

	properties, err := probe.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}

	// Two probes' worth of observations land on one property, so merge them the way
	// an inspection would.
	byID := map[string][]robotinspect.Observation{}
	for _, p := range properties {
		byID[p.ID] = append(byID[p.ID], p.Observations...)
	}

	resolution := robotinspect.Property{ID: "camera.video4.resolution", Observations: byID["camera.video4.resolution"]}
	assessment := resolution.Assess()
	if assessment.Verdict != robotinspect.VerdictDisagree {
		t.Fatalf("resolution verdict = %q, want %q (1280x720 asked, 848x480 delivered)",
			assessment.Verdict, robotinspect.VerdictDisagree)
	}
	for _, want := range []string{"1280x720", "848x480"} {
		if !strings.Contains(assessment.Detail, want) {
			t.Errorf("detail %q should carry both resolutions", assessment.Detail)
		}
	}

	rate := robotinspect.Property{ID: "camera.video4.rate", Observations: byID["camera.video4.rate"]}
	if got := rate.Assess().Verdict; got != robotinspect.VerdictDisagree {
		t.Errorf("rate verdict = %q, want %q (30 Hz asked, about 5 delivered)", got, robotinspect.VerdictDisagree)
	}

	// The request must never be attributed to the camera.
	for _, o := range byID["camera.video4.resolution"] {
		if o.Kind == robotinspect.Declared && o.Source.Origin != "requested" {
			t.Errorf("the asked-for value is attributed to %q, not to the caller", o.Source.Origin)
		}
	}
}

// With no mode asked for, the probe reports delivery alone and invents no claim.
func TestCameraWithoutARequestedModeMakesNoClaim(t *testing.T) {
	source := fakeCameras{
		dwell:   300 * time.Millisecond,
		devices: []CameraDevice{{StableID: "rs", Path: "/dev/video4", Transport: "USB", Online: true}},
		frames:  map[string][]CameraFrame{"rs": rawFrames(5, 848, 480, 20*time.Millisecond)},
	}
	properties, err := Camera{Window: 300 * time.Millisecond}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range properties {
		for _, o := range p.Observations {
			if o.Source.Origin == "requested" {
				t.Errorf("%s carries a requested value when nothing was asked for", p.ID)
			}
		}
	}
	if got := findIn(t, properties, "camera.video4.resolution").Assess().Verdict; got != robotinspect.VerdictSingle {
		t.Errorf("verdict = %q, want %q", got, robotinspect.VerdictSingle)
	}
}

// Asking for a mode a camera will not serve must not cost the measurement. The operator
// wants both facts: the request was refused, and here is what the camera does instead.
func TestCameraFallsBackToTheDefaultWhenARequestIsRefused(t *testing.T) {
	refusals := map[string]error{"rs": errors.New("resolution 1280x720 not advertised by this camera")}
	source := &refusingCameras{
		refuseOnRequest: refusals,
		devices:         []CameraDevice{{StableID: "rs", Path: "/dev/video4", Transport: "USB", Online: true}},
		frames:          rawFrames(8, 848, 480, 30*time.Millisecond),
		dwell:           400 * time.Millisecond,
	}
	probe := Camera{Window: 400 * time.Millisecond, Mode: CameraMode{Width: 1280, Height: 720, Framerate: 30}}

	properties, err := probe.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string][]robotinspect.Observation{}
	for _, p := range properties {
		byID[p.ID] = append(byID[p.ID], p.Observations...)
	}

	resolution := robotinspect.Property{ID: "camera.video4.resolution", Observations: byID["camera.video4.resolution"]}
	if got := resolution.Assess().Verdict; got != robotinspect.VerdictDisagree {
		t.Fatalf("verdict = %q, want %q: the request and the delivery must both be present",
			got, robotinspect.VerdictDisagree)
	}

	// And the reason the delivery differs travels with it.
	var carried bool
	for _, o := range byID["camera.video4.resolution"] {
		if strings.Contains(o.Conditions["requested_mode_refused"], "not advertised") {
			carried = true
		}
	}
	if !carried {
		t.Error("the refusal reason was lost; the report would show a mismatch with no explanation")
	}
	if source.attempts != 2 {
		t.Errorf("sampled %d times, want a request then a fallback", source.attempts)
	}
}

// A rate needs enough arrivals, not just enough seconds. Real use produced "2 Hz" from
// two frames for a camera that measures five over a longer window.
func TestCameraRefusesARateFromTooFewFrames(t *testing.T) {
	source := fakeCameras{
		dwell:   1100 * time.Millisecond,
		devices: []CameraDevice{{StableID: "rs", Path: "/dev/video2", Transport: "USB", Online: true}},
		frames:  map[string][]CameraFrame{"rs": rawFrames(2, 848, 480, 30*time.Millisecond)},
	}
	properties, err := Camera{Window: time.Second}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}
	rate := findIn(t, properties, "camera.video2.rate")
	if rate.Unknown == nil || rate.Unknown.Reason != robotinspect.ReasonWindowTooShort {
		t.Fatalf("rate = %+v, want an unknown rather than a confident 2 Hz", rate.Unknown)
	}
	if !strings.Contains(rate.Unknown.Detail, "too few") {
		t.Errorf("detail %q should say the sample count was the problem", rate.Unknown.Detail)
	}
	// The resolution seen in those two frames is still perfectly good.
	findIn(t, properties, "camera.video2.resolution")
}

// refusingCameras refuses a configured request once, then serves the default.
type refusingCameras struct {
	devices         []CameraDevice
	frames          []CameraFrame
	refuseOnRequest map[string]error
	dwell           time.Duration
	attempts        int
}

func (r *refusingCameras) Cameras(context.Context) ([]CameraDevice, error) { return r.devices, nil }

func (r *refusingCameras) SampleCamera(_ context.Context, stableID string, _ time.Duration, _ int, mode CameraMode) ([]CameraFrame, error) {
	r.attempts++
	if r.dwell > 0 {
		time.Sleep(r.dwell)
	}
	if mode.Requested() {
		if err := r.refuseOnRequest[stableID]; err != nil {
			return nil, err
		}
	}
	return r.frames, nil
}

// The agent never probes a local V4L2 device, so its liveness flag stays at the zero
// value. Reading that as "offline" condemned three working RealSense nodes on a G1 every
// run — and, worse, returned before measuring, so the reading that would have disproved it
// was never taken.
func TestCameraDoesNotCallALocalCameraOfflineOnAnUnsetFlag(t *testing.T) {
	source := fakeCameras{
		dwell: 300 * time.Millisecond,
		// Online is false because nothing ever set it, which is the whole point.
		devices: []CameraDevice{{StableID: "usb:0", Path: "/dev/video0", Transport: "USB"}},
		frames:  map[string][]CameraFrame{"usb:0": rawFrames(8, 640, 480, 20*time.Millisecond)},
	}

	properties, err := Camera{Window: time.Second}.Observe(context.Background(), cameraEnv(source))
	if err != nil {
		t.Fatal(err)
	}

	resolution := findIn(t, properties, "camera.video0.resolution")
	if resolution.Unknown != nil {
		t.Fatalf("a working USB camera was reported unmeasurable: %s", resolution.Unknown.Detail)
	}
	if len(resolution.Observations) == 0 {
		t.Fatal("the camera was never measured")
	}
}

// Where the agent does probe, a false flag is a fact and must still be honoured.
func TestCameraStillReportsAProbedCameraOffline(t *testing.T) {
	for _, transport := range []string{"IP", "ROS2"} {
		source := fakeCameras{
			devices: []CameraDevice{{StableID: "cam", Transport: transport, Online: false}},
		}
		properties, err := Camera{Window: time.Second}.Observe(context.Background(), cameraEnv(source))
		if err != nil {
			t.Fatal(err)
		}
		resolution := findIn(t, properties, "camera.cam.resolution")
		if resolution.Unknown == nil {
			t.Fatalf("%s: an offline camera was measured anyway", transport)
		}
		if !strings.Contains(resolution.Unknown.Detail, "offline") {
			t.Fatalf("%s: unexpected reason %q", transport, resolution.Unknown.Detail)
		}
	}
}
