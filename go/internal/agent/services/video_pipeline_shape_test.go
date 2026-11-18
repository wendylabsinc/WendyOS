package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func withMJPEGDevice(t *testing.T) {
	t.Helper()
	withYUYVModes(t, [][2]uint32{{1280, 720}})
	prev := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(string, uint32, uint32) bool { return true }
	t.Cleanup(func() { deviceSupportsMJPEGSize = prev })
}

func shapedPlan(shape pipelineShape, req *agentpb.StreamVideoRequest, transport camera.Transport, pw pipeWireSource, available map[string]bool) (gstPipelinePlan, error) {
	return planGStreamerPipelineShaped(shape, "gst", "/dev/video4", req, "x264enc", true, transport, "", pw, available)
}

func TestPlan_MJPEGCaptureTeesDecodedFramesForPixels(t *testing.T) {
	withMJPEGDevice(t)
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, map[string]bool{"x264enc": true, "jpegdec": true})
	if plan.pixels == nil || plan.pixels.GetWidth() != 1280 || plan.pixels.GetHeight() != 720 ||
		plan.pixels.GetFourcc() != fourccYUYV || plan.pixels.GetBytesPerLine() != 2560 {
		t.Errorf("pixels = %v, want 1280x720 YUYV 2560", plan.pixels)
	}
	if plan.raw != nil {
		t.Errorf("raw offered on an MJPEG capture: %v", plan.raw)
	}
	if got, w := plan.rawReason(), "camera is captured as MJPEG at 1280x720; raw frames are not offered"; got != w {
		t.Errorf("rawReason = %q, want %q", got, w)
	}
	joined := strings.Join(plan.args, " ")
	for _, s := range []string{
		"jpegdec ! tee name=rawtap",
		"rawtap. ! " + rawTapQueue + " ! videoconvert ! video/x-raw,format=YUY2 ! fdsink fd=3",
	} {
		if !strings.Contains(joined, s) {
			t.Errorf("pipeline missing %q: %s", s, joined)
		}
	}
	if plan.tapOnly {
		t.Error("an encoding pipeline must not be tapOnly")
	}
}

func TestPlan_WithoutEncoderWritesOnlyTheTap(t *testing.T) {
	check := func(t *testing.T, plan gstPipelinePlan, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		joined := strings.Join(plan.args, " ")
		for _, bad := range []string{"x264enc", "fd=1"} {
			if strings.Contains(joined, bad) {
				t.Errorf("pipeline contains %q: %s", bad, joined)
			}
		}
		for _, want := range []string{"fdsink fd=3", "format=YUY2"} {
			if !strings.Contains(joined, want) {
				t.Errorf("pipeline missing %q: %s", want, joined)
			}
		}
		if !plan.tapOnly || plan.pixels == nil {
			t.Errorf("tapOnly=%v pixels=%v", plan.tapOnly, plan.pixels)
		}
	}
	t.Run("mjpeg", func(t *testing.T) {
		withMJPEGDevice(t)
		plan, err := shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, camera.TransportUSB, pipeWireSource{}, map[string]bool{"x264enc": true, "jpegdec": true})
		check(t, plan, err)
	})
	t.Run("raw yuyv", func(t *testing.T) {
		withYUYVModes(t, [][2]uint32{{640, 480}})
		plan, err := shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, camera.TransportUSB, pipeWireSource{}, map[string]bool{"x264enc": true})
		check(t, plan, err)
	})
}

func TestPlan_WithoutEncoderNeedsATap(t *testing.T) {
	_, err := shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{}, camera.TransportCSI, pipeWireSource{}, map[string]bool{"x264enc": true, "libcamerasrc": true})
	if !errors.Is(err, errNoTapOnlyPipeline) {
		t.Errorf("CSI: err = %v", err)
	}
	_, err = shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{}, camera.TransportUSB, pipeWireSource{serial: 42}, map[string]bool{"x264enc": true, "pipewiresrc": true})
	if !errors.Is(err, errNoTapOnlyPipeline) {
		t.Errorf("PipeWire: err = %v", err)
	}
}

