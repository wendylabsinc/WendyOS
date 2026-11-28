package services

// Tests for a camera node made at the size its app's entitlement states: the
// camera is captured at that size when it lists it, every node of the camera
// has that one size, and a size that cannot be had leaves the node at the size
// a capture chooses, with the reason.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/camerashare"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var (
	szFull  = appconfig.CameraSize{Width: 1920, Height: 1080}
	szSmall = appconfig.CameraSize{Width: 640, Height: 480}

	szFullModes = map[string]sharedMode{
		appconfig.CameraFormatPixels: {1920, 1080, "YUYV", 3840, 30, "pixels", 0},
		appconfig.CameraFormatNative: {Width: 1920, Height: 1080, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes},
		appconfig.CameraFormatH264:   {Width: 1920, Height: 1080, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatH264, MaxFrameBytes: maxFrameBytes},
	}
)

const (
	szNotWholeAt = "the camera's own frames are not captured whole at %s: " + sfNoOwnFrames
	szSharedWith = "the camera is shared with %s at %s, and a camera is captured at one size"
)

func szCameraEntitlement(format, size string, allowlist ...string) appconfig.Entitlement {
	return appconfig.Entitlement{Type: appconfig.EntitlementCamera, Format: format, Size: size, Allowlist: allowlist}
}

func szJPEGWebcam(t *testing.T) *VideoService {
	t.Helper()
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{5, 15, 30})
	sfCanDeriveFrames(s)
	return s
}

func TestShareDecision_AStatedSizeTheCameraListsIsTheNodesSizeInEveryFormat(t *testing.T) {
	s := szJPEGWebcam(t)
	for _, format := range appconfig.CameraFormats {
		d := s.shareDecisionStating(usbCam(), format, szFull, nil)
		want := shareDecision{Shared: true, Mode: szFullModes[format], Sized: true}
		want.SoftwareEncoder, want.PresetIgnored = d.SoftwareEncoder, d.PresetIgnored
		if d != want {
			t.Errorf("%s: decision = %+v\nwant       %+v", format, d, want)
		}
		chosen := s.shareDecisionFor(context.Background(), usbCam(), format)
		if at := camerashare.ModeAt(chosen.Mode.String(), camerashare.SizeAtRate{Width: 1920, Height: 1080, FPS: 30}); at != d.Mode.String() {
			t.Errorf("%s: the command line reads the node's mode as %q, and the node declares %q", format, at, d.Mode)
		}
		if chosen.Sized || chosen.Mode.size() == szFull {
			t.Errorf("%s: without a stated size the decision is %+v", format, chosen)
		}
	}
}

func TestShareDecision_AStatedSizeThatACaptureChoosesAnywayChangesNothing(t *testing.T) {
	s := szJPEGWebcam(t)
	for _, format := range appconfig.CameraFormats {
		chosen := s.shareDecisionFor(context.Background(), usbCam(), format)
		if d := s.shareDecisionStating(usbCam(), format, appconfig.CameraSize{Width: 1280, Height: 720}, nil); d != chosen {
			t.Errorf("%s: decision = %+v\nwant the one without a size, %+v", format, d, chosen)
		}
		if d := s.shareDecisionStating(usbCam(), format, noSize, nil); d != chosen {
			t.Errorf("%s: no size stated: decision = %+v\nwant %+v", format, d, chosen)
		}
	}
}

func TestShareDecision_AStatedSizeTheCameraIsNotCapturedAtLeavesTheChosenSizeAndSaysWhy(t *testing.T) {
	larger := brioSizes()
	larger[v4l2PixFmtMJPEG] = append(larger[v4l2PixFmtMJPEG], [2]uint32{2560, 1440})
	cases := map[string]struct {
		service func(t *testing.T) *VideoService
		stated  appconfig.CameraSize
		whyNot  string
	}{
		"a size the camera does not list": {
			szJPEGWebcam,
			appconfig.CameraSize{Width: 1600, Height: 900},
			"the camera's own frames are not captured whole at 1600x900: " + sfNoOwnFrames + "the camera offers no JPEG at 1600x900",
		},
		"a size above what a JPEG capture hands on whole": {
			func(t *testing.T) *VideoService {
				s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, larger, []uint32{30})
				sfCanDeriveFrames(s)
				return s
			},
			appconfig.CameraSize{Width: 2560, Height: 1440},
			"the camera's own frames are not captured whole at 2560x1440: " + sfNoOwnFrames + "2560x1440 is above the 1920x1080 limit for capturing JPEG frames whole",
		},
		"a camera whose own frames are not captured whole": {
			func(t *testing.T) *VideoService {
				return shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
			},
			szFull,
			"the camera's own frames are not captured whole at 1920x1080: " + sfNoOwnFrames +
				"pictures and H.264 cannot be derived from its frames on this device: GStreamer lacks gdpdepay, gdppay, h264parse",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := tc.service(t)
			chosen := s.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatPixels)
			if !chosen.Shared || chosen.Mode != sfPixelsMode {
				t.Fatalf("without a size: %+v", chosen)
			}
			want := chosen
			want.AskedSize, want.AskedSizeWhyNot = tc.stated, tc.whyNot
			if d := s.shareDecisionStating(usbCam(), appconfig.CameraFormatPixels, tc.stated, nil); d != want {
				t.Errorf("decision = %+v\nwant       %+v", d, want)
			}
		})
	}
}

