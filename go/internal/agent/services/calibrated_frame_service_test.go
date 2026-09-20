package services

// Every promise this service makes, proved without a camera.
//
// The failure that motivated the whole feature was invisible: depth went away,
// four safety checks went inert at once, and the robot confidently read a name
// off a wall poster. A property that can only be checked by plugging in a D435i
// is a property that regresses exactly the same way, so all of it — the
// refusals, the mid-stream loss, the fan-out, the degradation when the capture
// helper is absent — is driven here from a source a test pushes frames into.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/agent/framesource/framesourcetest"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const (
	alignedDepth       = agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH
	measuredIntrinsics = agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS
	captureSettings    = agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS
)

// --- harness ---

func newCalibratedService(sources ...framesource.Source) *CalibratedFrameService {
	reg := framesource.NewRegistry(framesource.ProviderFunc(
		func(context.Context) ([]framesource.Source, error) { return sources, nil }))
	return NewCalibratedFrameService(context.Background(), zap.NewNop(), reg)
}

// calibratedStreamStub is the subscriber side. gate, when set, makes Send block
// until a token is handed over — which is how a "slow analytics consumer" is
// expressed without a sleep.
type calibratedStreamStub struct {
	grpc.ServerStreamingServer[agentpbv2.CalibratedFrame]
	ctx  context.Context
	gate chan struct{}

	mu   sync.Mutex
	sent []*agentpbv2.CalibratedFrame
}

func newStreamStub(ctx context.Context) *calibratedStreamStub {
	return &calibratedStreamStub{ctx: ctx}
}

func (s *calibratedStreamStub) Context() context.Context { return s.ctx }

func (s *calibratedStreamStub) Send(f *agentpbv2.CalibratedFrame) error {
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	s.sent = append(s.sent, f)
	s.mu.Unlock()
	return nil
}

func (s *calibratedStreamStub) frames() []*agentpbv2.CalibratedFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*agentpbv2.CalibratedFrame(nil), s.sent...)
}

func (s *calibratedStreamStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// run starts a subscription in the background and returns its error channel.
func run(svc *CalibratedFrameService, req *agentpbv2.StreamCalibratedFramesRequest, stream *calibratedStreamStub) chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- svc.StreamCalibratedFrames(req, stream) }()
	return errCh
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForSubscribers blocks until the hub for name has n subscribers. Fan-out
// is latest-wins with no replay, so a frame pushed before a joiner has actually
// joined is one that joiner never sees.
func waitForSubscribers(t *testing.T, svc *CalibratedFrameService, name string, n int) {
	t.Helper()
	waitFor(t, "subscribers to join", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		hub := svc.hubs[name]
		if hub == nil {
			return false
		}
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.subs) == n
	})
}

func awaitErr(t *testing.T, errCh chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end")
		return nil
	}
}

// --- requirements are refused by name, before the first frame ---

func TestCalibratedFrames_RequirementTheSourceLacksIsRefusedByName(t *testing.T) {
	// A source that has everything EXCEPT the requirement under test, so the
	// refusal can only be about that one property.
	for _, tc := range []struct {
		name     string
		provides []agentpbv2.FrameRequirement
		require  agentpbv2.FrameRequirement
		slug     string
	}{
		{"no depth", []agentpbv2.FrameRequirement{measuredIntrinsics, captureSettings}, alignedDepth, framesource.SlugAlignedDepth},
		{"no calibration", []agentpbv2.FrameRequirement{alignedDepth, captureSettings}, measuredIntrinsics, framesource.SlugMeasuredIntrinsics},
		{"no settings readback", []agentpbv2.FrameRequirement{alignedDepth, measuredIntrinsics}, captureSettings, framesource.SlugCaptureSettings},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := framesourcetest.NewFake("fake:1", 64, 48, tc.provides...)
			svc := newCalibratedService(fake)

			stream := newStreamStub(context.Background())
			err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{
				Source: "fake:1", Require: []agentpbv2.FrameRequirement{tc.require},
			}, stream)

			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition (err: %v)", status.Code(err), err)
			}
			if !streamreason.Has(err, streamreason.RequirementUnmet) {
				t.Errorf("error does not carry REQUIREMENT_UNMET: %v", err)
			}
			info := streamreason.Info(err)
			if got := info.GetMetadata()["missing"]; got != tc.slug {
				t.Errorf("metadata missing = %q, want %q", got, tc.slug)
			}
			if why := info.GetMetadata()[tc.slug]; why == "" {
				t.Errorf("refusal does not say WHY %s is unavailable: %v", tc.slug, info.GetMetadata())
			}
			// And it refused before doing anything to the camera at all.
			if fake.Opens() != 0 {
				t.Errorf("source was opened %d times for a request that could never be served", fake.Opens())
			}
			if stream.count() != 0 {
				t.Errorf("a refused subscriber received %d frames", stream.count())
			}
		})
	}
}