func TestPlan_OversizedMJPEGFrameKeepsTodaysPipeline(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{1920, 1080}})
	prev := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(string, uint32, uint32) bool { return true }
	t.Cleanup(func() { deviceSupportsMJPEGSize = prev })
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}, map[string]bool{"x264enc": true, "jpegdec": true})
	if plan.pixels != nil {
		t.Errorf("pixels = %v for a frame over the limit", plan.pixels)
	}
	if joined := strings.Join(plan.args, " "); strings.Contains(joined, "tee") || !strings.Contains(joined, "jpegdec") {
		t.Errorf("pipeline changed: %s", joined)
	}
}

func TestPlan_DefaultShapeEqualsUnshapedPlanner(t *testing.T) {
	cases := map[string]struct {
		transport camera.Transport
		avail     map[string]bool
	}{
		"raw yuyv": {camera.TransportUSB, map[string]bool{"x264enc": true}},
		"csi":      {camera.TransportCSI, map[string]bool{"x264enc": true, "libcamerasrc": true}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			withYUYVModes(t, [][2]uint32{{640, 480}})
			req := &agentpb.StreamVideoRequest{Width: 640, Height: 480}
			a, errA := planGStreamerPipeline("gst", "/dev/video4", req, "x264enc", true, c.transport, "", pipeWireSource{}, c.avail)
			b, errB := planGStreamerPipelineShaped(pipelineShape{encode: true}, "gst", "/dev/video4", req, "x264enc", true, c.transport, "", pipeWireSource{}, c.avail)
			if errA != nil || errB != nil {
				t.Fatalf("errs: %v %v", errA, errB)
			}
			a.rawWhyLazy, b.rawWhyLazy = nil, nil
			if !reflect.DeepEqual(a, b) {
				t.Errorf("plans differ:\n%+v\n%+v", a, b)
			}
		})
	}
}

func TestTapFormat_FollowsPixelsNotRaw(t *testing.T) {
	withMJPEGDevice(t)
	mjpeg := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, map[string]bool{"x264enc": true, "jpegdec": true})
	if mjpeg.raw != nil || tapFormat(mjpeg) != mjpeg.pixels || tapFormat(mjpeg) == nil {
		t.Errorf("MJPEG: raw=%v tapFormat=%v", mjpeg.raw, tapFormat(mjpeg))
	}
	withYUYVModes(t, [][2]uint32{{640, 480}})
	raw := mustPlan(t, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, map[string]bool{"x264enc": true})
	if raw.raw == nil || tapFormat(raw) != raw.raw {
		t.Errorf("raw: raw=%v tapFormat=%v", raw.raw, tapFormat(raw))
	}
	csi, err := shapedPlan(pipelineShape{encode: true}, &agentpb.StreamVideoRequest{}, camera.TransportCSI, pipeWireSource{}, map[string]bool{"x264enc": true, "libcamerasrc": true})
	if err != nil || tapFormat(csi) != nil {
		t.Errorf("CSI: err=%v tapFormat=%v", err, tapFormat(csi))
	}
}

// --- running the pipeline in the shape its subscribers need ---

// fakeEncodeDemand is a hub's demand as the producer reads it.
type fakeEncodeDemand struct {
	mu       sync.Mutex
	encoded  bool
	raw      bool
	restarts int
	encodes  []bool
	changed  chan struct{}
}

func newFakeEncodeDemand(encoded, raw bool) *fakeEncodeDemand {
	return &fakeEncodeDemand{encoded: encoded, raw: raw, changed: make(chan struct{}, 1)}
}

func (d *fakeEncodeDemand) wantEncoded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.encoded
}

func (d *fakeEncodeDemand) wantRaw() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.raw
}

func (d *fakeEncodeDemand) demandChanged() <-chan struct{} { return d.changed }

func (d *fakeEncodeDemand) encodedStreamRestarted() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restarts++
}

func (d *fakeEncodeDemand) setPipelineEncodes(encodes bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.encodes = append(d.encodes, encodes)
}

// set changes the demand and pokes demandChanged, as a hub does on every join
// and leave.
func (d *fakeEncodeDemand) set(encoded, raw bool) {
	d.mu.Lock()
	d.encoded, d.raw = encoded, raw
	d.mu.Unlock()
	select {
	case d.changed <- struct{}{}:
	default:
	}
}

func (d *fakeEncodeDemand) reported() (restarts int, encodes []bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.restarts, append([]bool(nil), d.encodes...)
}