func TestShareDecision_ACameraThatProducesH264IsCapturedAtAStatedSizeItLists(t *testing.T) {
	sizes := map[uint32][][2]uint32{
		v4l2PixFmtYUYV: {{640, 480}},
		v4l2PixFmtH264: {{640, 480}, {1280, 720}, {1920, 1080}},
	}
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtH264}, sizes, []uint32{30})
	hcCanDecode(s, "avdec_h264")
	chosenSize := captureSizeFor
	captureSizeFor = func(path string, pixfmt, width, height uint32) (uint32, uint32) {
		if width != 0 && height != 0 {
			return width, height
		}
		return chosenSize(path, pixfmt, width, height)
	}

	native := s.shareDecisionStating(usbCam(), appconfig.CameraFormatNative, szFull, nil)
	want := szFullModes[appconfig.CameraFormatH264]
	want.Format = appconfig.CameraFormatNative
	if !native.Shared || !native.Sized || native.Mode != want || native.AskedSize != noSize {
		t.Errorf("a size the camera lists in H.264: %+v, want its own frames at %s", native, want)
	}

	unlisted := appconfig.CameraSize{Width: 1600, Height: 900}
	d := s.shareDecisionStating(usbCam(), appconfig.CameraFormatNative, unlisted, nil)
	if !d.Shared || d.Sized || d.Mode.size() != (appconfig.CameraSize{Width: 1280, Height: 720}) ||
		d.AskedSize != unlisted || d.AskedSizeWhyNot != "the camera lists no H.264 at 1600x900" {
		t.Errorf("a size the camera does not list: %+v", d)
	}
}

func TestShareDecision_ACameraOtherAppsShareIsDecidedAtTheirSize(t *testing.T) {
	s := szJPEGWebcam(t)
	atFull := &sharedCapture{mode: szFullModes[appconfig.CameraFormatNative], sized: true, apps: []string{"cv-a", "cv-b"}}
	atChosen := &sharedCapture{mode: sfH264Mode, apps: []string{"cv-a"}}

	d := s.shareDecisionStating(usbCam(), appconfig.CameraFormatPixels, szSmall, atFull)
	want := shareDecision{Shared: true, Mode: szFullModes[appconfig.CameraFormatPixels], Sized: true, AskedSize: szSmall,
		AskedSizeWhyNot: "the camera is shared with cv-a, cv-b at 1920x1080, and a camera is captured at one size"}
	if d != want {
		t.Errorf("another size than the camera is shared at: %+v\nwant %+v", d, want)
	}
	for name, stated := range map[string]appconfig.CameraSize{"the same size": szFull, "no size": noSize} {
		want.AskedSize, want.AskedSizeWhyNot = noSize, ""
		if d := s.shareDecisionStating(usbCam(), appconfig.CameraFormatPixels, stated, atFull); d != want {
			t.Errorf("%s: %+v\nwant %+v", name, d, want)
		}
	}

	d = s.shareDecisionStating(usbCam(), appconfig.CameraFormatPixels, szFull, atChosen)
	want = shareDecision{Shared: true, Mode: sfPixelsMode, AskedSize: szFull,
		AskedSizeWhyNot: "the camera is shared with cv-a at 1280x720, and a camera is captured at one size"}
	if d != want {
		t.Errorf("a size stated for a camera shared at the size its capture chose: %+v\nwant %+v", d, want)
	}
}