func TestCalibratedFrames_RefusalNamesEveryMissingRequirement(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48) // provides nothing
	svc := newCalibratedService(fake)

	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{
		Source:  "fake:1",
		Require: []agentpbv2.FrameRequirement{captureSettings, alignedDepth, measuredIntrinsics},
	}, newStreamStub(context.Background()))

	missing := streamreason.Info(err).GetMetadata()["missing"]
	// Canonical order, whatever order the client asked in: a refusal that reads
	// differently depending on flag order is a refusal nobody can grep for.
	want := strings.Join([]string{
		framesource.SlugAlignedDepth, framesource.SlugMeasuredIntrinsics, framesource.SlugCaptureSettings,
	}, ",")
	if missing != want {
		t.Errorf("missing = %q, want %q", missing, want)
	}
}

func TestCalibratedFrames_ASourceThatProvidesWhatWasRequiredStreams(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newStreamStub(ctx)
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source:  "fake:1",
		Require: []agentpbv2.FrameRequirement{alignedDepth, measuredIntrinsics, captureSettings},
	}, stream)

	fake.Push(framesourcetest.Frame(1, 64, 48))
	waitFor(t, "the first frame", func() bool { return stream.count() == 1 })

	got := stream.frames()[0]
	// Rule 4: both planes are one capture instant. Not a convention the
	// consumer maintains — a property of the message it receives.
	if got.GetFrameId() != 1 || got.GetCapturedAtNs() == 0 {
		t.Errorf("frame id/instant = %d/%d", got.GetFrameId(), got.GetCapturedAtNs())
	}
	// Rule 1: aligned depth is the colour plane's resolution, exactly.
	if got.GetDepth().GetWidth() != got.GetColourFormat().GetWidth() ||
		got.GetDepth().GetHeight() != got.GetColourFormat().GetHeight() {
		t.Errorf("aligned depth is %dx%d against a %dx%d colour plane",
			got.GetDepth().GetWidth(), got.GetDepth().GetHeight(),
			got.GetColourFormat().GetWidth(), got.GetColourFormat().GetHeight())
	}
	// Rule 2: depth carries the scale that makes it metres.
	if got.GetDepth().GetScaleM() <= 0 {
		t.Errorf("depth scale = %v; depth in unknown units is not depth", got.GetDepth().GetScaleM())
	}
	if got.GetSource() != "fake:1" {
		t.Errorf("frame source = %q", got.GetSource())
	}

	cancel()
	if err := awaitErr(t, errCh); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled stream ended with %v", err)
	}
}

// --- the rule that turns a silent degradation into a visible outage ---