// recordedLaunches is a launch func that records each shape and blocks until
// its ctx ends, unless failWith returns an error for that shape first.
type recordedLaunches struct {
	failWith func(pipelineShape) error
	started  chan pipelineShape
	mu       sync.Mutex
	shapes   []pipelineShape
	ctxs     []context.Context
}

func newRecordedLaunches() *recordedLaunches {
	return &recordedLaunches{started: make(chan pipelineShape, 64)}
}

func (l *recordedLaunches) launch(ctx context.Context, shape pipelineShape) error {
	l.mu.Lock()
	l.shapes = append(l.shapes, shape)
	l.ctxs = append(l.ctxs, ctx)
	l.mu.Unlock()
	l.started <- shape
	if l.failWith != nil {
		if err := l.failWith(shape); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (l *recordedLaunches) recorded() ([]pipelineShape, []context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]pipelineShape(nil), l.shapes...), append([]context.Context(nil), l.ctxs...)
}

// next waits for the next launch.
func (l *recordedLaunches) next(t *testing.T, within time.Duration) pipelineShape {
	t.Helper()
	select {
	case shape := <-l.started:
		return shape
	case <-time.After(within):
		t.Fatalf("no launch within %v", within)
		return pipelineShape{}
	}
}

// none fails if a launch starts within d.
func (l *recordedLaunches) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case shape := <-l.started:
		t.Fatalf("unexpected launch %+v", shape)
	case <-time.After(d):
	}
}

// runShaped runs runShapedPipelines in the background. stop cancels its ctx and
// returns what it returned.
func runShaped(t *testing.T, demand encodeDemand, grace time.Duration, l *recordedLaunches) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- runShapedPipelines(ctx, demand, grace, l.launch) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Error("runShapedPipelines did not return after its ctx was cancelled")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() }) //nolint:errcheck
	return stop
}

func TestRunShapedPipelines_StartsWithoutEncoderForPixelOnlyDemand(t *testing.T) {
	demand := newFakeEncodeDemand(false, true)
	launches := newRecordedLaunches()
	stop := runShaped(t, demand, time.Second, launches)
	if shape := launches.next(t, time.Second); shape.encode {
		t.Fatal("first launch runs the encoder with only pixel subscribers")
	}
	if restarts, encodes := demand.reported(); restarts != 0 || !reflect.DeepEqual(encodes, []bool{false}) {
		t.Errorf("restarts=%d setPipelineEncodes=%v, want 0 and [false]", restarts, encodes)
	}
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled loop returned %v, want context.Canceled", err)
	}
}

func TestRunShapedPipelines_AddsEncoderAsSoonAsAViewerArrives(t *testing.T) {
	demand := newFakeEncodeDemand(false, true)
	launches := newRecordedLaunches()
	runShaped(t, demand, time.Minute, launches)
	launches.next(t, time.Second)
	demand.set(true, true)
	if shape := launches.next(t, 500*time.Millisecond); !shape.encode {
		t.Fatal("relaunch for a viewer has no encoder")
	}
	_, ctxs := launches.recorded()
	if ctxs[0].Err() == nil {
		t.Error("the launch without the encoder is still running")
	}
	if restarts, encodes := demand.reported(); restarts != 1 || !reflect.DeepEqual(encodes, []bool{false, true}) {
		t.Errorf("restarts=%d setPipelineEncodes=%v, want 1 and [false true]", restarts, encodes)
	}
}

func TestRunShapedPipelines_DropsEncoderOnlyAfterGrace(t *testing.T) {
	const grace = 150 * time.Millisecond
	demand := newFakeEncodeDemand(true, true)
	launches := newRecordedLaunches()
	runShaped(t, demand, grace, launches)
	if shape := launches.next(t, time.Second); !shape.encode {
		t.Fatal("first launch has no encoder for a viewer")
	}
	demand.set(false, true)
	// The grace starts once the loop reads the change, so it cannot end within
	// half of it. The relaunch comes after the rest, with room for a slow runner.
	launches.none(t, grace/2)
	if shape := launches.next(t, grace+2*time.Second); shape.encode {
		t.Fatal("relaunch after the grace still runs the encoder")
	}
	if restarts, encodes := demand.reported(); restarts != 1 || !reflect.DeepEqual(encodes, []bool{true, false}) {
		t.Errorf("restarts=%d setPipelineEncodes=%v, want 1 and [true false]", restarts, encodes)
	}
}