func TestCreateCameraShares_ANodeIsMadeAtTheSizeItsEntitlementStates(t *testing.T) {
	for _, format := range appconfig.CameraFormats {
		t.Run(format, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			sfCanDeriveFrames(h.s)
			fp := installFakeProducers(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{szCameraEntitlement(format, "1920x1080")})

			want := szFullModes[format]
			records := h.records("cv-a")
			if len(records) != 1 {
				t.Fatalf("records = %d; logs: %v", len(records), h.logs.All())
			}
			r := records[0]
			if r.mode != want || !r.sized || r.askedSize != noSize || r.askedSizeWhyNot != "" {
				t.Fatalf("record holds %s (sized %v, asked %q), want %s at its stated size", r.mode, r.sized, r.askedSize, want)
			}
			if opened := r.io.(*csNode).mode; opened != want {
				t.Errorf("the node declares %+v, want %+v", opened, want)
			}
			if n := h.logged("camera share: cv-a shares Brio 101 (/dev/video0) through /dev/video255 at " + want.String() + ", format " + format + ", stamp time"); n != 1 {
				t.Errorf("shares line logged %d times; logs: %v", n, h.logs.All())
			}
			if n := h.logged("asked for size"); n != 0 {
				t.Errorf("a size that is served was warned of; logs: %v", h.logs.All())
			}

			r.io.(*csNode).send(t, 1)
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
				t.Fatalf("the camera did not start; logs: %v", h.logs.All())
			}
			if req := fp.request(0); req.GetWidth() != 1920 || req.GetHeight() != 1080 || req.GetFramerate() != 0 {
				t.Errorf("the camera was started with %dx%d@%d, want 1920x1080 and no rate", req.GetWidth(), req.GetHeight(), req.GetFramerate())
			}
			if format == appconfig.CameraFormatPixels {
				fp.hub(0).broadcast(sfDerivedPictureOf(1920, 1080, 7))
				if e := r.io.(*csNode).next(t); e.kind != "frame" || e.size != 1920*1080*2 {
					t.Errorf("a 1920x1080 picture reached the node as %+v", e)
				}
			}

			row := h.reportOf("/dev/video0")
			if got, want := row["share_state"], "cv-a [format "+format+", stamp time, size 1920x1080] (reading"; !strings.HasPrefix(got, want) {
				t.Errorf("share_state = %q, want it to begin %q", got, want)
			}
			if row["shared_size"] != "1920x1080@30" || row["share_mode"] != "1280x720 YUYV, 30 fps" {
				t.Errorf("row = %v, want shared_size 1920x1080@30 beside the mode a node without a size gets", row)
			}

			share := h.stateShares("cv-a")[0]
			mode := share["mode"].(map[string]any)
			if share["sized"] != true || mode["width"] != float64(1920) || mode["height"] != float64(1080) {
				t.Errorf("state file share = %v, want it sized at 1920x1080", share)
			}
			if _, asked := share["askedSize"]; asked {
				t.Errorf("state file share holds an asked size for a size that is served: %v", share)
			}
			if v := h.stateFile()["version"]; v != float64(3) {
				t.Errorf("state file version = %v, want 3", v)
			}
		})
	}
}

// sfDerivedPictureOf is sfDerivedPicture at another size.
func sfDerivedPictureOf(width, height uint32, sampleID uint64) *videoFrame {
	f := spFrame(width, height, width*2, uint64(time.Now().UnixNano()))
	f.codec, f.sampleID, f.nativeClock = agentpb.VideoCodec_VIDEO_CODEC_RAW, sampleID, v4l2MonotonicClock
	return f
}

func TestCreateCameraShares_ASizeTheCameraIsNotCapturedAtGetsTheChosenSizeNodeThatSaysSo(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{szCameraEntitlement("", "1600x900")})

	const whyNot = "the camera's own frames are not captured whole at 1600x900: " + sfNoOwnFrames + "the camera offers no JPEG at 1600x900"
	const told = "asked for size 1600x900; serving 1280x720 because " + whyNot
	records := h.records("cv-a")
	if len(records) != 1 {
		t.Fatalf("the app got no node for a size that cannot be had; logs: %v", h.logs.All())
	}
	r := records[0]
	if r.mode != sfPixelsMode || r.sized || r.askedSize != (appconfig.CameraSize{Width: 1600, Height: 900}) || r.askedSizeWhyNot != whyNot {
		t.Fatalf("record holds %s (sized %v, asked %q because %q)", r.mode, r.sized, r.askedSize, r.askedSizeWhyNot)
	}
	if n := h.logged("camera share: cv-a: Brio 101 (/dev/video0): " + told); n != 1 {
		t.Errorf("the size not served was warned of %d times; logs: %v", n, h.logs.All())
	}
	if got, want := h.reportOf("/dev/video0")["share_state"], "cv-a [format pixels, stamp time] (idle; "+told+")"; got != want {
		t.Errorf("share_state = %q\nwant          %q", got, want)
	}
	r.io.(*csNode).send(t, 1)
	if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
		t.Fatal("the camera did not start")
	}
	if req := fp.request(0); req.GetWidth() != 0 || req.GetHeight() != 0 {
		t.Errorf("the camera was started with %dx%d, want no size", req.GetWidth(), req.GetHeight())
	}
	share := h.stateShares("cv-a")[0]
	if share["askedSize"] != "1600x900" || share["askedSizeWhyNot"] != whyNot || share["sized"] != nil {
		t.Errorf("state file share = %v", share)
	}
	if v := h.stateFile()["version"]; v != float64(1) {
		t.Errorf("state file version = %v, want 1: no share is at a stated size", v)
	}

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{szCameraEntitlement("", "1600x900")})
	if again := h.records("cv-a"); len(again) != 1 || again[0] != r || len(h.openedNodes()) != 1 {
		t.Errorf("the container created again got another node")
	}
}