func TestCalibratedFrames_LosingDepthMidStreamEndsTheStream(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Require: []agentpbv2.FrameRequirement{alignedDepth},
	}, stream)

	fake.Push(framesourcetest.Frame(1, 64, 48))
	waitFor(t, "the first good frame", func() bool { return stream.count() == 1 })
	// The depth sensor drops out. The source keeps producing colour.
	fake.Push(framesourcetest.ColourOnly(2, 64, 48))

	err := awaitErr(t, errCh)
	if status.Code(err) != codes.FailedPrecondition || !streamreason.Has(err, streamreason.RequirementUnmet) {
		t.Fatalf("stream ended with %v; want FailedPrecondition/REQUIREMENT_UNMET", err)
	}
	info := streamreason.Info(err)
	if got := info.GetMetadata()["missing"]; got != framesource.SlugAlignedDepth {
		t.Errorf("missing = %q", got)
	}
	// The wording distinguishes "stopped" from "never could": the operator's
	// next move is different.
	if !strings.Contains(status.Convert(err).Message(), "stopped") {
		t.Errorf("message does not say the source STOPPED providing depth: %q", status.Convert(err).Message())
	}
	if stream.count() != 1 {
		t.Errorf("subscriber received %d frames; the depth-less one must not have been delivered", stream.count())
	}
}

func TestCalibratedFrames_ASubscriberThatRequiredNothingKeepsItsColour(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newStreamStub(ctx)
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, stream)

	// One at a time: fan-out is latest-wins, so two frames pushed back to back
	// legitimately coalesce into one delivery.
	fake.Push(framesourcetest.Frame(1, 64, 48))
	waitFor(t, "the first frame", func() bool { return stream.count() == 1 })
	fake.Push(framesourcetest.ColourOnly(2, 64, 48))
	waitFor(t, "the depth-less second frame", func() bool { return stream.count() == 2 })
	if stream.frames()[1].GetDepth() != nil {
		t.Error("the second frame should have arrived without depth, as it was captured")
	}

	cancel()
	<-errCh
}

// --- the source's own rules, enforced at the boundary ---

func TestCalibratedFrames_DepthWithoutAScaleIsDroppedNotShipped(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Require: []agentpbv2.FrameRequirement{alignedDepth},
	}, stream)

	// A source whose depth scale went unreadable. Guessing 1 mm here is exactly
	// the silent degradation this feature exists to prevent, so the plane is
	// removed and the depth-requiring subscriber is told.
	bad := framesourcetest.Frame(1, 64, 48)
	bad.Depth.ScaleM = 0
	fake.Push(bad)

	err := awaitErr(t, errCh)
	if !streamreason.Has(err, streamreason.RequirementUnmet) {
		t.Fatalf("stream ended with %v; want REQUIREMENT_UNMET", err)
	}
	if stream.count() != 0 {
		t.Errorf("a plane in unknown units was delivered to %d frames", stream.count())
	}
}

func TestCalibratedFrames_DepthThatDoesNotMatchTheColourResolutionIsDropped(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Require: []agentpbv2.FrameRequirement{alignedDepth},
	}, stream)

	// Claims alignment at a different resolution — which would silently index
	// the wrong pixel for every lookup a consumer makes.
	bad := framesourcetest.Frame(1, 64, 48)
	bad.Depth.Width, bad.Depth.Height = 32, 24
	bad.Depth.BytesPerLine = 64
	bad.Depth.Data = make([]byte, 64*24)
	fake.Push(bad)

	if err := awaitErr(t, errCh); !streamreason.Has(err, streamreason.RequirementUnmet) {
		t.Fatalf("stream ended with %v; want REQUIREMENT_UNMET", err)
	}
}

func TestCalibratedFrames_SensorNativeDepthDoesNotSatisfyAlignedDepth(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Require: []agentpbv2.FrameRequirement{alignedDepth},
	}, stream)

	unaligned := framesourcetest.Frame(1, 64, 48)
	unaligned.Depth.Alignment = agentpbv2.DepthPlane_ALIGNMENT_SENSOR_NATIVE
	fake.Push(unaligned)

	if err := awaitErr(t, errCh); !streamreason.Has(err, streamreason.RequirementUnmet) {
		t.Fatalf("stream ended with %v; want REQUIREMENT_UNMET for sensor-native depth", err)
	}
}

// --- fan-out ---