func TestRunShapedPipelines_FlappingViewerKeepsTheEncoder(t *testing.T) {
	// The grace is long next to the 50ms toggles, so a scheduling delay on a
	// loaded runner cannot stretch one absence into a whole grace.
	const grace = time.Second
	demand := newFakeEncodeDemand(true, true)
	launches := newRecordedLaunches()
	runShaped(t, demand, grace, launches)
	launches.next(t, time.Second)
	for i := 0; i < 12; i++ { // 600ms: the viewer leaves and returns every 50ms
		demand.set(i%2 == 1, true)
		time.Sleep(50 * time.Millisecond)
	}
	// Past a grace from the last absence, so a timer any of them left running
	// would have fired by now.
	launches.none(t, grace+50*time.Millisecond)
	if shapes, _ := launches.recorded(); len(shapes) != 1 {
		t.Errorf("%d launches, want exactly one: %+v", len(shapes), shapes)
	}
}

func TestRunShapedPipelines_FallsBackWhenThereIsNoTap(t *testing.T) {
	const grace = 50 * time.Millisecond
	demand := newFakeEncodeDemand(false, true)
	launches := newRecordedLaunches()
	launches.failWith = func(shape pipelineShape) error {
		if !shape.encode {
			return errNoTapOnlyPipeline
		}
		return nil
	}
	runShaped(t, demand, grace, launches)
	if shape := launches.next(t, time.Second); shape.encode {
		t.Fatal("first launch was not without the encoder")
	}
	if shape := launches.next(t, time.Second); !shape.encode {
		t.Fatal("pipeline without a tap was not relaunched with the encoder")
	}
	demand.set(true, true)
	demand.set(false, true)
	launches.none(t, 6*grace)
	shapes, _ := launches.recorded()
	if !reflect.DeepEqual(shapes, []pipelineShape{{encode: false}, {encode: true}}) {
		t.Errorf("launches = %+v, want one without the encoder, then one with it", shapes)
	}
}

func TestRunShapedPipelines_NilDemandLaunchesOnceEncoded(t *testing.T) {
	boom := errors.New("boom")
	var shapes []pipelineShape
	err := runShapedPipelines(context.Background(), nil, time.Millisecond, func(_ context.Context, shape pipelineShape) error {
		shapes = append(shapes, shape)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the launch's own", err)
	}
	if !reflect.DeepEqual(shapes, []pipelineShape{{encode: true}}) {
		t.Errorf("launches = %+v, want one with the encoder", shapes)
	}
}

func TestRunShapedPipelines_ReturnsTheLaunchError(t *testing.T) {
	for name, encoded := range map[string]bool{"with encoder": true, "without encoder": false} {
		t.Run(name, func(t *testing.T) {
			boom := errors.New("boom")
			launches := newRecordedLaunches()
			launches.failWith = func(pipelineShape) error { return boom }
			result := make(chan error, 1)
			go func() {
				result <- runShapedPipelines(context.Background(), newFakeEncodeDemand(encoded, true), time.Millisecond, launches.launch)
			}()
			select {
			case err := <-result:
				if !errors.Is(err, boom) {
					t.Errorf("err = %v, want the launch's own", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("loop did not end when its launch failed")
			}
			if shapes, _ := launches.recorded(); len(shapes) != 1 || shapes[0].encode != encoded {
				t.Errorf("launches = %+v, want one with encode=%v", shapes, encoded)
			}
		})
	}
}

// offerRecorder is a hub sink that records the order of a producer's offers and
// what the tap published. publishRaw answers false, as a hub with nobody left
// does, so a tap-only pipeline ends after its first frame.
type offerRecorder struct {
	mu     sync.Mutex
	events []string
	frames []string
}

func (o *offerRecorder) record(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *offerRecorder) wantRaw() bool { return true }
func (o *offerRecorder) publishRaw(data []byte, _ uint64, _ *agentpb.RawFormat) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.frames = append(o.frames, string(data))
	return false
}
func (o *offerRecorder) rawOffered(*agentpb.RawFormat)    { o.record("raw offered") }
func (o *offerRecorder) rawNotOffered(func() string)      { o.record("raw not offered") }
func (o *offerRecorder) pixelsOffered(*agentpb.RawFormat) { o.record("pixels offered") }

func (o *offerRecorder) seen() (events, frames []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...), append([]string(nil), o.frames...)
}

// fakeGstLaunch writes a stand-in for gst-launch-1.0 that prints "encoded" on
// stdout, writes tap to fd 3 when it is not empty, and then waits.
func fakeGstLaunch(t *testing.T, tap string) string {
	t.Helper()
	script := "#!/bin/sh\nprintf encoded\n"
	if tap != "" {
		script += "printf " + tap + " >&3\n"
	}
	script += "exec /bin/sleep 60\n"
	program := filepath.Join(t.TempDir(), "gst-launch-1.0")
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return program
}

var tinyTapFormat = &agentpb.RawFormat{Width: 2, Height: 1, Fourcc: fourccYUYV, BytesPerLine: 4}

func TestRunCameraPipeline_OffersPixelsBeforeTheRawDecision(t *testing.T) {
	for name, c := range map[string]struct {
		raw, pixels *agentpb.RawFormat
		want        []string
	}{
		"raw capture":   {tinyTapFormat, tinyTapFormat, []string{"pixels offered", "raw offered"}},
		"mjpeg capture": {nil, tinyTapFormat, []string{"pixels offered", "raw not offered"}},
		"no tap":        {nil, nil, []string{"raw not offered"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sink := &offerRecorder{}
			svc := &VideoService{logger: zap.NewNop()}
			plan := gstPipelinePlan{args: []string{fakeGstLaunch(t, "")}, raw: c.raw, pixels: c.pixels, rawWhy: "test"}
			err := svc.runCameraPipeline(ctx, func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return false },
				"/dev/video4", gstEncoderResult{element: "x264enc"}, plan, nil, sink, nil)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if events, _ := sink.seen(); !reflect.DeepEqual(events, c.want) {
				t.Errorf("offers = %v, want %v", events, c.want)
			}
		})
	}
}