func TestCreateCameraShares_ASecondAppGetsTheSizeTheCameraIsSharedAt(t *testing.T) {
	t.Run("the first app states a size", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		sfCanDeriveFrames(h.s)
		fp := installFakeProducers(h.s)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{szCameraEntitlement("native", "1920x1080")})
		h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{szCameraEntitlement("", "640x480")})
		h.s.CreateCameraShares(context.Background(), "cv-c", "sh.wendy.cv-c", nil)

		const told = "asked for size 640x480; serving 1920x1080 because the camera is shared with cv-a at 1920x1080, and a camera is captured at one size"
		b, c := h.records("cv-b"), h.records("cv-c")
		if len(b) != 1 || len(c) != 1 {
			t.Fatalf("records: cv-b %d, cv-c %d; logs: %v", len(b), len(c), h.logs.All())
		}
		pixels := szFullModes[appconfig.CameraFormatPixels]
		if b[0].mode != pixels || !b[0].sized || b[0].askedSize != szSmall {
			t.Errorf("cv-b holds %s (sized %v, asked %q), want the camera's %s", b[0].mode, b[0].sized, b[0].askedSize, pixels)
		}
		if c[0].mode != pixels || !c[0].sized || c[0].askedSize != noSize {
			t.Errorf("cv-c, which states no size, holds %s (sized %v, asked %q), want the camera's %s", c[0].mode, c[0].sized, c[0].askedSize, pixels)
		}
		if n := h.logged("camera share: cv-b: Brio 101 (/dev/video0): " + told); n != 1 {
			t.Errorf("cv-b's size was warned of %d times; logs: %v", n, h.logs.All())
		}
		want := "cv-a [format native, stamp time, size 1920x1080] (idle), " +
			"cv-b [format pixels, stamp time, size 1920x1080] (idle; " + told + "), " +
			"cv-c [format pixels, stamp time, size 1920x1080] (idle)"
		if got := h.reportOf("/dev/video0")["share_state"]; got != want {
			t.Errorf("share_state = %q\nwant          %q", got, want)
		}

		for _, r := range append(h.records("cv-a"), append(b, c...)...) {
			r.io.(*csNode).send(t, 1)
		}
		if !csWaitFor(2*time.Second, func() bool {
			hubs := fp.count()
			if hubs != 1 {
				return false
			}
			hub := fp.hub(0)
			hub.mu.Lock()
			defer hub.mu.Unlock()
			return len(hub.subs) == 3
		}) {
			t.Fatalf("the three nodes are not on one capture: %d started; logs: %v", fp.count(), h.logs.All())
		}
		if req := fp.request(0); req.GetWidth() != 1920 || req.GetHeight() != 1080 {
			t.Errorf("the camera was started with %dx%d, want 1920x1080", req.GetWidth(), req.GetHeight())
		}
	})

	t.Run("the first app states none", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		sfCanDeriveFrames(h.s)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
		h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{szCameraEntitlement("", "1920x1080")})

		const told = "asked for size 1920x1080; serving 1280x720 because the camera is shared with cv-a at 1280x720, and a camera is captured at one size"
		b := h.records("cv-b")
		if len(b) != 1 || b[0].mode != sfPixelsMode || b[0].sized || b[0].askedSize != szFull {
			t.Fatalf("cv-b's records = %+v, want one at the size cv-a shares the camera at; logs: %v", b, h.logs.All())
		}
		if got, want := h.reportOf("/dev/video0")["share_state"], "cv-a [format pixels, stamp time] (idle), cv-b [format pixels, stamp time] (idle; "+told+")"; got != want {
			t.Errorf("share_state = %q\nwant          %q", got, want)
		}

		// cv-a goes; cv-b keeps the node it was made with until it is deployed
		// again, and then gets the size it states.
		h.s.RemoveCameraShares("cv-a")
		h.drainRemovals()
		if kept := h.records("cv-b"); len(kept) != 1 || kept[0] != b[0] {
			t.Fatalf("cv-b's node changed when cv-a went")
		}
		h.s.RemoveCameraShares("cv-b")
		h.drainRemovals()
		h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{szCameraEntitlement("", "1920x1080")})
		if again := h.records("cv-b"); len(again) != 1 || again[0].mode != szFullModes[appconfig.CameraFormatPixels] || !again[0].sized || again[0].askedSize != noSize {
			t.Fatalf("cv-b deployed again alone holds %+v, want its stated size", again)
		}
	})
}