func TestCalibratedFrames_SlowSubscriberNeitherStallsAnotherNorReceivesABacklog(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fast := newStreamStub(ctx)
	slow := newStreamStub(ctx)
	slow.gate = make(chan struct{})

	fastErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, fast)
	waitFor(t, "the capture to start", func() bool { return fake.Opens() == 1 })
	slowErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, slow)
	waitFor(t, "the second subscriber to join", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		hub := svc.hubs["fake:1"]
		if hub == nil {
			return false
		}
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.subs) == 2
	})

	// One capture, two audiences.
	if fake.Opens() != 1 {
		t.Fatalf("source opened %d times for two subscribers; the camera must be shared", fake.Opens())
	}

	// One at a time, so the fast subscriber demonstrably receives each — the
	// point being that it keeps receiving while the slow one is stuck, not that
	// fan-out is lossless (it deliberately is not).
	const frames = 5
	for i := 1; i <= frames; i++ {
		fake.Push(framesourcetest.Frame(uint64(i), 64, 48))
		want := i
		waitFor(t, "the fast subscriber to keep up", func() bool { return fast.count() == want })
	}
	if slow.count() != 0 {
		t.Fatalf("the slow subscriber is gated and should have delivered nothing yet")
	}

	// Release it twice: the first Send completes with frame 1, and the next
	// thing it is handed must be the NEWEST frame, not frame 2. A measurement's
	// value is that it describes now.
	slow.gate <- struct{}{}
	slow.gate <- struct{}{}
	waitFor(t, "the slow subscriber to catch up", func() bool { return slow.count() == 2 })

	got := slow.frames()
	if got[0].GetFrameId() != 1 {
		t.Errorf("slow subscriber's first frame = %d, want 1", got[0].GetFrameId())
	}
	if got[1].GetFrameId() != frames {
		t.Errorf("slow subscriber's second frame = %d, want %d — it worked through a backlog instead of jumping to the newest",
			got[1].GetFrameId(), frames)
	}

	cancel()
	close(slow.gate)
	<-fastErr
	<-slowErr
	waitFor(t, "the shared capture to be released", func() bool { return fake.Closes() == 1 })
}

func TestCalibratedFrames_ConflictingGeometryIsRefusedWithWhatIsRunning(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newStreamStub(ctx)
	firstErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Width: 64, Height: 48, Framerate: 30,
	}, first)
	waitFor(t, "the capture to start", func() bool { return fake.Opens() == 1 })

	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Width: 32, Height: 24, Framerate: 30,
	}, newStreamStub(context.Background()))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(err.Error(), "64x48@30") {
		t.Errorf("refusal does not name the geometry already running: %v", err)
	}
	if fake.Opens() != 1 {
		t.Errorf("the second request restarted the capture (%d opens)", fake.Opens())
	}

	cancel()
	<-firstErr
}

// --- a source that is present and cannot be opened ---

func TestCalibratedFrames_UnavailableSourceIsListedAndRefusedByName(t *testing.T) {
	const reason = "Intel(R) RealSense(TM) Depth Camera 435i is attached but the wendy-realsense-source capture helper is not installed"
	svc := newCalibratedService(framesource.NewUnavailableSource(
		"realsense", framesource.KindRealSense, "Intel(R) RealSense(TM) Depth Camera 435i", reason))

	// It is LISTED. Hiding it would tell an operator their camera does not
	// exist, which is the same silence the feature exists to remove.
	listed, err := svc.ListCalibratedSources(context.Background(), &agentpbv2.ListCalibratedSourcesRequest{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(listed.GetSources()) != 1 || listed.GetSources()[0].GetAvailable() {
		t.Fatalf("listing = %+v; want one unavailable source", listed.GetSources())
	}
	if listed.GetSources()[0].GetUnavailableReason() != reason {
		t.Errorf("listing does not carry the reason: %q", listed.GetSources()[0].GetUnavailableReason())
	}

	streamErr := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{Source: "realsense"},
		newStreamStub(context.Background()))
	if status.Code(streamErr) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(streamErr))
	}
	if !streamreason.Has(streamErr, streamreason.CalibratedSourceUnavailable) {
		t.Fatalf("error does not carry CALIBRATED_SOURCE_UNAVAILABLE: %v", streamErr)
	}
	md := streamreason.Info(streamErr).GetMetadata()
	if md["helper"] != framesource.HelperName {
		t.Errorf("refusal does not name the helper to install: %v", md)
	}
	if !strings.Contains(streamErr.Error(), "not installed") {
		t.Errorf("refusal does not carry the reason: %v", streamErr)
	}
}