func TestRunCameraPipeline_TapOnlyReadsOnlyTheTap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sink := &offerRecorder{}
	svc := &VideoService{logger: zap.NewNop()}
	plan := gstPipelinePlan{args: []string{fakeGstLaunch(t, "abcd")}, pixels: tinyTapFormat, tapOnly: true, rawWhy: "test"}
	var broadcasts atomic.Int32
	err := svc.runCameraPipeline(ctx, func([]byte, frameTimestamp, agentpb.VideoCodec) bool {
		broadcasts.Add(1)
		return true
	}, "/dev/video4", gstEncoderResult{element: "x264enc"}, plan, nil, sink, nil)
	if ctx.Err() != nil {
		t.Fatal("tap-only pipeline did not end when the hub had nobody left")
	}
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if n := broadcasts.Load(); n != 0 {
		t.Errorf("tap-only pipeline broadcast %d stdout chunks", n)
	}
	events, frames := sink.seen()
	if !reflect.DeepEqual(frames, []string{"abcd"}) {
		t.Errorf("tap published %q, want one frame \"abcd\"", frames)
	}
	if want := []string{"pixels offered", "raw not offered"}; !reflect.DeepEqual(events, want) {
		t.Errorf("offers = %v, want %v", events, want)
	}
}

func TestTapDeliveries_CountsTapFramesAndKeepsTheHubsInterfaces(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	var delivered atomic.Bool
	var sink rawSink = tapDeliveries{hubSink: h, delivered: &delivered}
	if _, ok := sink.(encodeDemand); !ok {
		t.Fatal("the wrapped hub hides encodeDemand: the producer would always encode")
	}
	if _, ok := sink.(pixelSink); !ok {
		t.Fatal("the wrapped hub hides pixelSink: pixel subscribers would be turned away")
	}
	if sink.publishRaw([]byte("abcd"), 1, tinyTapFormat) || delivered.Load() {
		t.Fatal("a frame with nobody to receive it counted as delivered")
	}
	id, ch, err := h.subscribePixels()
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(id)
	if !sink.publishRaw([]byte("abcd"), 2, tinyTapFormat) || !delivered.Load() {
		t.Fatal("a tap frame that reached a pixel subscriber did not count as delivered")
	}
	if frame := <-ch; string(frame.data) != "abcd" {
		t.Errorf("pixel subscriber got %q", frame.data)
	}
}