// Two creates that decide before either stores: the one that stores second
// finds the camera taken at another size, and decides again with it.
func TestCreateCameraShares_TwoCreatesAtOnceSettleOnOneSize(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	entered, release := h.blockOpenOf("/dev/video255")
	first := make(chan struct{})
	go func() {
		defer close(first)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{szCameraEntitlement("", "1920x1080")})
	}()
	<-entered
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{szCameraEntitlement("", "640x480")})
	close(release)
	<-first

	a, b := h.records("cv-a"), h.records("cv-b")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("records: cv-a %d, cv-b %d; logs: %v", len(a), len(b), h.logs.All())
	}
	small := sharedMode{640, 480, "YUYV", 1280, 30, "pixels", 0}
	if b[0].mode != small || !b[0].sized || b[0].askedSize != noSize {
		t.Errorf("cv-b, stored first, holds %s (asked %q), want its stated %s", b[0].mode, b[0].askedSize, small)
	}
	if a[0].mode != small || !a[0].sized || a[0].askedSize != szFull ||
		a[0].askedSizeWhyNot != "the camera is shared with cv-b at 640x480, and a camera is captured at one size" {
		t.Errorf("cv-a, stored second, holds %s (asked %q because %q), want the camera's %s", a[0].mode, a[0].askedSize, a[0].askedSizeWhyNot, small)
	}
	if n := h.logged(shareSizeTaken); n != 0 {
		t.Errorf("a share that was made on its second attempt was logged as not made; logs: %v", h.logs.All())
	}
	h.drainRemovals()
	if pending := h.pendingRemovals(); len(pending) != 0 {
		t.Errorf("the node of cv-a's first attempt was not removed: %v", pending)
	}
}

func TestCameraShareState_AStatedSizeSurvivesAnAgentRestartAndTheSameNodesComeBack(t *testing.T) {
	entitlements := map[string][]appconfig.Entitlement{
		"cv-a": {szCameraEntitlement("native", "1920x1080")},
		"cv-b": {szCameraEntitlement("", "640x480")},
	}
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements["cv-a"])
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", entitlements["cv-b"])
	type node struct {
		nr              int
		mode            sharedMode
		sized           bool
		askedSize       appconfig.CameraSize
		askedSizeWhyNot string
	}
	nodesOf := func(h *csHarness) map[string]node {
		nodes := map[string]node{}
		for _, id := range []string{"cv-a", "cv-b"} {
			for _, r := range h.records(id) {
				nodes[id] = node{r.node, r.mode, r.sized, r.askedSize, r.askedSizeWhyNot}
			}
		}
		return nodes
	}
	before := nodesOf(h)
	want := map[string]node{
		"cv-a": {255, szFullModes[appconfig.CameraFormatNative], true, noSize, ""},
		"cv-b": {254, szFullModes[appconfig.CameraFormatPixels], true, szSmall, "the camera is shared with cv-a at 1920x1080, and a camera is captured at one size"},
	}
	if len(before) != 2 || before["cv-a"] != want["cv-a"] || before["cv-b"] != want["cv-b"] {
		t.Fatalf("created %+v\nwant    %+v", before, want)
	}
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	h.s.Shutdown()

	// Whichever container the restarted agent hears of first, and whichever
	// node is read first, every node is the one its record holds.
	for _, order := range [][]string{{"cv-a", "cv-b"}, {"cv-b", "cv-a"}} {
		t.Run("first "+order[0], func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			sfCanDeriveFrames(h.s)
			fp := installFakeProducers(h.s)
			h.loop.aux[255], h.loop.aux[254] = "Brio 101", "Brio 101"
			h.writeState(string(state))
			h.s.loadCameraShares()
			if loaded := nodesOf(h); loaded["cv-a"] != before["cv-a"] || loaded["cv-b"] != before["cv-b"] {
				t.Fatalf("loaded %+v\nwant   %+v", loaded, before)
			}

			h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: order[0]}, {ID: order[1]}})
			for _, id := range order {
				h.s.CreateCameraShares(context.Background(), id, "sh.wendy."+id, entitlements[id])
			}
			if after := nodesOf(h); after["cv-a"] != before["cv-a"] || after["cv-b"] != before["cv-b"] {
				t.Fatalf("after adoption and a create of each container: %+v\nwant %+v", after, before)
			}
			if opened := h.openedNodes(); len(opened) != 2 {
				t.Fatalf("%d nodes were opened, want only the two adopted; logs: %v", len(opened), h.logs.All())
			}
			for _, id := range order {
				r := h.records(id)[0]
				if r.pump == nil || r.io.(*csNode).mode != before[id].mode {
					t.Fatalf("%s was not adopted at %s; logs: %v", id, before[id].mode, h.logs.All())
				}
			}
			if n := h.logged("redeploy the app"); n != 0 {
				t.Errorf("an adopted node at its stated size was told to redeploy; logs: %v", h.logs.All())
			}

			h.records(order[0])[0].io.(*csNode).send(t, 1)
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
				t.Fatal("the camera did not start")
			}
			if req := fp.request(0); req.GetWidth() != 1920 || req.GetHeight() != 1080 {
				t.Errorf("the camera was started with %dx%d, want 1920x1080", req.GetWidth(), req.GetHeight())
			}
			h.records(order[1])[0].io.(*csNode).send(t, 1)
			time.Sleep(50 * time.Millisecond)
			if fp.count() != 1 {
				t.Errorf("the second node's read started the camera again: %d captures", fp.count())
			}
			for _, id := range order {
				h.s.checkAdoptedMode(context.Background(), id, h.records(id)[0])
			}
			if note := h.reportOf("/dev/video0")["share_state"]; strings.Contains(note, "redeploy") {
				t.Errorf("share_state tells an app at its stored size to redeploy: %q", note)
			}
		})
	}
}