func TestCalibratedFrames_UnknownSourceNamesWhatTheDeviceHas(t *testing.T) {
	svc := newCalibratedService(framesourcetest.NewFake("fake:1", 64, 48, alignedDepth))

	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{Source: "fake:9"},
		newStreamStub(context.Background()))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
	if !strings.Contains(err.Error(), "fake:1") {
		t.Errorf("refusal does not name the sources this device has: %v", err)
	}
}

func TestCalibratedFrames_NoSourceNamedAndSeveralCouldServeIsRefusedNotGuessed(t *testing.T) {
	svc := newCalibratedService(
		framesourcetest.NewFake("fake:1", 64, 48, alignedDepth),
		framesourcetest.NewFake("fake:2", 64, 48, alignedDepth),
	)
	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{
		Require: []agentpbv2.FrameRequirement{alignedDepth},
	}, newStreamStub(context.Background()))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument; picking a camera at random is a coin toss over which one measures the world", status.Code(err))
	}
}

// --- message size ---

func TestCalibratedFrames_FrameLargerThanTheClientLimitIsRefusedWithBothNumbers(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 1920, 1080, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	// The default gRPC receive limit: a 1080p BGR colour plane alone is over it.
	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"},
		newStreamStub(context.Background()))
	if !streamreason.Has(err, streamreason.FrameTooLarge) {
		t.Fatalf("error does not carry FRAME_TOO_LARGE: %v", err)
	}
	md := streamreason.Info(err).GetMetadata()
	if md["frame_bytes"] == "" || md["limit_bytes"] == "" {
		t.Errorf("refusal does not carry both numbers: %v", md)
	}
	if fake.Opens() != 0 {
		t.Errorf("the camera was opened for a stream that could never be delivered")
	}

	// A client that raised its limit is served.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newStreamStub(ctx)
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", MaxFrameBytes: 16 << 20,
	}, stream)
	waitFor(t, "the capture to start", func() bool { return fake.Opens() == 1 })
	cancel()
	<-errCh
}

func TestCalibratedFrames_RequestedGeometryScalesTheSizeRefusal(t *testing.T) {
	// Fits at its 640x480 default, does not at 1920x1080.
	fake := framesourcetest.NewFake("fake:1", 640, 480, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newStreamStub(ctx)
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, stream)
	waitFor(t, "the default request to be accepted", func() bool { return fake.Opens() == 1 })
	cancel()
	<-errCh

	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Width: 1920, Height: 1080,
	}, newStreamStub(context.Background()))
	if !streamreason.Has(err, streamreason.FrameTooLarge) {
		t.Fatalf("a request for nine times the pixels was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "1920x1080") {
		t.Errorf("refusal does not name the geometry it is about: %v", err)
	}
}

// --- capture lifecycle ---

func TestCalibratedFrames_CaptureEndingClosesEverySubscriber(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, stream)
	waitFor(t, "the capture to start", func() bool { return fake.Opens() == 1 })

	fake.End(errors.New("camera detached"))

	err := awaitErr(t, errCh)
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "camera detached") {
		t.Fatalf("stream ended with %v; want Unavailable carrying the source's own reason", err)
	}
}

func TestCalibratedFrames_SourceThatWillNotOpenIsReportedNotSilentlyEmpty(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth)
	fake.OpenErr = framesource.ErrHelperNotInstalled
	svc := newCalibratedService(fake)

	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"},
		newStreamStub(context.Background()))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !streamreason.Has(err, streamreason.CalibratedSourceUnavailable) {
		t.Errorf("a helper that cannot start must say so by reason: %v", err)
	}
}