// fakeGStreamer puts stand-ins for gst-inspect-1.0 (listing x264enc, h264parse
// and the extra elements) and gst-launch-1.0 first on PATH. Each launch appends
// its arguments to the returned log; a launch whose pipeline names x264enc
// prints "encoded" on stdout, and every launch writes one tapBytes frame to fd 3
// when tapBytes > 0.
func fakeGStreamer(t *testing.T, tapBytes int, extra ...string) (launchLog string) {
	t.Helper()
	launch := "case \"$*\" in\n*x264enc*) printf encoded ;;\nesac\n"
	if tapBytes > 0 {
		launch += "/usr/bin/head -c " + strconv.Itoa(tapBytes) + " /dev/zero >&3\n"
	}
	launch += "exec /bin/sleep 60\n"
	return fakeGStreamerScript(t, launch, extra...)
}

// fakeGStreamerScript is fakeGStreamer with the rest of gst-launch-1.0 given:
// launchBody runs after the launch has appended its arguments to the log.
func fakeGStreamerScript(t *testing.T, launchBody string, extra ...string) (launchLog string) {
	t.Helper()
	dir := t.TempDir()
	launchLog = filepath.Join(dir, "launches")
	inspect := "echo 'x264: x264enc: H.264 encoder'\necho 'videoparsersbad: h264parse: H.264 parser'\n"
	for _, e := range extra {
		inspect += "echo 'test: " + e + ": test element'\n"
	}
	launch := "echo \"$*\" >> " + launchLog + "\n" + launchBody
	for name, body := range map[string]string{"gst-inspect-1.0": inspect, "gst-launch-1.0": launch} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return launchLog
}

// launchedPipelines reads a fakeGStreamer launch log.
func launchedPipelines(t *testing.T, launchLog string) []string {
	t.Helper()
	b, err := os.ReadFile(launchLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func waitForFrame(t *testing.T, ch chan *videoFrame, what string) *videoFrame {
	t.Helper()
	select {
	case frame, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed waiting for %s", what)
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s within 5s", what)
		return nil
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// streamIntoHub runs streamGStreamer for h's camera in the background, the way
// runProducer feeds a hub. The returned func cancels it and returns its error.
func streamIntoHub(t *testing.T, svc *VideoService, h *deviceHub, req *agentpb.StreamVideoRequest, transport camera.Transport) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- svc.streamGStreamer(ctx, func(data []byte, _ frameTimestamp, codec agentpb.VideoCodec) bool {
			return h.produce(&videoFrame{data: data, codec: codec})
		}, "/dev/video4", req, transport, "", pipeWireSource{}, h)
	}()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-result:
			case <-time.After(10 * time.Second):
				t.Error("streamGStreamer did not return after its ctx was cancelled")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() }) //nolint:errcheck
	return stop
}

func TestStreamGStreamer_RunsTheEncoderOnlyWhileAViewerWatches(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{640, 480}})
	const frameBytes = 640 * 2 * 480
	launchLog := fakeGStreamer(t, frameBytes)
	h, cancelHub := newTestHub(t)
	defer cancelHub()
	pixelID, pixels, err := h.subscribePixels()
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(pixelID)
	svc := &VideoService{logger: zap.NewNop(), encoderIdleGrace: 100 * time.Millisecond}
	stop := streamIntoHub(t, svc, h, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, camera.TransportUSB)

	if frame := waitForFrame(t, pixels, "pixel frame"); len(frame.data) != frameBytes {
		t.Fatalf("pixel frame is %d bytes, want %d", len(frame.data), frameBytes)
	}
	if h.pipelineEncodes() {
		t.Error("pipeline encodes with only a pixel subscriber")
	}
	if got := launchedPipelines(t, launchLog); len(got) != 1 || strings.Contains(got[0], "x264enc") {
		t.Fatalf("launches = %q, want one without the encoder", got)
	}

	viewerID, viewer, err := h.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	if frame := waitForFrame(t, viewer, "encoded chunk"); frame.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || string(frame.data) != "encoded" {
		t.Fatalf("viewer got codec=%v %q", frame.codec, frame.data)
	}
	if !h.pipelineEncodes() {
		t.Error("pipeline does not encode for a viewer")
	}
	if got := launchedPipelines(t, launchLog); len(got) != 2 || !strings.Contains(got[1], "x264enc") {
		t.Fatalf("launches = %q, want a second one with the encoder", got)
	}
	waitForFrame(t, pixels, "pixel frame after the encoder was added")

	h.unsubscribe(viewerID)
	waitUntil(t, "the encoder is dropped after the grace", func() bool { return !h.pipelineEncodes() })
	waitUntil(t, "a third launch starts", func() bool { return len(launchedPipelines(t, launchLog)) >= 3 })
	waitForFrame(t, pixels, "pixel frame after the encoder was dropped")
	if got := launchedPipelines(t, launchLog); len(got) != 3 || strings.Contains(got[2], "x264enc") {
		t.Errorf("launches = %q, want a third and last one without the encoder", got)
	}
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled stream returned %v, want context.Canceled", err)
	}
}