func TestCameraShareState_IsVersion3OnlyWhileSomeShareIsAtAStatedSize(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	native := []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")}
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", native)
	if v := h.stateFile()["version"]; v != float64(2) {
		t.Fatalf("state file version = %v, want 2 before any share states a size", v)
	}
	if _, sized := h.stateShares("cv-a")[0]["sized"]; sized {
		t.Errorf("a share at the size its capture chose is written as sized: %v", h.stateShares("cv-a")[0])
	}
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()

	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{szCameraEntitlement("", "1920x1080")})
	if v := h.stateFile()["version"]; v != float64(3) {
		t.Fatalf("state file version = %v, want 3 with a pixels share at a stated size", v)
	}
	h.s.RemoveCameraShares("cv-b")
	h.drainRemovals()
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", native)
	if v := h.stateFile()["version"]; v != float64(2) {
		t.Fatalf("state file version = %v, want 2 again once the sized share went", v)
	}
}

func TestCameraShareState_OlderVersionsLoadWithNoShareAtAStatedSize(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		t.Run("version "+version, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			h.writeState(`{"version":` + version + `,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
				strings.Replace(csStateRecord(csBrio(), "/dev/video0", sfPixelsMode, 254), `"node"`, `"format":"pixels","stamp":"time","node"`, 1) + `]}}}`)
			h.s.loadCameraShares()
			records := h.records("cv-a")
			if len(records) != 1 || records[0].mode != sfPixelsMode || records[0].sized || records[0].askedSize != noSize {
				t.Fatalf("records = %+v; logs: %v", records, h.logs.All())
			}
		})
	}
}

func TestSharedMode_ACameraSharedAtAStatedSizeRunsAtItWhoeverStartsIt(t *testing.T) {
	sized := func(t *testing.T) (*csHarness, *fakeProducers) {
		h := newCSHarness(t, csBrio())
		sfCanDeriveFrames(h.s)
		fp := installFakeProducers(h.s)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{szCameraEntitlement("", "1920x1080")})
		if len(h.records("cv-a")) != 1 {
			t.Fatalf("no share; logs: %v", h.logs.All())
		}
		return h, fp
	}
	const path = "/dev/video0"

	t.Run("a join without parameters starts it at the shared size", func(t *testing.T) {
		h, fp := sized(t)
		hub, id, _, err := h.s.joinHub(context.Background(), path, &agentpb.StreamVideoRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer hub.unsubscribe(id)
		if req := fp.request(0); req.GetWidth() != 1920 || req.GetHeight() != 1080 {
			t.Fatalf("started with %dx%d, want 1920x1080", req.GetWidth(), req.GetHeight())
		}
		if _, held := hub.heldExplicitly(); held {
			t.Error("a join without parameters holds the camera's parameters")
		}
		h.records("cv-a")[0].io.(*csNode).send(t, 1)
		if !csWaitFor(2*time.Second, func() bool {
			hub.mu.Lock()
			defer hub.mu.Unlock()
			return len(hub.subs) == 2
		}) {
			t.Fatalf("the node did not join the running capture: %d captures; logs: %v", fp.count(), h.logs.All())
		}
		if fp.count() != 1 {
			t.Errorf("the node's read restarted a capture already at the shared size: %d captures", fp.count())
		}
	})

	t.Run("a viewer is held to the shared size", func(t *testing.T) {
		h, _ := sized(t)
		if _, err := h.s.applySharedMode(path, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}); status.Code(err) != codes.InvalidArgument ||
			!strings.Contains(err.Error(), "runs only at its shared mode, 1920x1080 YUYV, 30 fps") {
			t.Errorf("1280x720 on a camera shared at 1920x1080: %v", err)
		}
		req, err := h.s.applySharedMode(path, &agentpb.StreamVideoRequest{Width: 1920, Height: 1080})
		if err != nil || streamRequestAssertsParams(req) {
			t.Errorf("1920x1080 on a camera shared at 1920x1080: %v, %v", req, err)
		}
	})

	t.Run("a recording that names the shared size joins and reports it", func(t *testing.T) {
		h, fp := sized(t)
		hub, id, _, w, hgt, fps, restarted, err := h.s.joinHubForCapture(context.Background(), path,
			&agentpb.StreamVideoRequest{Width: 1920, Height: 1080, Framerate: 30}, encodedFrames, noPreset)
		if err != nil {
			t.Fatal(err)
		}
		defer hub.unsubscribe(id)
		if w != 1920 || hgt != 1080 || fps != 30 || restarted {
			t.Errorf("reported %dx%d@%d (restarted %v), want the shared mode", w, hgt, fps, restarted)
		}
		if req := fp.request(0); req.GetWidth() != 1920 || req.GetHeight() != 1080 {
			t.Errorf("started with %dx%d, want 1920x1080", req.GetWidth(), req.GetHeight())
		}
		_, _, _, _, _, _, _, err = h.s.joinHubForCapture(context.Background(), path,
			&agentpb.StreamVideoRequest{Width: 1280, Height: 720}, encodedFrames, noPreset)
		if !isCameraCaptureRefusal(err) {
			t.Errorf("a recording at 1280x720 on a camera shared at 1920x1080: %v", err)
		}
	})

	t.Run("a recording that names nothing starts it at the shared size and reports no other size than it asked for", func(t *testing.T) {
		h, fp := sized(t)
		hub, id, _, w, hgt, fps, restarted, err := h.s.joinHubForCapture(context.Background(), path, &agentpb.StreamVideoRequest{}, encodedFrames, noPreset)
		if err != nil {
			t.Fatal(err)
		}
		defer hub.unsubscribe(id)
		if w != 0 || hgt != 0 || fps != 0 || restarted {
			t.Errorf("reported %dx%d@%d (restarted %v), want what it asked for: nothing", w, hgt, fps, restarted)
		}
		if req := fp.request(0); req.GetWidth() != 1920 || req.GetHeight() != 1080 {
			t.Errorf("started with %dx%d, want 1920x1080", req.GetWidth(), req.GetHeight())
		}
	})

	t.Run("a capture running at the size it chose is taken over", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		sfCanDeriveFrames(h.s)
		fp := installFakeProducers(h.s)
		viewer, id, _, err := h.s.joinHub(context.Background(), path, &agentpb.StreamVideoRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer viewer.unsubscribe(id)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{szCameraEntitlement("", "1920x1080")})
		h.records("cv-a")[0].io.(*csNode).send(t, 1)
		if !csWaitFor(2*time.Second, func() bool { return fp.count() == 2 }) {
			t.Fatalf("the camera was not started again: %d captures; logs: %v", fp.count(), h.logs.All())
		}
		if req := fp.request(1); req.GetWidth() != 1920 || req.GetHeight() != 1080 {
			t.Errorf("the takeover started with %dx%d, want 1920x1080", req.GetWidth(), req.GetHeight())
		}
		if !viewer.wasRestarted() {
			t.Error("the capture a join without parameters started was not marked restarted")
		}
	})

	t.Run("its node is asked for at the shared size", func(t *testing.T) {
		h, _ := sized(t)
		r := h.records("cv-a")[0]
		if why := h.s.nodeUnservedAt(path, r.mode, r.stamp); why != "" {
			t.Errorf("a node at its stated size is not served: %s", why)
		}
		own, declined, native := h.s.ownFramesAtDefaults(path)
		if !native || declined != "" || own.width != 1920 || own.height != 1080 {
			t.Errorf("a capture without parameters hands on %+v (%q), want 1920x1080", own, declined)
		}
	})
}

func TestCameraShareReport_ListsTheSizesAnEntitlementCanState(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	row := h.reportOf("/dev/video0")
	if got, want := row["share_sizes"], "640x480@30, 1280x720@30, 1920x1080@30"; got != want {
		t.Errorf("share_sizes = %q, want %q", got, want)
	}
	if _, held := row["shared_size"]; held || row["share_sizes_unavailable"] != "" {
		t.Errorf("row of a camera no app shares = %v", row)
	}
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
	if got := h.reportOf("/dev/video0")["shared_size"]; got != "1280x720@30" {
		t.Errorf("shared_size = %q, want the size its capture chose, 1280x720@30", got)
	}

	// A camera whose own frames are not captured whole has no size to state.
	h.s.Shutdown()
	h = newCSHarness(t, csBrio())
	row = h.reportOf("/dev/video0")
	if row["share_sizes"] != "" || row["share_sizes_unavailable"] != sfCannotDerive {
		t.Errorf("row = %v, want no sizes because %q", row, sfCannotDerive)
	}
}

// szShareForTest is shareModeForTest for a camera captured at mode's size
// because an entitlement states it.
func szShareForTest(svc *VideoService, path string, mode sharedMode, containers ...string) {
	shareModeForTest(svc, path, mode, containers...)
	svc.shares.mu.Lock()
	defer svc.shares.mu.Unlock()
	for _, id := range containers {
		svc.shares.containers[id].shares[0].sized = true
	}
}

// 1280x720 is the size an unsized share of this camera has, so a hub held at
// it would be joined as it is; a share at a stated size is not served by it.
func TestJoinHubForShare_ACameraHeldAtAnotherSizeThanTheStatedOne(t *testing.T) {
	const path = "/dev/video0"
	mode := szFullModes[appconfig.CameraFormatPixels]
	held := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1280, Height: 720}

	t.Run("a viewer is ended and the camera starts at the stated size", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		fp := installFakeProducers(svc)
		ctx := context.Background()
		oldHub, viewerID, viewerCh, err := svc.getOrCreateHub(ctx, path, held, hubHolderStreamClient)
		if err != nil {
			t.Fatal(err)
		}
		viewerDone := make(chan error, 1)
		go func() {
			defer oldHub.unsubscribe(viewerID)
			viewerDone <- svc.pumpFrames(newFakeVideoStream(ctx), oldHub, viewerID, viewerCh)
		}()

		szShareForTest(svc, path, mode, "cv-a")
		share, err := svc.joinHubForShare(ctx, path, 0, mode, noPreset)
		if err != nil {
			t.Fatalf("share join: %v", err)
		}
		defer share.Leave()
		select {
		case err := <-viewerDone:
			const want = "video stream ended: camera /dev/video0 is now shared with cv-a and runs only at its shared mode, " +
				"1920x1080 YUYV, 30 fps; reconnect without a size or rate"
			if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != want {
				t.Fatalf("viewer ended with %v\nwant InvalidArgument %q", err, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the viewer's stream was not ended")
		}
		if got := hubParams(currentHub(svc, path)); got != [3]uint32{1920, 1080, 0} {
			t.Fatalf("replacement hub at %v, want 1920x1080 and no rate", got)
		}
		if req := fp.request(1); fp.count() != 2 || req.GetWidth() != 1920 || req.GetHeight() != 1080 {
			t.Fatalf("producers = %d, the second started with %dx%d; want it at 1920x1080", fp.count(), req.GetWidth(), req.GetHeight())
		}
	})

	t.Run("a recording is waited for and then the camera starts at the stated size", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		fp := installFakeProducers(svc)
		ctx := context.Background()
		recHub, recID, recCh, _, _, _, _, err := svc.joinHubForCapture(ctx, path, held, encodedFrames, noPreset)
		if err != nil {
			t.Fatal(err)
		}
		szShareForTest(svc, path, mode, "cv-a")

		_, err = svc.joinHubForShare(ctx, path, 0, mode, noPreset)
		var wait *shareWaitError
		if !errors.As(err, &wait) {
			recHub.unsubscribe(recID)
			t.Fatalf("err = %v, want *shareWaitError", err)
		}
		if wait.Holder != "episode capture" || wait.Params != "1280x720 at default rate" {
			t.Fatalf("wait = %+v, want episode capture at 1280x720", wait)
		}
		if !channelStillOpen(recCh) || recHub.wasRestarted() || fp.count() != 1 {
			t.Fatal("the recording was interrupted")
		}
		recHub.unsubscribe(recID)
		select {
		case <-wait.Done:
		case <-time.After(2 * time.Second):
			t.Fatal("Done did not close once the recording left")
		}
		var share shareSubscription
		if !csWaitFor(2*time.Second, func() bool {
			share, err = svc.joinHubForShare(ctx, path, 0, mode, noPreset)
			return err == nil
		}) {
			t.Fatalf("share join after the recording left: %v", err)
		}
		defer share.Leave()
		if got := hubParams(currentHub(svc, path)); got != [3]uint32{1920, 1080, 0} {
			t.Fatalf("the camera runs at %v after the recording, want 1920x1080", got)
		}
	})
}