// --- the hub's lifecycle: nobody is left waiting, and nothing joins a corpse ---

// SIGTERM cancels the agent context and GracefulStop waits for open streams.
// A capture whose subscriber never disconnects must be ended by the service,
// not by the client, or the agent -- and the helper holding the camera -- hangs.
func TestCalibratedFrames_ShutdownEndsEveryCaptureAndReleasesTheCamera(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	// A subscriber whose context never ends: the client is still connected.
	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, stream)
	waitFor(t, "the capture to start", func() bool { return fake.Opens() == 1 })

	done := make(chan struct{})
	go func() { svc.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return while a subscriber was still connected")
	}
	if err := awaitErr(t, errCh); err != nil {
		t.Errorf("a stream ended by shutdown returned %v; want a clean end", err)
	}
	if fake.Closes() != 1 {
		t.Errorf("the capture was closed %d times; the camera must be released on shutdown", fake.Closes())
	}
	// Nothing new starts on a service that has shut down.
	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"},
		newStreamStub(context.Background()))
	if status.Code(err) != codes.Unavailable {
		t.Errorf("a subscribe after shutdown returned %v; want Unavailable", err)
	}
}

// publish returning false is the producer's cue to return -- but the hub
// stays in the map until dropHub runs. A subscriber that joins in that window
// must find the hub already marked finished, under the same lock, or it is
// handed a doorbell nobody will ever ring.
func TestFrameHub_LosingTheLastSubscriberMarksTheHubFinishedUnderTheLock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &frameHub{
		subs: map[int]*frameSub{}, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		source:  "fake:1",
		listing: framesourcetest.NewFake("fake:1", 64, 48, alignedDepth).Desc(),
	}
	if _, _, err := hub.join(framesource.Options{}, []agentpbv2.FrameRequirement{alignedDepth}, 0); err != nil {
		t.Fatalf("join: %v", err)
	}
	// The only subscriber required depth; a depth-less frame ends it, and with
	// it the capture.
	if hub.publish(framesourcetest.ColourOnly(1, 64, 48)) {
		t.Fatal("publish reported a live subscriber after closing the only one")
	}
	if _, _, err := hub.join(framesource.Options{}, nil, 0); !errors.Is(err, errHubEnding) {
		t.Errorf("a join after the last subscriber was closed returned %v; want errHubEnding", err)
	}
}

// A client that closes its stream and immediately reopens must get a capture,
// not a clean EOF with no frames from the hub its old stream was tearing down.
func TestCalibratedFrames_ReconnectingImmediatelyGetsAFreshCaptureNotAnEmptyStream(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := newStreamStub(firstCtx)
	firstErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, first)
	waitFor(t, "the first capture to start", func() bool { return fake.Opens() == 1 })
	cancelFirst()
	<-firstErr
	// The old hub's producer may still be tearing down here; the subscriber
	// that arrives now must not attach to it.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := newStreamStub(ctx)
	secondErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, second)
	waitFor(t, "a fresh capture to start", func() bool { return fake.Opens() == 2 })
	fake.Push(framesourcetest.Frame(7, 64, 48))
	waitFor(t, "the reconnected subscriber to receive a frame", func() bool { return second.count() == 1 })

	cancel()
	<-secondErr
}

// --- the capture's own descriptor, not the listing's promise ---