func TestStreamGStreamer_CameraWithoutATapKeepsTheEncoder(t *testing.T) {
	// A libcamerasrc capture has no tap, like a PipeWire one. (In production a
	// CSI camera's producer is given no hub to read demand from at all.)
	launchLog := fakeGStreamer(t, 0, "libcamerasrc")
	h, cancelHub := newTestHub(t)
	defer cancelHub()
	pixelID, pixels, err := h.subscribePixels()
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(pixelID)
	svc := &VideoService{logger: zap.NewNop(), encoderIdleGrace: 50 * time.Millisecond}
	stop := streamIntoHub(t, svc, h, &agentpb.StreamVideoRequest{}, camera.TransportCSI)

	select {
	case _, ok := <-pixels:
		if ok {
			t.Fatal("pixel subscriber got a frame from a camera with no tap")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pixel subscriber was left waiting on a camera with no tap")
	}
	if err := h.subscriberErr(pixelID); !errors.Is(err, errPixelsUnavailable) {
		t.Errorf("pixel subscriber closed with %v, want errPixelsUnavailable", err)
	}
	waitUntil(t, "the pipeline starts", func() bool { return len(launchedPipelines(t, launchLog)) == 1 })
	time.Sleep(200 * time.Millisecond) // four graces: long enough to see a wrong relaunch
	if got := launchedPipelines(t, launchLog); len(got) != 1 || !strings.Contains(got[0], "x264enc") {
		t.Errorf("launches = %q, want exactly one, with the encoder", got)
	}
	if !h.pipelineEncodes() {
		t.Error("pipeline without a tap reports that it does not encode")
	}
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled stream returned %v, want context.Canceled", err)
	}
}

func TestStreamGStreamer_ReappliesStoredControlsOnEveryRelaunch(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{640, 480}})
	launchLog := fakeGStreamer(t, 640*2*480)
	h, cancelHub := newTestHub(t)
	defer cancelHub()
	pixelID, pixels, err := h.subscribePixels()
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(pixelID)
	// The operator stored an exposure. A relaunch closes the camera and opens it
	// again, and a driver may reset its controls when it does.
	var applied atomic.Int32
	svc := &VideoService{
		logger:           zap.NewNop(),
		encoderIdleGrace: 100 * time.Millisecond,
		controls:         newCameraControlStore(filepath.Join(t.TempDir(), "controls.json")),
		controlIndexFor: func(string) (map[string]uint32, error) {
			return map[string]uint32{"exposure_time_absolute": 0x009a0902}, nil
		},
		applyLocalControls: func(path string, ctrls []controlValue) ([]*agentpb.CameraControlResult, error) {
			if path != "/dev/video4" || len(ctrls) != 1 || ctrls[0].name != "exposure_time_absolute" || ctrls[0].value != 20 {
				t.Errorf("applied %v to %s, want exposure_time_absolute=20 on /dev/video4", ctrls, path)
			}
			applied.Add(1)
			return nil, nil
		},
	}
	if err := svc.controls.merge("/dev/video4", []storedControl{{Name: "exposure_time_absolute", Value: 20}}); err != nil {
		t.Fatal(err)
	}
	streamIntoHub(t, svc, h, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, camera.TransportUSB)

	// The first launch is the producer's start, where captureNode applies them.
	waitForFrame(t, pixels, "pixel frame")
	if n := applied.Load(); n != 0 {
		t.Errorf("first launch applied stored controls %d times; captureNode already did", n)
	}
	viewerID, viewer, err := h.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, viewer, "encoded chunk")
	if n := applied.Load(); n != 1 {
		t.Errorf("stored controls applied %d times by the relaunch for a viewer, want once", n)
	}
	h.unsubscribe(viewerID)
	waitUntil(t, "a third launch starts", func() bool { return len(launchedPipelines(t, launchLog)) >= 3 })
	if n := applied.Load(); n != 2 {
		t.Errorf("stored controls applied %d times after two relaunches, want once per relaunch", n)
	}
}

func TestStreamGStreamer_WarnsWhenARelaunchFindsTheCameraTaken(t *testing.T) {
	for name, viewerFirst := range map[string]bool{"relaunch": false, "first launch": true} {
		t.Run(name, func(t *testing.T) {
			withYUYVModes(t, [][2]uint32{{640, 480}})
			// The launch with the encoder finds the camera busy, as when another
			// process opens it in the gap between two launches.
			fakeGStreamerScript(t, "case \"$*\" in\n"+
				"*x264enc*) echo 'ERROR: v4l2src: Device /dev/video4 is busy' >&2; exit 1 ;;\n"+
				"esac\n/usr/bin/head -c "+strconv.Itoa(640*2*480)+" /dev/zero >&3\nexec /bin/sleep 60\n")
			h, cancelHub := newTestHub(t)
			defer cancelHub()
			core, logs := observer.New(zap.WarnLevel)
			svc := &VideoService{logger: zap.New(core), encoderIdleGrace: time.Minute}
			var pixels chan *videoFrame
			if viewerFirst {
				viewerID, _, err := h.subscribe()
				if err != nil {
					t.Fatal(err)
				}
				defer h.unsubscribe(viewerID)
			} else {
				pixelID, ch, err := h.subscribePixels()
				if err != nil {
					t.Fatal(err)
				}
				defer h.unsubscribe(pixelID)
				pixels = ch
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- svc.streamGStreamer(ctx, func(data []byte, _ frameTimestamp, codec agentpb.VideoCodec) bool {
					return h.produce(&videoFrame{data: data, codec: codec})
				}, "/dev/video4", &agentpb.StreamVideoRequest{Width: 640, Height: 480}, camera.TransportUSB, "", pipeWireSource{}, h)
			}()
			if !viewerFirst {
				// The launch without the encoder runs; a viewer relaunches it with one.
				waitForFrame(t, pixels, "pixel frame")
				viewerID, _, err := h.subscribe()
				if err != nil {
					t.Fatal(err)
				}
				defer h.unsubscribe(viewerID)
			}
			var err error
			select {
			case err = <-result:
			case <-time.After(10 * time.Second):
				t.Fatal("stream did not end when its launch found the camera busy")
			}
			if !isCameraInUse(err) {
				t.Fatalf("err = %v, want the camera in use", err)
			}
			var warned []string
			for _, entry := range logs.FilterLevelExact(zap.WarnLevel).All() {
				if entry.ContextMap()["device"] == "/dev/video4" && strings.Contains(entry.Message, "another process") {
					warned = append(warned, entry.Message)
				}
			}
			if viewerFirst && len(warned) != 0 {
				t.Errorf("a first launch that found the camera busy warned of a relaunch: %q", warned)
			}
			if !viewerFirst && len(warned) != 1 {
				t.Errorf("a relaunch that lost the camera logged %q, want one warning naming /dev/video4", warned)
			}
		})
	}
}

func TestRunCameraPipeline_TapOnlyFailureNamesNoEncoder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	program := filepath.Join(t.TempDir(), "gst-launch-1.0")
	script := "#!/bin/sh\necho 'ERROR: from element /GstPipeline:pipeline0/GstJpegDec:jpegdec0: Internal data stream error.' >&2\nexit 1\n"
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.ErrorLevel)
	svc := &VideoService{logger: zap.New(core)}
	plan := gstPipelinePlan{args: []string{program}, pixels: tinyTapFormat, tapOnly: true, rawWhy: "test"}
	err := svc.runCameraPipeline(ctx, func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return true },
		"/dev/video4", gstEncoderResult{element: "x264enc"}, plan, nil, &offerRecorder{}, nil)
	const label = "no encoder (decoded frames only)"
	if err == nil || !strings.Contains(err.Error(), "using "+label) || strings.Contains(err.Error(), "x264enc") {
		t.Errorf("err = %v, want it to name %q and not the encoder that did not run", err, label)
	}
	failed := logs.FilterMessage("GStreamer pipeline failed").All()
	if len(failed) != 1 || failed[0].ContextMap()["encoder"] != label {
		t.Errorf("failure logs = %v, want one with encoder=%q", failed, label)
	}
}