// The first subscriber is admitted on the listing. If the helper then opens a
// mode without depth, that subscriber must be refused up front -- "cannot
// provide" -- not handed a depth-less first frame and told the source
// "stopped providing". The operator's next move is different for each.
func TestCalibratedFrames_FirstSubscriberIsReCheckedAgainstWhatTheCaptureOpened(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	fake.Negotiated = &agentpbv2.CalibratedSource{
		Source: "fake:1", Kind: "fake", Available: true,
		ColourWidth: 64, ColourHeight: 48, ColourFourcc: "BGR3",
		Provides: []agentpbv2.FrameRequirement{measuredIntrinsics, captureSettings}, // no depth after all
	}
	svc := newCalibratedService(fake)

	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Require: []agentpbv2.FrameRequirement{alignedDepth},
	}, stream)

	err := awaitErr(t, errCh)
	if !streamreason.Has(err, streamreason.RequirementUnmet) {
		t.Fatalf("stream ended with %v; want REQUIREMENT_UNMET", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "cannot provide") || strings.Contains(msg, "stopped") {
		t.Errorf("refusal reads %q; this capture never could, it did not stop", msg)
	}
	if stream.count() != 0 {
		t.Errorf("subscriber received %d frames before being refused", stream.count())
	}
}

// A capture started with defaults has zero opts; a joiner naming exactly the
// size the helper reported must be admitted, and one naming another size must
// be told the real numbers rather than "the source's default geometry".
func TestCalibratedFrames_JoinerIsComparedAgainstTheNegotiatedGeometry(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	fake.Negotiated = &agentpbv2.CalibratedSource{
		Source: "fake:1", Kind: "fake", Available: true,
		ColourWidth: 64, ColourHeight: 48, ColourFourcc: "BGR3",
		DepthWidth: 64, DepthHeight: 48,
		Provides:      []agentpbv2.FrameRequirement{alignedDepth, measuredIntrinsics, captureSettings},
		MaxFrameBytes: framesource.FrameBytes(64*3, 48, 64*2, 48),
	}
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newStreamStub(ctx)
	firstErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, first)
	waitFor(t, "the capture to report its geometry", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		hub := svc.hubs["fake:1"]
		return hub != nil && hub.negotiatedDesc() != nil
	})

	joiner := newStreamStub(ctx)
	joinerErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Width: 64, Height: 48,
	}, joiner)
	waitForSubscribers(t, svc, "fake:1", 2)
	fake.Push(framesourcetest.Frame(1, 64, 48))
	waitFor(t, "the joiner naming the running size to receive a frame", func() bool { return joiner.count() == 1 })
	if fake.Opens() != 1 {
		t.Errorf("the joiner restarted the capture (%d opens)", fake.Opens())
	}

	err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Width: 32, Height: 24,
	}, newStreamStub(context.Background()))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(err.Error(), "64x48") {
		t.Errorf("refusal does not name the geometry actually running: %v", err)
	}
	// A framerate cannot be confirmed from the descriptor, so a joiner naming
	// one is told to leave it out rather than silently trusted.
	err = svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{
		Source: "fake:1", Width: 64, Height: 48, Framerate: 30,
	}, newStreamStub(context.Background()))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "framerate") {
		t.Errorf("a joiner naming a framerate got %v; want a refusal saying to omit it", err)
	}

	cancel()
	<-firstErr
	<-joinerErr
}

// A default-geometry request joins whatever is running, so it must be sized
// against THAT, not against the listing's default: a subscriber admitted at
// the 640x480 estimate and then sent 1080p frames dies on its first Recv,
// which is the exact failure the size gate exists to prevent.
func TestCalibratedFrames_JoinerIsSizedAgainstTheRunningCaptureNotTheListing(t *testing.T) {
	t.Run("from the requested geometry", func(t *testing.T) {
		fake := framesourcetest.NewFake("fake:1", 640, 480, alignedDepth, measuredIntrinsics, captureSettings)
		svc := newCalibratedService(fake)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first := newStreamStub(ctx)
		firstErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{
			Source: "fake:1", Width: 1920, Height: 1080, MaxFrameBytes: 16 << 20,
		}, first)
		waitFor(t, "the 1080p capture to start", func() bool { return fake.Opens() == 1 })

		err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"},
			newStreamStub(context.Background()))
		if !streamreason.Has(err, streamreason.FrameTooLarge) {
			t.Fatalf("a default-limit joiner of a 1080p capture was admitted: %v", err)
		}
		if !strings.Contains(err.Error(), "1920x1080") {
			t.Errorf("refusal does not name the running geometry: %v", err)
		}
		cancel()
		<-firstErr
	})
	t.Run("from the negotiated descriptor", func(t *testing.T) {
		fake := framesourcetest.NewFake("fake:1", 640, 480, alignedDepth, measuredIntrinsics, captureSettings)
		fake.Negotiated = &agentpbv2.CalibratedSource{
			Source: "fake:1", Kind: "fake", Available: true,
			ColourWidth: 1920, ColourHeight: 1080, ColourFourcc: "BGR3",
			DepthWidth: 1920, DepthHeight: 1080,
			Provides:      []agentpbv2.FrameRequirement{alignedDepth, measuredIntrinsics, captureSettings},
			MaxFrameBytes: framesource.FrameBytes(1920*3, 1080, 1920*2, 1080),
		}
		svc := newCalibratedService(fake)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first := newStreamStub(ctx)
		firstErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1", MaxFrameBytes: 16 << 20}, first)
		waitFor(t, "the capture to report its geometry", func() bool {
			svc.mu.Lock()
			defer svc.mu.Unlock()
			hub := svc.hubs["fake:1"]
			return hub != nil && hub.negotiatedDesc() != nil
		})

		err := svc.StreamCalibratedFrames(&agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"},
			newStreamStub(context.Background()))
		if !streamreason.Has(err, streamreason.FrameTooLarge) {
			t.Fatalf("a default-limit joiner of a capture the helper opened at 1080p was admitted: %v", err)
		}
		cancel()
		<-firstErr
	})
}

// Enumerating forks the capture helper, and forking a second helper against a
// camera the first is streaming from is the disturbance the Provider contract
// forbids. A joiner naming a running source has no reason to enumerate at all.
func TestCalibratedFrames_JoiningARunningSourceDoesNotEnumerateAgain(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	var (
		mu           sync.Mutex
		enumerations int
	)
	reg := framesource.NewRegistry(framesource.ProviderFunc(func(context.Context) ([]framesource.Source, error) {
		mu.Lock()
		enumerations++
		mu.Unlock()
		return []framesource.Source{fake}, nil
	}))
	svc := NewCalibratedFrameService(context.Background(), zap.NewNop(), reg)
	countEnumerations := func() int {
		mu.Lock()
		defer mu.Unlock()
		return enumerations
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newStreamStub(ctx)
	firstErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, first)
	waitFor(t, "the capture to start", func() bool { return fake.Opens() == 1 })
	before := countEnumerations()

	joiner := newStreamStub(ctx)
	joinerErr := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, joiner)
	waitForSubscribers(t, svc, "fake:1", 2)
	fake.Push(framesourcetest.Frame(1, 64, 48))
	waitFor(t, "the joiner to receive a frame", func() bool { return joiner.count() == 1 })
	if got := countEnumerations(); got != before {
		t.Errorf("joining a running source enumerated %d more time(s); the helper must not be forked against a camera it is streaming from", got-before)
	}

	cancel()
	<-firstErr
	<-joinerErr
}

// A colour plane that does not carry the bytes its geometry claims is not a
// frame. It is not skipped -- a subscriber waiting on frames that never come
// is the silent failure this exists to remove -- the capture is ended, loudly.
func TestCalibratedFrames_AColourPlaneThatDoesNotMatchItsGeometryEndsTheCapture(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth, measuredIntrinsics, captureSettings)
	svc := newCalibratedService(fake)

	stream := newStreamStub(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, stream)

	bad := framesourcetest.Frame(1, 64, 48)
	bad.Colour = bad.Colour[:len(bad.Colour)/2]
	fake.Push(bad)

	err := awaitErr(t, errCh)
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("stream ended with %v; want Unavailable naming the colour plane", err)
	}
	if stream.count() != 0 {
		t.Errorf("a colour plane a consumer would read past the end of was delivered (%d frames)", stream.count())
	}
	waitFor(t, "the capture to be released", func() bool { return fake.Closes() == 1 })
}
