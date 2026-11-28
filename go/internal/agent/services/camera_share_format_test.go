package services

// Tests for a camera node that follows its app's entitlement: the format
// chooses the node's declared mode and the hub subscription that fills it, and
// the stamp chooses what each buffer's timestamp field holds.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var (
	sfPixelsMode = sharedMode{1280, 720, "YUYV", 2560, 30, "pixels", 0}
	sfNativeMode = sharedMode{Width: 1280, Height: 720, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes}
	sfH264Mode   = sharedMode{Width: 1280, Height: 720, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatH264, MaxFrameBytes: maxFrameBytes}
)

// The test services list only jpegdec, as a device that cannot derive pictures
// and H.264 from a camera's own frames; sfCanDeriveFrames makes one that can.
func sfCanDeriveFrames(s *VideoService) {
	s.gstMemo.mu.Lock()
	s.gstMemo.available = map[string]bool{"gdpdepay": true, "gdppay": true, "jpegdec": true, "h264parse": true, "x264enc": true}
	s.gstMemo.mu.Unlock()
}

// What the test cameras' captures say of their own frames: none of them
// offers H.264, and the JPEG source says why it does not hand theirs on.
const (
	sfNoOwnFrames  = "H.264: the camera offers no H.264; JPEG: "
	sfNotWhole     = "the camera's own frames are not captured whole: " + sfNoOwnFrames
	sfCannotDerive = sfNotWhole + "pictures and H.264 cannot be derived from its frames on this device: GStreamer lacks gdpdepay, gdppay, h264parse"
)

// sfPinnedGap opens the failure text of an assertion that holds what the code
// does today where nobody decided it should: a change that fails one may be
// the fix, and then the assertion is what changes.
const sfPinnedGap = "pinned gap, not a rule (a fix for it changes this assertion): "

func sfCameraEntitlement(format, stamp string, allowlist ...string) appconfig.Entitlement {
	return appconfig.Entitlement{Type: appconfig.EntitlementCamera, Format: format, Stamp: stamp, Allowlist: allowlist}
}

// sfJPEGFrame is one of the camera's own frames as native capture hands it on.
func sfJPEGFrame(sampleID uint64, kernelNs int64) *videoFrame {
	return &videoFrame{
		data: []byte{0xFF, 0xD8, 0xFF, 0xE0, byte(sampleID), 0xFF, 0xD9}, codec: videoCodecMJPEG,
		native: true, auAligned: true, sampleID: sampleID,
		tsNs: uint64(time.Now().UnixNano()), nativeNs: kernelNs, nativeClock: v4l2MonotonicClock,
	}
}

var (
	sfH264ParameterSets = []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 1, 0x68, 0xCE}
	sfH264IDRSlice      = []byte{0, 0, 0, 1, 0x65, 0x88, 0x84}
	sfH264InterSlice    = []byte{0, 0, 0, 1, 0x41, 0x9A, 0x02}
)

// sfH264Frame is one whole H.264 access unit made from the camera frame with
// this sample ID: a random-access one carries its parameter sets and an IDR
// slice.
func sfH264Frame(sampleID uint64, randomAccess bool) *videoFrame {
	data := append([]byte(nil), sfH264InterSlice...)
	if randomAccess {
		data = append(append([]byte(nil), sfH264ParameterSets...), sfH264IDRSlice...)
	}
	return &videoFrame{data: data, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true, sampleID: sampleID,
		tsNs: uint64(time.Now().UnixNano())}
}

// sfDerivedPicture is a picture decoded from the camera frame with this sample
// ID: it has that frame's identity and kernel capture time.
func sfDerivedPicture(sampleID uint64, kernelNs int64) *videoFrame {
	f := spModeFrame(uint64(time.Now().UnixNano()))
	f.codec, f.sampleID, f.nativeNs, f.nativeClock = agentpb.VideoCodec_VIDEO_CODEC_RAW, sampleID, kernelNs, v4l2MonotonicClock
	return f
}

// sfTapPicture is a picture off a GStreamer pipeline's tap, as pumpRawTap
// broadcasts it: no sample ID and no kernel capture time.
func sfTapPicture(wallNs uint64) *videoFrame {
	f := spModeFrame(wallNs)
	f.codec = agentpb.VideoCodec_VIDEO_CODEC_RAW
	return f
}

func sfPumpAs(mode sharedMode, stamp string) func(*sharePump) {
	return func(p *sharePump) { p.mode, p.stamp = mode, stamp }
}

// sfRecheckEvery sets how often a pump looks again at a camera that cannot
// serve its node.
func sfRecheckEvery(t *testing.T, d time.Duration) {
	t.Helper()
	prev := shareCannotServeRecheck
	shareCannotServeRecheck = d
	t.Cleanup(func() { shareCannotServeRecheck = prev })
}

// sfVerdict is a pump's unserved answer a test can change.
type sfVerdict struct {
	mu     sync.Mutex
	reason string
	asked  int
}

func (v *sfVerdict) set(reason string) {
	v.mu.Lock()
	v.reason = reason
	v.mu.Unlock()
}

func (v *sfVerdict) unserved() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.asked++
	return v.reason
}

func (v *sfVerdict) timesAsked() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.asked
}

func sfUnserved(v *sfVerdict) func(*sharePump) {
	return func(p *sharePump) { p.unserved = v.unserved }
}

func (h *sharePumpHarness) noWriteWithin(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case e := <-h.node.events:
		t.Fatalf("a frame was written to the node: %+v", e)
	case <-time.After(d):
	}
}

// phasesSeen records every state a pump is in from now on, sampled as fast as
// the scheduler allows, until the returned function is called.
func (h *sharePumpHarness) phasesSeen() (stop func() map[sharePhase]bool) {
	seen := map[sharePhase]bool{}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			seen[h.pump.State().Phase] = true
			select {
			case <-done:
				return
			case <-time.After(50 * time.Microsecond):
			}
		}
	}()
	return func() map[sharePhase]bool {
		close(done)
		<-stopped
		return seen
	}
}

func (h *sharePumpHarness) expectState(t *testing.T, want shareState) {
	t.Helper()
	if !csWaitFor(spWait, func() bool { return h.pump.State() == want }) {
		t.Fatalf("state = %+v, want %+v", h.pump.State(), want)
	}
}

func TestShareDecision_EachFormatDeclaresItsOwnModeAtOneSizeAndRate(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{5, 15, 30})
	sfCanDeriveFrames(s)
	want := map[string]sharedMode{
		appconfig.CameraFormatPixels: sfPixelsMode,
		appconfig.CameraFormatNative: sfNativeMode,
		appconfig.CameraFormatH264:   sfH264Mode,
	}
	for _, format := range appconfig.CameraFormats {
		d := s.shareDecisionFor(context.Background(), usbCam(), format)
		if !d.Shared || d.AskedFormat != "" {
			t.Fatalf("%s: decision = %+v", format, d)
		}
		if d.Mode != want[format] {
			t.Errorf("%s: Mode = %+v, want %+v", format, d.Mode, want[format])
		}
		if d.Mode.pixelFormat() == 0 || d.Mode.bufferBytes() <= 0 {
			t.Errorf("%s: no node can declare %+v", format, d.Mode)
		}
		if d.Mode.Width != sfPixelsMode.Width || d.Mode.Height != sfPixelsMode.Height || d.Mode.FPS != sfPixelsMode.FPS {
			t.Errorf("%s: %s differs in size or rate from the picture mode %s", format, d.Mode, sfPixelsMode)
		}
	}
	if sfNativeMode.pixelFormat() != v4l2PixFmtMJPEG || sfH264Mode.pixelFormat() != v4l2PixFmtH264 {
		t.Errorf("pixel formats = %#x, %#x", sfNativeMode.pixelFormat(), sfH264Mode.pixelFormat())
	}
	if got := describeShareNode(sfNativeMode, appconfig.CameraStampFrame); got != "1280x720 MJPG, 30 fps, format native, stamp frame" {
		t.Errorf("describeShareNode = %q", got)
	}
}

// One node for every camera comes first: a format the camera cannot provide
// is served as pictures, and the decision says which format was asked for and
// why it could not be had. What decides is the check the capture itself makes.
func TestShareDecision_AFormatTheCameraCannotProvideIsServedAsPictures(t *testing.T) {
	jpegCamera := func(t *testing.T) *VideoService {
		return shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
	}
	cases := map[string]struct {
		service  func(t *testing.T) *VideoService
		pictures sharedMode
		whyNot   string
	}{
		"the camera has no JPEG": {
			func(t *testing.T) *VideoService {
				s := shareTestService(t, []uint32{v4l2PixFmtUYVY}, map[uint32][][2]uint32{v4l2PixFmtUYVY: {{640, 480}}}, nil)
				sfCanDeriveFrames(s)
				return s
			},
			sharedMode{640, 480, "UYVY", 1280, 30, "pixels", 0},
			sfNotWhole + "the camera offers no JPEG at 640x480",
		},
		"the device cannot derive from its frames": {jpegCamera, sfPixelsMode, sfCannotDerive},
		"deriving from its frames failed recently": {
			func(t *testing.T) *VideoService {
				s := jpegCamera(t)
				sfCanDeriveFrames(s)
				s.derivedFailures.remember(s.cameraIdentityKey(usbCam().GetPath()), "the encoder gave no frame", false)
				return s
			},
			sfPixelsMode,
			sfNotWhole + "deriving pictures and H.264 from this camera's frames failed recently: the encoder gave no frame",
		},
	}
	for name, tc := range cases {
		for _, format := range []string{appconfig.CameraFormatNative, appconfig.CameraFormatH264} {
			t.Run(name+"/"+format, func(t *testing.T) {
				s := tc.service(t)
				d := s.shareDecisionFor(context.Background(), usbCam(), format)
				want := shareDecision{Shared: true, Mode: tc.pictures, AskedFormat: format, AskedWhyNot: tc.whyNot}
				if d != want {
					t.Errorf("decision = %+v\nwant       %+v", d, want)
				}
				if alone := s.shareDecisionsFor(usbCam(), format)[format]; alone.Shared || alone.Reason != tc.whyNot {
					t.Errorf("the format on its own: %+v, want the refusal %q", alone, tc.whyNot)
				}
			})
		}
	}
}

// A device that can decode a webcam's frames and has no H.264 encoder. An
// h264 node cannot be had there, and is served as pictures: that is the rule
// above. A native node carries the camera's own frames, which no encoder
// makes, and is refused for the same reason all the same, because a capture
// hands the camera's frames on whole only when it can also derive H.264 from
// them. Nobody decided that: it is pinned here until native is decided apart.
func TestShareDecision_PinnedGap_ANativeNodeIsRefusedWithoutTheH264EncoderItDoesNotNeed(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
	sfCanDeriveFrames(s)
	s.gstMemo.mu.Lock()
	delete(s.gstMemo.available, "x264enc")
	s.gstMemo.mu.Unlock()
	const noEncoder = sfNotWhole + "pictures and H.264 cannot be derived from its frames on this device: no supported GStreamer encoder found"

	h264 := s.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatH264)
	if !h264.Shared || h264.Mode != sfPixelsMode || h264.AskedFormat != appconfig.CameraFormatH264 || !strings.HasPrefix(h264.AskedWhyNot, noEncoder) {
		t.Errorf("h264: decision = %+v, want pictures served because %q", h264, noEncoder)
	}

	native := s.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatNative)
	if !native.Shared || native.Mode != sfPixelsMode || native.AskedFormat != appconfig.CameraFormatNative || !strings.HasPrefix(native.AskedWhyNot, noEncoder) {
		t.Errorf(sfPinnedGap+"native: decision = %+v; today a native node is refused for the encoder it does not need, and pictures are served because %q",
			native, noEncoder)
	}
	if alone := s.shareDecisionsFor(usbCam(), appconfig.CameraFormatNative)[appconfig.CameraFormatNative]; alone.Shared || !strings.HasPrefix(alone.Reason, noEncoder) {
		t.Errorf(sfPinnedGap+"native on its own: %+v, today refused because %q", alone, noEncoder)
	}
}

func TestShareDecision_ACameraWithNoModeAtAllStaysUnsharedWithItsReason(t *testing.T) {
	cases := map[string]struct {
		service func(t *testing.T) (*VideoService, *agentpb.VideoDevice)
		reason  string
	}{
		"a camera no format shares": {
			func(t *testing.T) (*VideoService, *agentpb.VideoDevice) {
				s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
				sfCanDeriveFrames(s)
				dev := usbCam()
				dev.Transport = agentpb.VideoTransport_VIDEO_TRANSPORT_CSI
				return s, dev
			},
			"CSI cameras are not shared yet",
		},
		"no pictures either": {
			func(t *testing.T) (*VideoService, *agentpb.VideoDevice) {
				return shareTestService(t, []uint32{v4l2PixFmtYUYV}, map[uint32][][2]uint32{}, nil), usbCam()
			},
			"no shared mode: camera advertises no discrete frame size to capture raw frames at",
		},
	}
	for name, tc := range cases {
		for _, format := range appconfig.CameraFormats {
			t.Run(name+"/"+format, func(t *testing.T) {
				s, dev := tc.service(t)
				if d := s.shareDecisionFor(context.Background(), dev, format); d != (shareDecision{Reason: tc.reason}) {
					t.Errorf("decision = %+v, want only the reason %q", d, tc.reason)
				}
			})
		}
	}
}

func TestCreateCameraShares_EachCamerasNodeFollowsItsEntitlement(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920(), csCam6())
	sfCanDeriveFrames(h.s)
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "C920"),
		sfCameraEntitlement("", "", "Cam Six"),
	})

	type node struct {
		mode  sharedMode
		stamp string
		kind  frameKind
	}
	want := map[string]node{
		"Brio 101": {sfNativeMode, appconfig.CameraStampFrame, nativeFrames},
		"C920":     {sfH264Mode, appconfig.CameraStampTime, encodedFrames},
		"Cam Six":  {sfPixelsMode, appconfig.CameraStampTime, pixelFrames},
	}
	records := h.records("cv-a")
	if len(records) != len(want) {
		t.Fatalf("records = %d, want %d; logs: %v", len(records), len(want), h.logs.All())
	}
	opened := map[string]sharedMode{}
	for _, n := range h.openedNodes() {
		opened[n.path] = n.mode
	}
	for i, r := range records {
		w := want[r.cameraName]
		if r.mode != w.mode || r.stamp != w.stamp || r.askedFormat != "" {
			t.Errorf("%s: record holds %s (asked %q), want %s", r.cameraName, describeShareNode(r.mode, r.stamp), r.askedFormat, describeShareNode(w.mode, w.stamp))
		}
		if got := opened[shareNodePath(r.node)]; got != w.mode {
			t.Errorf("%s: node declares %+v, want %+v", r.cameraName, got, w.mode)
		}
		if r.pump.mode != w.mode || r.pump.stamp != w.stamp {
			t.Errorf("%s: pump serves %s", r.cameraName, describeShareNode(r.pump.mode, r.pump.stamp))
		}

		r.io.(*csNode).send(t, 1)
		if !csWaitFor(2*time.Second, func() bool { return fp.count() == i+1 }) {
			t.Fatalf("%s: its camera did not start; logs: %v", r.cameraName, h.logs.All())
		}
		hub := fp.hub(i)
		hub.mu.Lock()
		var kinds []frameKind
		for _, sub := range hub.subs {
			kinds = append(kinds, sub.kind)
		}
		hub.mu.Unlock()
		if !reflect.DeepEqual(kinds, []frameKind{w.kind}) {
			t.Errorf("%s: hub subscriptions = %v, want one of kind %v", r.cameraName, kinds, w.kind)
		}
	}
	if n := h.logged("camera share: cv-a shares Brio 101 (/dev/video0) through /dev/video255 at 1280x720 MJPG, 30 fps, format native, stamp frame"); n != 1 {
		t.Errorf("shares line with the node's settings logged %d times; logs: %v", n, h.logs.All())
	}
	if got, want := h.reportOf("/dev/video0")["share_state"], "cv-a [format native, stamp frame] (reading)"; got != want {
		t.Errorf("share_state = %q, want %q", got, want)
	}
	if row := h.reportOf("/dev/video0"); row["share_mode"] != "1280x720 YUYV, 30 fps" || row["share_mode_native"] != "1280x720 MJPG, 30 fps" ||
		row["share_mode_h264"] != "1280x720 H264, 30 fps" || row["share_unavailable_native"] != "" || row["share_unavailable_h264"] != "" {
		t.Errorf("the camera's modes by format = %v", row)
	}

	shares := h.stateShares("cv-a")
	if len(shares) != 3 {
		t.Fatalf("state file shares = %v", shares)
	}
	for _, sh := range shares {
		w := want[sh["camera"].(map[string]any)["name"].(string)]
		mode := sh["mode"].(map[string]any)
		if sh["format"] != w.mode.Format || sh["stamp"] != w.stamp || mode["fourcc"] != w.mode.Fourcc {
			t.Errorf("state file share = %v, want %s", sh, describeShareNode(w.mode, w.stamp))
		}
		if max, compressed := mode["maxFrameBytes"]; compressed != w.mode.compressed() || (compressed && max != float64(maxFrameBytes)) {
			t.Errorf("state file mode = %v, want maxFrameBytes %d", mode, w.mode.MaxFrameBytes)
		}
	}
	if v := h.stateFile()["version"]; v != float64(2) {
		t.Errorf("state file version = %v, want 2", v)
	}
}

// One app with a node on each of three cameras, two that produce JPEG and one
// that produces H.264. A frame of every camera, on every plane its hub has,
// is delivered before any node is read: each node holds only its own camera's
// frame of its own format, whole, with the stamp its entry asks for.
func TestCreateCameraShares_AJPEGAndAnH264CameraInOneAppEachFillTheirOwnNode(t *testing.T) {
	h := newCSHarness(t, csBrio(), csH264Cam(), csC920())
	hcCanDecode(h.s, "avdec_h264", "jpegdec", "x264enc")
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "Logi H264"),
		sfCameraEntitlement("", appconfig.CameraStampFrame, "C920"),
	})

	brioJPEG, brioPicture, brioUnit := sfJPEGFrame(7, 9_000_000_000), sfDerivedPicture(7, 9_000_000_000), sfH264Frame(7, true)
	logiUnit, logiPicture := hcOwnFrame(41, true, 5_000_000_000), sfDerivedPicture(41, 5_000_000_000)
	c920JPEG, c920Picture, c920Unit := sfJPEGFrame(3, 2_000_000_000), sfDerivedPicture(3, 2_000_000_000), sfH264Frame(3, true)
	// then is the frame after own on the node's own plane: the node holds it
	// right after own, so nothing else was written in between.
	type node struct {
		mode             sharedMode
		stamp            string
		kind             frameKind
		planes           []*videoFrame
		own, then        *videoFrame
		stamps, stamped2 shareFrameStamp
	}
	want := map[string]node{
		"Brio 101": {sfNativeMode, appconfig.CameraStampFrame, nativeFrames, []*videoFrame{brioPicture, brioUnit, brioJPEG},
			brioJPEG, sfJPEGFrame(8, 9_100_000_000), stampWithSampleID(7), stampWithSampleID(8)},
		"Logi H264": {hcH264Mode, appconfig.CameraStampTime, encodedFrames, []*videoFrame{logiPicture, logiUnit},
			logiUnit, hcOwnFrame(42, false, 5_100_000_000), stampWithCaptureTime(5_000_000_000), stampWithCaptureTime(5_100_000_000)},
		"C920": {sfPixelsMode, appconfig.CameraStampFrame, pixelFrames, []*videoFrame{c920JPEG, c920Unit, c920Picture},
			c920Picture, sfDerivedPicture(4, 2_100_000_000), stampWithSampleID(3), stampWithSampleID(4)},
	}
	records := h.records("cv-a")
	if len(records) != len(want) {
		t.Fatalf("records = %d, want %d; logs: %v", len(records), len(want), h.logs.All())
	}
	// A record's path is its pump's to write at each join: read under the lock.
	paths := map[string]string{}
	for i, v := range h.view("cv-a") {
		paths[records[i].cameraName] = v.path
	}
	for _, r := range records {
		w := want[r.cameraName]
		if r.mode != w.mode || r.stamp != w.stamp || r.askedFormat != "" {
			t.Fatalf("%s: record holds %s (asked %q), want %s", r.cameraName, describeShareNode(r.mode, r.stamp), r.askedFormat, describeShareNode(w.mode, w.stamp))
		}
		r.io.(*csNode).send(t, 1)
	}
	if !csWaitFor(2*time.Second, func() bool { return fp.count() == len(want) }) {
		t.Fatalf("%d captures started, want one for each camera; logs: %v", fp.count(), h.logs.All())
	}
	for _, r := range records {
		var kinds []frameKind
		if !csWaitFor(2*time.Second, func() bool {
			hub := currentHub(h.s, paths[r.cameraName])
			if hub == nil {
				return false
			}
			hub.mu.Lock()
			defer hub.mu.Unlock()
			kinds = kinds[:0]
			for _, sub := range hub.subs {
				kinds = append(kinds, sub.kind)
			}
			return len(kinds) == 1
		}) {
			t.Fatalf("%s: its camera did not start for its node alone; logs: %v", r.cameraName, h.logs.All())
		}
		if w := want[r.cameraName]; kinds[0] != w.kind {
			t.Errorf("%s: hub subscription is of kind %v, want %v", r.cameraName, kinds[0], w.kind)
		}
	}

	for _, r := range records {
		for _, f := range want[r.cameraName].planes {
			currentHub(h.s, paths[r.cameraName]).broadcast(f)
		}
	}
	for _, r := range records {
		currentHub(h.s, paths[r.cameraName]).broadcast(want[r.cameraName].then)
	}
	for _, r := range records {
		w, written := want[r.cameraName], r.io.(*csNode)
		if e := written.next(t); e.size != len(w.own.data) || e.stamp != w.stamps {
			t.Errorf("%s: its node got %+v, want the %d-byte frame stamped %+v", r.cameraName, e, len(w.own.data), w.stamps)
		}
		if e := written.next(t); e.size != len(w.then.data) || e.stamp != w.stamped2 {
			t.Errorf("%s: its node then got %+v, want its camera's next frame, %d bytes stamped %+v: another frame was written before it",
				r.cameraName, e, len(w.then.data), w.stamped2)
		}
		if fp.count() != len(want) {
			t.Errorf("%s: %d captures started by now, want still one for each camera", r.cameraName, fp.count())
		}
	}
	for path, state := range map[string]string{
		"/dev/video0": "cv-a [format native, stamp frame] (reading)",
		"/dev/video2": "cv-a [format h264, stamp time] (reading)",
		"/dev/video4": "cv-a [format pixels, stamp frame] (reading)",
	} {
		if got := h.reportOf(path)["share_state"]; got != state {
			t.Errorf("%s: share_state = %q, want %q", path, got, state)
		}
	}
}

func TestCreateCameraShares_NoEntitlementSettingsIsPixelsWithCaptureTimes(t *testing.T) {
	for name, entitlements := range map[string][]appconfig.Entitlement{
		"no entitlements":      nil,
		"camera without keys":  {sfCameraEntitlement("", "")},
		"values not known yet": {sfCameraEntitlement("raw16", "sequence")},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			sfCanDeriveFrames(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
			records := h.records("cv-a")
			if len(records) != 1 {
				t.Fatalf("records = %d; logs: %v", len(records), h.logs.All())
			}
			if r := records[0]; r.mode != sfPixelsMode || r.stamp != appconfig.CameraStampTime || r.askedFormat != "" {
				t.Fatalf("record holds %s (asked %q)", describeShareNode(r.mode, r.stamp), r.askedFormat)
			}
			if st := records[0].pump.State(); st != (shareState{Phase: shareIdle}) {
				t.Fatalf("state = %+v, want idle", st)
			}
		})
	}
}

// A container's nodes are made when it is created, for the cameras present
// then. An entry that names a camera plugged in afterwards has no effect until
// the next create: today the camera gets no node at the replug, at a sync or at
// the container's next start, and what the app finds at its path is not a node
// the agent feeds. Nobody decided that; it is pinned until a camera that
// appears is given the node its entry asks for. The next create giving it that
// node is the rule, and the last part of the test.
func TestCreateCameraShares_PinnedGap_ACameraPluggedInAfterTheCreateGetsNoNodeUntilTheNextCreate(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	entitlements := []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "C920"),
		sfCameraEntitlement("", "", "Brio 101"),
	}
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	h.watch()
	present := h.records("cv-a")
	if len(present) != 1 || present[0].cameraName != "Brio 101" {
		t.Fatalf("records = %+v, want one for the Brio, the only camera present at the create; logs: %v", present, h.logs.All())
	}
	stillNoNodeForTheC920 := func(when string, covers []csCover) {
		t.Helper()
		if records := h.records("cv-a"); len(records) != 1 || records[0] != present[0] || len(h.openedNodes()) != 1 {
			t.Fatalf(sfPinnedGap+"%s: records = %+v with %d nodes opened; today the C920 its entry names has no node yet",
				when, records, len(h.openedNodes()))
		}
		if got := coversWithout(h.coverList()); !reflect.DeepEqual(got, covers) {
			t.Fatalf(sfPinnedGap+"%s: covers = %+v; today only the Brio is covered: %+v", when, got, covers)
		}
		if row := h.reportOf("/dev/video4"); row["shared_with"] != "" || row["share_state"] != "" || row["share_mode_native"] != "1280x720 MJPG, 30 fps" {
			t.Fatalf(sfPinnedGap+"%s: the C920's row = %v; today it could have a native node and is shared with no app", when, row)
		}
	}

	h.plug(csC920())
	h.waitLogged("camera share: /dev/video4 appeared; no running app shares it", 1)
	atStart := []csCover{{pid: 11, src: "/dev/video255", dst: "/dev/video0"}}
	stillNoNodeForTheC920("after the camera was plugged in", atStart)
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true, PID: 11}})
	stillNoNodeForTheC920("after a sync", atStart)
	h.s.ReleaseCameraShares("cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 12); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	stillNoNodeForTheC920("after the container's next start", append(atStart, csCover{pid: 12, src: "/dev/video255", dst: "/dev/video0"}))

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
	records := h.records("cv-a")
	if len(records) != 2 || records[0] != present[0] {
		t.Fatalf("after the next create: records = %+v, want the Brio's kept and the C920's; logs: %v", records, h.logs.All())
	}
	if r := records[1]; r.cameraName != "C920" || r.mode != sfNativeMode || r.stamp != appconfig.CameraStampFrame || r.askedFormat != "" {
		t.Errorf("the C920's record holds %s (asked %q), want %s as its entry asks",
			describeShareNode(r.mode, r.stamp), r.askedFormat, describeShareNode(sfNativeMode, appconfig.CameraStampFrame))
	}
	if got, want := h.reportOf("/dev/video4")["share_state"], "cv-a [format native, stamp frame] (idle)"; got != want {
		t.Errorf("share_state = %q, want %q", got, want)
	}
}

// Every entry of the app has an allowlist, and one camera is on none of them.
// Whether such a camera should be shared with the app at all is not decided.
// Today an allowlist only says which cameras an entry's format and stamp are
// for, so the camera gets a node like any other, of pixels with capture times.
// That is pinned here, beside the rule that a camera an entry names follows it.
func TestCreateCameraShares_PinnedGap_ACameraNoAllowlistNamesIsSharedAsPixelsWithCaptureTimes(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "C920"),
		sfCameraEntitlement(appconfig.CameraFormatH264, appconfig.CameraStampFrame, "/dev/video9", "Cam Six"),
	})
	type node struct {
		mode  sharedMode
		stamp string
	}
	got := map[string]node{}
	for _, r := range h.records("cv-a") {
		got[r.cameraName] = node{r.mode, r.stamp}
	}
	if named, want := got["C920"], (node{sfNativeMode, appconfig.CameraStampFrame}); named != want {
		t.Fatalf("the camera an entry names has the node %+v, want %+v; logs: %v", named, want, h.logs.All())
	}
	unnamed, shared := got["Brio 101"]
	if !shared || len(got) != 2 || unnamed != (node{sfPixelsMode, appconfig.CameraStampTime}) {
		t.Fatalf(sfPinnedGap+"nodes = %+v; today the Brio, which no allowlist names, is shared as pixels with capture times; logs: %v", got, h.logs.All())
	}
	if got, today := h.reportOf("/dev/video0")["share_state"], "cv-a [format pixels, stamp time] (idle)"; got != today {
		t.Errorf(sfPinnedGap+"share_state of the camera no allowlist names = %q, today %q", got, today)
	}
	if n := h.logged("camera share: cv-a shares Brio 101 (/dev/video0) through /dev/video255 at 1280x720 YUYV, 30 fps, format pixels, stamp time"); n != 1 {
		t.Errorf(sfPinnedGap+"the shares line of the camera no allowlist names was logged %d times, today once; logs: %v", n, h.logs.All())
	}
}

// An app that asks for a format its camera cannot provide still gets a node
// for the camera, of pictures, and every report of the share says so. Its
// other settings, and other apps' nodes, are as they would be.
func TestCreateCameraShares_AFormatTheCameraCannotProvideGetsAPicturesNodeThatSaysSo(t *testing.T) {
	for _, format := range []string{appconfig.CameraFormatNative, appconfig.CameraFormatH264} {
		t.Run(format, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			fp := installFakeProducers(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(format, "")})
			h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", nil)

			records := h.records("cv-a")
			if len(records) != 1 || len(h.records("cv-b")) != 1 {
				t.Fatalf("records = %d and %d, want a node each; logs: %v", len(records), len(h.records("cv-b")), h.logs.All())
			}
			r := records[0]
			if r.mode != sfPixelsMode || r.stamp != appconfig.CameraStampTime || r.askedFormat != format || r.askedWhyNot != sfCannotDerive {
				t.Fatalf("record holds %s, asked %q (%q)", describeShareNode(r.mode, r.stamp), r.askedFormat, r.askedWhyNot)
			}
			if got := r.io.(*csNode).mode; got != sfPixelsMode {
				t.Errorf("the node declares %+v, want pictures", got)
			}
			said := "asked for " + format + "; serving pixels because " + sfCannotDerive
			var warned int
			for _, e := range h.logs.All() {
				if e.Message == "camera share: cv-a: Brio 101 (/dev/video0): "+said && e.Level.String() == "warn" {
					warned++
				}
			}
			if warned != 1 {
				t.Errorf("warned %d times that pictures are served instead; logs: %v", warned, h.logs.All())
			}
			row := h.reportOf("/dev/video0")
			wantState := "cv-a [format pixels, stamp time] (idle; " + said + "), cv-b [format pixels, stamp time] (idle)"
			if row["share_state"] != wantState {
				t.Errorf("share_state = %q\nwant         %q", row["share_state"], wantState)
			}
			if row["share_mode"] != "1280x720 YUYV, 30 fps" || row["share_mode_"+format] != "" || row["share_unavailable_"+format] != sfCannotDerive || row["share_unavailable"] != "" {
				t.Errorf("the camera's row = %v", row)
			}
			if sh := h.stateShares("cv-a")[0]; sh["askedFormat"] != format || sh["askedWhyNot"] != sfCannotDerive {
				t.Errorf("state file share = %v", sh)
			}

			r.io.(*csNode).send(t, 1)
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
				t.Fatalf("the camera did not start; logs: %v", h.logs.All())
			}
			wall := uint64(time.Now().UnixNano())
			fp.hub(0).broadcast(sfTapPicture(wall))
			if e := r.io.(*csNode).next(t); e.size != len(sfTapPicture(wall).data) || e.stamp.holdsSampleID {
				t.Errorf("the node got %+v, want the picture with its capture time", e)
			}
		})
	}
}

func TestCreateCameraShares_AnAskedFormatSurvivesAnAgentRestartInTheState(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")})
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	h.s.Shutdown()
	h = newCSHarness(t, csBrio())
	h.loop.aux[255] = "Brio 101"
	h.writeState(string(state))
	h.s.loadCameraShares()
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a"}})
	want := "cv-a [format pixels, stamp time] (idle; asked for native; serving pixels because " + sfCannotDerive + ")"
	if got := h.reportOf("/dev/video0")["share_state"]; got != want {
		t.Errorf("share_state = %q\nwant         %q", got, want)
	}
}

// A "frame" stamp on a camera whose pictures carry no frame number: the node
// exists, the camera is never opened for it, and its state says why from the
// moment it is made.
func TestCreateCameraShares_AFrameStampNodeWithNoFrameNumbersIsNotJoinedAndSaysWhy(t *testing.T) {
	sfRecheckEvery(t, 5*time.Millisecond)
	h := newCSHarness(t, csBrio())
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement("", appconfig.CameraStampFrame)})
	records := h.records("cv-a")
	if len(records) != 1 || records[0].mode != sfPixelsMode || records[0].stamp != appconfig.CameraStampFrame {
		t.Fatalf("records = %+v; logs: %v", records, h.logs.All())
	}
	r := records[0]
	why := "the camera's own frames are not captured whole (" + sfNoOwnFrames + "pictures and H.264 cannot be derived from its frames on this device: GStreamer lacks gdpdepay, gdppay, h264parse)" +
		`, so its pictures carry no frame number for the "frame" stamp`
	want := "cv-a [format pixels, stamp frame] (cannot serve: " + why + ")"
	if !csWaitFor(2*time.Second, func() bool { return h.reportOf("/dev/video0")["share_state"] == want }) {
		t.Fatalf("share_state with no reader = %q\nwant                         %q", h.reportOf("/dev/video0")["share_state"], want)
	}

	r.io.(*csNode).send(t, 1)
	time.Sleep(50 * time.Millisecond) // ten rechecks
	if fp.count() != 0 {
		t.Fatalf("the camera was opened %d times for a node it cannot serve", fp.count())
	}
	if got := h.reportOf("/dev/video0")["share_state"]; got != want {
		t.Fatalf("share_state with a reader = %q\nwant                       %q", got, want)
	}

	// The camera's frames can be captured whole now: the next look joins.
	sfCanDeriveFrames(h.s)
	if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
		t.Fatalf("the camera did not start once it could serve the node; logs: %v", h.logs.All())
	}
	fp.hub(0).broadcast(sfDerivedPicture(77, 5_000_000_000))
	if e := r.io.(*csNode).next(t); e.stamp != stampWithSampleID(77) {
		t.Errorf("buffer stamp = %+v, want sample ID 77", e.stamp)
	}
	if !csWaitFor(2*time.Second, func() bool {
		return h.reportOf("/dev/video0")["share_state"] == "cv-a [format pixels, stamp frame] (reading)"
	}) {
		t.Errorf("share_state after a frame = %q", h.reportOf("/dev/video0")["share_state"])
	}
}

// A container created again while a record of an earlier one is still there
// gets the node its own entitlements ask for.
func TestCreateCameraShares_AContainerCreatedAgainFollowsItsNewSettings(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
	stale := h.records("cv-a")[0]
	staleNode := stale.io.(*csNode)

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
	if records := h.records("cv-a"); len(records) != 1 || records[0] != stale || len(h.openedNodes()) != 1 {
		t.Fatalf("a create with the same settings made the share again: %+v", records)
	}

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame)})
	h.drainRemovals()
	records := h.records("cv-a")
	if len(records) != 1 || records[0] == stale {
		t.Fatalf("records = %+v, want one new record; logs: %v", records, h.logs.All())
	}
	r := records[0]
	if r.mode != sfNativeMode || r.stamp != appconfig.CameraStampFrame || r.io.(*csNode).mode != sfNativeMode || r.pump.stamp != appconfig.CameraStampFrame {
		t.Errorf("the new share is %s", describeShareNode(r.mode, r.stamp))
	}
	if staleNode.closes.Load() != 1 || !reflect.DeepEqual(h.loop.removedAux(), []int{stale.node}) {
		t.Errorf("the stale node: closed %d times, removed nodes %v", staleNode.closes.Load(), h.loop.removedAux())
	}
	if n := h.logged("camera share: cv-a was created again: its share of Brio 101 was 1280x720 YUYV, 30 fps, format pixels, stamp time and is made again as 1280x720 MJPG, 30 fps, format native, stamp frame"); n != 1 {
		t.Errorf("the replacement was logged %d times; logs: %v", n, h.logs.All())
	}
	if shares := h.stateShares("cv-a"); len(shares) != 1 || shares[0]["format"] != "native" || shares[0]["stamp"] != "frame" {
		t.Errorf("state file shares = %v", shares)
	}

	// Only the stamp differs.
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")})
	h.drainRemovals()
	if records := h.records("cv-a"); len(records) != 1 || records[0].mode != sfNativeMode || records[0].stamp != appconfig.CameraStampTime {
		t.Errorf("after a create with another stamp: %+v", records)
	}
}

// A camera that produces H.264 has the same frames for a native node and an
// h264 one, and both declare H264. They are still two nodes: a container
// created again with the other format gets its node made again, as one that
// goes to pixels does.
func TestCreateCameraShares_AnH264CamerasNodeIsMadeAgainForAnotherFormat(t *testing.T) {
	h := newCSHarness(t, csH264Cam())
	hcCanDecode(h.s, "avdec_h264")
	installFakeProducers(h.s)
	subscribedAs := func(r *shareRecord, kind frameKind) bool {
		r.io.(*csNode).send(t, 1)
		return csWaitFor(2*time.Second, func() bool {
			hub := currentHub(h.s, "/dev/video2")
			if hub == nil {
				return false
			}
			hub.mu.Lock()
			defer hub.mu.Unlock()
			if len(hub.subs) != 1 {
				return false
			}
			for _, sub := range hub.subs {
				return sub.kind == kind
			}
			return false
		})
	}
	native := []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")}
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", native)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", native)
	records := h.records("cv-a")
	if len(records) != 1 || records[0].mode != hcNativeMode || len(h.openedNodes()) != 1 {
		t.Fatalf("records = %+v with %d nodes opened, want one native node kept; logs: %v", records, len(h.openedNodes()), h.logs.All())
	}
	if !subscribedAs(records[0], nativeFrames) {
		t.Fatalf("the native node did not subscribe for the camera's own frames; logs: %v", h.logs.All())
	}

	var removed []int
	for i, step := range []struct {
		format string
		mode   sharedMode
		kind   frameKind
	}{
		{appconfig.CameraFormatH264, hcH264Mode, encodedFrames},
		{appconfig.CameraFormatPixels, hcPixelsMode, pixelFrames},
	} {
		stale := h.records("cv-a")[0]
		staleNode := stale.io.(*csNode)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(step.format, "")})
		h.drainRemovals()
		records := h.records("cv-a")
		if len(records) != 1 || records[0] == stale || len(h.openedNodes()) != i+2 {
			t.Fatalf("%s: records = %+v with %d nodes opened, want one new record on a new node; logs: %v", step.format, records, len(h.openedNodes()), h.logs.All())
		}
		r := records[0]
		if r.mode != step.mode || r.stamp != appconfig.CameraStampTime || r.askedFormat != "" || r.io.(*csNode).mode != step.mode || r.pump.mode != step.mode {
			t.Errorf("%s: the new share is %s (asked %q) on a node declaring %+v", step.format, describeShareNode(r.mode, r.stamp), r.askedFormat, r.io.(*csNode).mode)
		}
		removed = append(removed, stale.node)
		if staleNode.closes.Load() != 1 || !reflect.DeepEqual(h.loop.removedAux(), removed) {
			t.Errorf("%s: the stale node: closed %d times, removed nodes %v, want %v", step.format, staleNode.closes.Load(), h.loop.removedAux(), removed)
		}
		line := "camera share: cv-a was created again: its share of Logi H264 was " + describeShareNode(stale.mode, stale.stamp) +
			" and is made again as " + describeShareNode(step.mode, appconfig.CameraStampTime)
		if n := h.logged(line); n != 1 {
			t.Errorf("%s: %q logged %d times; logs: %v", step.format, line, n, h.logs.All())
		}
		shares := h.stateShares("cv-a")
		if len(shares) != 1 || shares[0]["node"] != float64(r.node) || shares[0]["mode"].(map[string]any)["fourcc"] != step.mode.Fourcc {
			t.Errorf("%s: state file shares = %v", step.format, shares)
		}
		// A file with only pixels and capture times is version 1, which has no settings.
		if format, stored := shares[0]["format"]; stored != step.mode.compressed() || (stored && format != step.format) {
			t.Errorf("%s: state file share = %v", step.format, shares[0])
		}
		if !subscribedAs(r, step.kind) {
			t.Errorf("%s: the new node did not subscribe for frames of kind %v", step.format, step.kind)
		}
	}
}

// Two cameras of one model, told apart in the app's entitlements by their
// paths. A record follows its camera by the port it is on, not by its path, so
// when the two come back with their paths swapped each node keeps its settings
// and covers its camera's new path. The path an entry names then serves the
// other entry's settings, without a warning, until the next create reads the
// allowlists again.
func TestRecover_SameModelCamerasThatSwapPathsKeepTheirSettingsNotTheirPathsEntries(t *testing.T) {
	left := csBrio()
	right := csCamera{"video2", "Brio 101", left.formats, camera.Identity{Device: csPort("1-2.2"), Index: "0", Name: "Brio 101"}}
	h := newCSHarness(t, left, right)
	sfCanDeriveFrames(h.s)
	entitlements := []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "/dev/video2"),
	}
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	h.watch()

	type node struct {
		path  string
		mode  sharedMode
		stamp string
		nr    int
	}
	nodes := func() map[string]node {
		out := map[string]node{}
		views := h.view("cv-a")
		for i, r := range h.records("cv-a") {
			out[views[i].camera.Device] = node{views[i].path, r.mode, r.stamp, views[i].node}
		}
		return out
	}
	asEntered := map[string]node{
		left.id.Device:  {"/dev/video0", sfNativeMode, appconfig.CameraStampFrame, 255},
		right.id.Device: {"/dev/video2", sfH264Mode, appconfig.CameraStampTime, 254},
	}
	if got := nodes(); !reflect.DeepEqual(got, asEntered) {
		t.Fatalf("nodes at the create = %+v, want %+v; logs: %v", got, asEntered, h.logs.All())
	}
	created := h.records("cv-a")

	loggedBeforeTheSwap := h.logs.Len()
	h.unplug("video0")
	h.unplug("video2")
	h.plug(csCamera{"video0", "Brio 101", left.formats, right.id})
	h.waitLogged("camera share: covered /dev/video0 for cv-a again in ", 1)
	h.plug(csCamera{"video2", "Brio 101", left.formats, left.id})
	h.waitLogged("camera share: covered /dev/video2 for cv-a again in ", 1)

	swapped := map[string]node{
		left.id.Device:  {"/dev/video2", sfNativeMode, appconfig.CameraStampFrame, 255},
		right.id.Device: {"/dev/video0", sfH264Mode, appconfig.CameraStampTime, 254},
	}
	if got := nodes(); !reflect.DeepEqual(got, swapped) {
		t.Fatalf("nodes after the swap = %+v, want %+v", got, swapped)
	}
	if records := h.records("cv-a"); len(records) != 2 || records[0] != created[0] || records[1] != created[1] || len(h.openedNodes()) != 2 {
		t.Errorf("the swap made a share again: %+v with %d nodes opened", records, len(h.openedNodes()))
	}
	wantCovers := []csCover{{pid: 11, src: "/dev/video254", dst: "/dev/video0"}, {pid: 11, src: "/dev/video255", dst: "/dev/video2"}}
	if got := coversWithout(h.coverList()[2:]); !reflect.DeepEqual(got, wantCovers) {
		t.Errorf("covers after the swap = %+v, want %+v", got, wantCovers)
	}
	// What the app now reads at each path is the other path's entry. The h264
	// node names its encoder as it did before the swap.
	for path, state := range map[string]string{
		"/dev/video0": "cv-a [format h264, stamp time] (idle; " + sfSoftwareEncoderIdle + ")",
		"/dev/video2": "cv-a [format native, stamp frame] (idle)",
	} {
		if got := h.reportOf(path)["share_state"]; got != state {
			t.Errorf(sfPinnedGap+"%s: share_state = %q; today it is %q, the other path's entry, with no note of the swap", path, got, state)
		}
	}
	// Whatever is logged of this app's shares from the swap on, at any level,
	// but that each camera was covered again.
	var said []string
	for _, e := range h.logs.All()[loggedBeforeTheSwap:] {
		if strings.Contains(e.Message, "cv-a") && !strings.HasPrefix(e.Message, "camera share: covered /dev/video") {
			said = append(said, e.Level.String()+": "+e.Message)
		}
	}
	if len(said) != 0 {
		t.Errorf(sfPinnedGap+"the log says of cv-a's shares after the swap: %q; today it says only that each camera was covered again, "+
			"and nothing of a path that now serves another entry's settings", said)
	}

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
	h.drainRemovals()
	asEnteredAgain := map[string]node{
		right.id.Device: {path: "/dev/video0", mode: sfNativeMode, stamp: appconfig.CameraStampFrame},
		left.id.Device:  {path: "/dev/video2", mode: sfH264Mode, stamp: appconfig.CameraStampTime},
	}
	got := nodes()
	for port, n := range got {
		n.nr = 0
		got[port] = n
	}
	if !reflect.DeepEqual(got, asEnteredAgain) {
		t.Fatalf("nodes after the next create = %+v, want %+v; logs: %v", got, asEnteredAgain, h.logs.All())
	}
	if n := h.logged("camera share: cv-a was created again: its share of Brio 101 was "); n != 2 {
		t.Errorf("%d shares were made again, want both; logs: %v", n, h.logs.All())
	}
}

// A camera with an h264 or native node and a "frame" stamp goes while its app
// reads and comes back, at its path or at another one: the node covers it
// again and is fed again. An h264 node resumes on a frame the app can decode
// alone. The sample IDs go on where they were, so the app reads none twice.
func TestRecover_ANodeWithAFrameStampIsCoveredAndFedAgainAfterAReplug(t *testing.T) {
	elsewhere := csBrio()
	elsewhere.base = "video2"
	for _, tc := range []struct {
		name   string
		format string
		back   csCamera
	}{
		{"h264, back at its path", appconfig.CameraFormatH264, csBrio()},
		{"native, back at its path", appconfig.CameraFormatNative, csBrio()},
		{"h264, back at another path", appconfig.CameraFormatH264, elsewhere},
		{"native, back at another path", appconfig.CameraFormatNative, elsewhere},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := shareRetryBackoff
			shareRetryBackoff = []time.Duration{5 * time.Millisecond, time.Hour}
			t.Cleanup(func() { shareRetryBackoff = prev }) // after the harness's Shutdown
			h := newCSHarness(t, csBrio())
			sfCanDeriveFrames(h.s)
			fp := installFakeProducers(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a",
				[]appconfig.Entitlement{sfCameraEntitlement(tc.format, appconfig.CameraStampFrame)})
			if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
				t.Fatalf("errors: %v", errs)
			}
			h.watch()
			records := h.records("cv-a")
			if len(records) != 1 || records[0].mode.Format != tc.format {
				t.Fatalf("records = %+v; logs: %v", records, h.logs.All())
			}
			node := records[0].io.(*csNode)

			// One camera frame as its capture hands it to the hub, which numbers
			// it, and for an h264 node the access unit made from it.
			capture := func(hub *deviceHub, randomAccess bool) (sampleID uint64, size int) {
				own := sfJPEGFrame(0, 1_000_000_000)
				hub.produce(own)
				if tc.format != appconfig.CameraFormatH264 {
					return own.sampleID, len(own.data)
				}
				unit := sfH264Frame(0, randomAccess)
				if _, err := hub.broadcastDerivedFrom(own, unit); err != nil {
					t.Fatal(err)
				}
				return own.sampleID, len(unit.data)
			}
			wantWrite := func(what string, sampleID uint64, size int) {
				t.Helper()
				if e := node.next(t); e.stamp != stampWithSampleID(sampleID) || e.size != size {
					t.Fatalf("%s: the node got %+v, want the %d-byte frame stamped with sample ID %d", what, e, size, sampleID)
				}
			}

			node.send(t, 1)
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
				t.Fatalf("the camera did not start; logs: %v", h.logs.All())
			}
			for i, randomAccess := range []bool{true, false} {
				sampleID, size := capture(fp.hub(0), randomAccess)
				if sampleID != uint64(i+1) {
					t.Fatalf("the camera's frame %d is numbered %d", i+1, sampleID)
				}
				wantWrite("before the replug", sampleID, size)
			}

			h.unplug("video0")
			fp.hub(0).cancel() // its capture ends with the camera
			h.waitLogged("retrying in 1h0m0s", 1)
			if st := records[0].pump.State(); st != (shareState{Phase: shareUnplugged}) {
				t.Fatalf("state with the camera away = %+v, want %q", st, shareUnplugged)
			}

			backAt := "/dev/" + tc.back.base
			h.plug(tc.back)
			h.waitLogged("camera share: covered "+backAt+" for cv-a again in ", 1)
			if covers, want := h.coverList(), (csCover{11, "/dev/video255", backAt, cameraShareRecoverTimeout}); len(covers) != 2 || covers[1] != want {
				t.Errorf("covers = %+v, want the start's and %+v", covers, want)
			}
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 2 }) {
				t.Fatalf("the camera did not start again within 2 s of coming back; logs: %v", h.logs.All())
			}
			if hub := currentHub(h.s, backAt); hub != fp.hub(1) {
				t.Fatalf("the capture that started is not the one of %s", backAt)
			}

			var numbers []uint64
			var sizes []int
			for _, randomAccess := range []bool{false, true, false, true} {
				sampleID, size := capture(fp.hub(1), randomAccess)
				numbers, sizes = append(numbers, sampleID), append(sizes, size)
			}
			if want := []uint64{3, 4, 5, 6}; !reflect.DeepEqual(numbers, want) {
				t.Fatalf("sample IDs after the replug = %v, want %v: they go on from the 2 before it", numbers, want)
			}
			// Every write is read in turn up to the last frame, so none is
			// written between two of them.
			written := 2
			for i := range numbers {
				if i == 0 && tc.format == appconfig.CameraFormatH264 {
					continue // not decodable alone: the app has none of the frames before it
				}
				wantWrite("after the replug", numbers[i], sizes[i])
				written++
			}
			if all := node.all(); len(all) != written {
				t.Errorf("node writes = %+v, want %d in all", all, written)
			}
			want := "cv-a [" + describeShareSettings(tc.format, appconfig.CameraStampFrame) + "] (reading)"
			if tc.format == appconfig.CameraFormatH264 {
				want = "cv-a [format h264, stamp frame] (reading; " + sfSoftwareEncoderNote + ")"
			}
			if !csWaitFor(2*time.Second, func() bool { return h.reportOf(backAt)["share_state"] == want }) {
				t.Errorf("share_state = %q, want %q", h.reportOf(backAt)["share_state"], want)
			}
		})
	}
}

// The node is put over a returning camera, and its capture started, as soon as
// the kernel has made the camera's node, before udev has named it: the frame
// numbers the app reads go on all the same, on the port the camera left or on
// another.
func TestRecover_ANodeWithAFrameStampGoesOnNumberingBeforeUdevNamesTheCamera(t *testing.T) {
	onAnotherPort := csBrio()
	onAnotherPort.base, onAnotherPort.id.Device = "video2", csPort("1-2.2")
	for name, back := range map[string]csCamera{
		"back at its path, on its port":         csBrio(),
		"back at another path, on another port": onAnotherPort,
	} {
		t.Run(name, func(t *testing.T) {
			prev := shareRetryBackoff
			shareRetryBackoff = []time.Duration{5 * time.Millisecond, time.Hour}
			t.Cleanup(func() { shareRetryBackoff = prev }) // after the harness's Shutdown
			h := newCSHarness(t, csBrio())
			udev := h.udevNames()
			udev.name("/dev/video0", sfBrioByID)
			sfCanDeriveFrames(h.s)
			fp := installFakeProducers(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a",
				[]appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame)})
			if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
				t.Fatalf("errors: %v", errs)
			}
			h.watch()
			records := h.records("cv-a")
			if len(records) != 1 {
				t.Fatalf("records = %+v; logs: %v", records, h.logs.All())
			}
			node := records[0].io.(*csNode)
			// The frame numbers the app reads off its node for n camera frames.
			read := func(hub *deviceHub, n int) []uint64 {
				t.Helper()
				var numbers []uint64
				for range n {
					hub.produce(sfJPEGFrame(0, 1_000_000_000))
					e := node.next(t)
					if !e.stamp.holdsSampleID {
						t.Fatalf("the node got %+v, want a frame stamped with its number", e)
					}
					numbers = append(numbers, e.stamp.sampleID)
				}
				return numbers
			}

			node.send(t, 1)
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
				t.Fatalf("the camera did not start; logs: %v", h.logs.All())
			}
			if numbers := read(fp.hub(0), 2); !reflect.DeepEqual(numbers, []uint64{1, 2}) {
				t.Fatalf("frame numbers before the replug = %v, want [1 2]", numbers)
			}

			udev.forget("/dev/video0")
			h.unplug("video0")
			fp.hub(0).cancel() // its capture ends with the camera
			h.waitLogged("retrying in 1h0m0s", 1)

			backAt := "/dev/" + back.base
			h.plug(back)
			h.waitLogged("camera share: covered "+backAt+" for cv-a again in ", 1)
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 2 }) {
				t.Fatalf("the camera did not start again within 2 s of coming back; logs: %v", h.logs.All())
			}
			udev.name(backAt, sfBrioByID)
			if numbers := read(fp.hub(1), 2); !reflect.DeepEqual(numbers, []uint64{3, 4}) {
				t.Fatalf("frame numbers after the replug = %v, want [3 4]: they go on from the 2 before it", numbers)
			}
		})
	}
}

func TestCameraShareState_Version1LoadsAsPixelsWithCaptureTimes(t *testing.T) {
	h := newCSHarness(t, csBrio())
	stored := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 15}
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", stored, 254) + `]}}}`)
	h.s.loadCameraShares()

	records := h.records("cv-a")
	if len(records) != 1 {
		t.Fatalf("records = %d; logs: %v", len(records), h.logs.All())
	}
	stored.Format = appconfig.CameraFormatPixels
	if r := records[0]; r.mode != stored || r.stamp != appconfig.CameraStampTime {
		t.Fatalf("record holds %s, want %s", describeShareNode(r.mode, r.stamp), describeShareNode(stored, appconfig.CameraStampTime))
	}
}

// An agent that knows only version 1 ignores a version 2 file and leaves
// running apps' nodes without a writer, so the file is version 1, exactly as
// that agent wrote it, whenever no share needs more.
func TestCameraShareState_IsVersion2OnlyWhileSomeShareNeedsIt(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
	version1 := map[string]any{
		"version": float64(1),
		"containers": map[string]any{"cv-a": map[string]any{
			"app": "sh.wendy.cv-a",
			"shares": []any{map[string]any{
				"camera": map[string]any{"device": csPort("1-2.1"), "index": "0", "name": "Brio 101"},
				"path":   "/dev/video0",
				"mode": map[string]any{"width": float64(1280), "height": float64(720), "fourcc": "YUYV",
					"bytesPerLine": float64(2560), "fps": float64(30)},
				"node": float64(255),
			}},
		}},
	}
	if state := h.stateFile(); !reflect.DeepEqual(state, version1) {
		t.Fatalf("state file with only pixels/time shares = %+v\nwant the version 1 file %+v", state, version1)
	}

	for name, entitlement := range map[string]appconfig.Entitlement{
		"a format": sfCameraEntitlement(appconfig.CameraFormatH264, ""),
		"a stamp":  sfCameraEntitlement("", appconfig.CameraStampFrame),
	} {
		h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{entitlement})
		if v := h.stateFile()["version"]; v != float64(2) {
			t.Fatalf("%s: state file version = %v, want 2", name, v)
		}
		for id, wantFormat := range map[string]string{"cv-a": "pixels", "cv-b": h.records("cv-b")[0].mode.Format} {
			if sh := h.stateShares(id)[0]; sh["format"] != wantFormat || sh["stamp"] != h.records(id)[0].stamp {
				t.Fatalf("%s: version 2 share of %s = %v", name, id, sh)
			}
		}
		h.s.RemoveCameraShares("cv-b")
		h.drainRemovals()
		if state := h.stateFile(); !reflect.DeepEqual(state, version1) {
			t.Fatalf("%s: state file after the share that needed version 2 went = %+v\nwant %+v", name, state, version1)
		}
	}
}

func TestCameraShareState_Version2KeepsEachSharesSettingsAcrossAnAgentRestart(t *testing.T) {
	h := newCSHarness(t, csBrio(), csC920())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, "Brio 101"),
		sfCameraEntitlement(appconfig.CameraFormatH264, appconfig.CameraStampFrame, "C920"),
	})
	type settings struct {
		mode  sharedMode
		stamp string
	}
	before := map[string]settings{}
	for _, r := range h.records("cv-a") {
		before[r.cameraName] = settings{r.mode, r.stamp}
	}
	if want := (map[string]settings{"Brio 101": {sfNativeMode, "frame"}, "C920": {sfH264Mode, "frame"}}); !reflect.DeepEqual(before, want) {
		t.Fatalf("created %+v, want %+v", before, want)
	}

	// The agent restarts: its records come from the file, and the nodes are
	// still there.
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[int]string{}
	for _, r := range h.records("cv-a") {
		nodes[r.node] = r.cameraName
	}
	h.s.Shutdown()
	h = newCSHarness(t, csBrio(), csC920())
	sfCanDeriveFrames(h.s)
	fp := installFakeProducers(h.s)
	for nr, label := range nodes {
		h.loop.aux[nr] = label
	}
	h.writeState(string(state))
	h.s.loadCameraShares()
	after := map[string]settings{}
	for _, r := range h.records("cv-a") {
		after[r.cameraName] = settings{r.mode, r.stamp}
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("loaded %+v, want %+v", after, before)
	}

	// Adoption opens each node at its stored mode and serves it as stored,
	// whatever a share made today would get.
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a"}})
	for _, r := range h.records("cv-a") {
		want := before[r.cameraName]
		if r.pump == nil {
			t.Fatalf("%s was not adopted; logs: %v", r.cameraName, h.logs.All())
		}
		if r.pump.mode != want.mode || r.pump.stamp != want.stamp {
			t.Errorf("%s: adopted pump serves %s", r.cameraName, describeShareNode(r.pump.mode, r.pump.stamp))
		}
		if got := r.io.(*csNode).mode; got != want.mode {
			t.Errorf("%s: adopted node opened at %+v, want %+v", r.cameraName, got, want.mode)
		}
	}
	if n := h.logged("adopted /dev/video255 for Brio 101 at 1280x720 MJPG, 30 fps, format native, stamp frame"); n != 1 {
		t.Errorf("adoption line with the node's settings logged %d times; logs: %v", n, h.logs.All())
	}
	for _, r := range h.records("cv-a") {
		if r.cameraName != "C920" {
			continue
		}
		r.io.(*csNode).send(t, 1)
		if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
			t.Fatal("the adopted h264 node's camera did not start")
		}
		fp.hub(0).broadcast(sfH264Frame(41, true))
		if e := r.io.(*csNode).next(t); e.stamp != stampWithSampleID(41) {
			t.Errorf("adopted h264 node's buffer stamp = %+v, want sample ID 41", e.stamp)
		}
	}
}

func TestCameraShareState_AShareWithSettingsThisAgentDoesNotKnowIsDropped(t *testing.T) {
	share := func(format, stamp, fourcc string, maxFrameBytes int) string {
		return fmt.Sprintf(`{"camera":{"device":%q,"index":"0","name":"Brio 101"},"path":"/dev/video0",`+
			`"mode":{"width":1280,"height":720,"fourcc":%q,"bytesPerLine":0,"fps":30,"maxFrameBytes":%d},"format":%q,"stamp":%q,"node":254}`,
			csPort("1-2.1"), fourcc, maxFrameBytes, format, stamp)
	}
	for name, tc := range map[string]struct {
		record string
		kept   bool
	}{
		"native":                {share("native", "frame", "MJPG", maxFrameBytes), true},
		"unknown format":        {share("raw16", "time", "MJPG", maxFrameBytes), false},
		"unknown stamp":         {share("native", "sequence", "MJPG", maxFrameBytes), false},
		"no settings":           {share("", "", "MJPG", maxFrameBytes), false},
		"compressed, no limit":  {share("native", "time", "MJPG", 0), false},
		"codec no node carries": {share("native", "time", "VP80", maxFrameBytes), false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			h.loop.aux[254] = "Brio 101"
			h.writeState(`{"version":2,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` + tc.record + `]}}}`)
			h.s.loadCameraShares()
			if got := len(h.records("cv-a")) == 1; got != tc.kept {
				t.Fatalf("kept = %v, want %v; logs: %v", got, tc.kept, h.logs.All())
			}
			if !tc.kept && h.logged("this agent could not have made it") != 1 {
				t.Errorf("the dropped share was not logged: %v", h.logs.All())
			}
		})
	}
}

func TestCameraShares_AppsWithDifferentFormatsOnOneCameraAllGetFrames(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	fp := installFakeProducers(h.s)
	apps := []struct{ id, format, stamp string }{
		{"cv-a", appconfig.CameraFormatPixels, appconfig.CameraStampTime},
		{"cv-b", appconfig.CameraFormatNative, appconfig.CameraStampFrame},
		{"cv-c", appconfig.CameraFormatH264, appconfig.CameraStampFrame},
		{"cv-d", appconfig.CameraFormatPixels, appconfig.CameraStampFrame},
	}
	nodes := map[string]*csNode{}
	for _, app := range apps {
		h.s.CreateCameraShares(context.Background(), app.id, "sh.wendy."+app.id, []appconfig.Entitlement{sfCameraEntitlement(app.format, app.stamp)})
		records := h.records(app.id)
		if len(records) != 1 || records[0].mode.Format != app.format || records[0].stamp != app.stamp {
			t.Fatalf("%s: records = %+v; logs: %v", app.id, records, h.logs.All())
		}
		nodes[app.id] = records[0].io.(*csNode)
		nodes[app.id].send(t, 1)
	}
	if !csWaitFor(2*time.Second, func() bool {
		if fp.count() != 1 {
			return false
		}
		hub := fp.hub(0)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.subs) == len(apps)
	}) {
		t.Fatalf("the apps do not share one capture: %d producers; logs: %v", fp.count(), h.logs.All())
	}
	if got := hubParams(fp.hub(0)); got != [3]uint32{} {
		t.Fatalf("the camera runs at %v, want its defaults", got)
	}

	// One camera frame, as the hub delivers it on each plane.
	hub := fp.hub(0)
	jpeg, picture, accessUnit := sfJPEGFrame(7, 9_000_000_000), sfDerivedPicture(7, 9_000_000_000), sfH264Frame(7, true)
	// And the camera's next frame: each node holds it right after the first, so
	// no frame of another plane was written to it in between.
	nextJPEG, nextPicture, nextUnit := sfJPEGFrame(8, 9_100_000_000), sfDerivedPicture(8, 9_100_000_000), sfH264Frame(8, false)
	for _, f := range []*videoFrame{jpeg, picture, accessUnit, nextJPEG, nextPicture, nextUnit} {
		hub.broadcast(f)
	}
	for _, n := range []struct {
		id, node    string
		first, then *videoFrame
		stamps      [2]shareFrameStamp
	}{
		{"cv-a", "pixels node", picture, nextPicture, [2]shareFrameStamp{stampWithCaptureTime(9_000_000_000), stampWithCaptureTime(9_100_000_000)}},
		{"cv-b", "native node", jpeg, nextJPEG, [2]shareFrameStamp{stampWithSampleID(7), stampWithSampleID(8)}},
		{"cv-c", "h264 node", accessUnit, nextUnit, [2]shareFrameStamp{stampWithSampleID(7), stampWithSampleID(8)}},
		// The same pictures as cv-a's, which differs from this node in its stamp alone.
		{"cv-d", "pixels node with a frame stamp", picture, nextPicture, [2]shareFrameStamp{stampWithSampleID(7), stampWithSampleID(8)}},
	} {
		for i, f := range []*videoFrame{n.first, n.then} {
			if e := nodes[n.id].next(t); e.size != len(f.data) || e.stamp != n.stamps[i] {
				t.Errorf("%s: write %d = %+v, want the %d-byte frame of its own format stamped %+v", n.node, i+1, e, len(f.data), n.stamps[i])
			}
		}
		if all := nodes[n.id].all(); len(all) != 2 {
			t.Errorf("%s: node writes = %+v, want the two frames of its own format", n.node, all)
		}
	}

	// The camera's one shared mode, for viewers and recordings, is its size
	// and rate whatever each app's format.
	mode, sharers, ok := h.s.sharedModeOf("/dev/video0")
	if !ok || !reflect.DeepEqual(sharers, []string{"cv-a", "cv-b", "cv-c", "cv-d"}) {
		t.Fatalf("sharedModeOf = %v, %v", sharers, ok)
	}
	if _, err := h.s.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{Width: mode.Width, Height: mode.Height, Framerate: mode.FPS}); err != nil {
		t.Errorf("a viewer asking the shared size and rate was refused: %v", err)
	}
	if _, err := h.s.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}); err == nil {
		t.Error("a viewer asking another size was admitted")
	}
	want := "cv-a [format pixels, stamp time] (reading), cv-b [format native, stamp frame] (reading), cv-c [format h264, stamp frame] (reading; " + sfSoftwareEncoderNote + "), " +
		"cv-d [format pixels, stamp frame] (reading)"
	if got := h.reportOf("/dev/video0")["share_state"]; got != want {
		t.Errorf("share_state = %q\nwant         %q", got, want)
	}
}

func TestJoinHubForShare_TakesAViewersCameraOverForEveryNodeFormat(t *testing.T) {
	type refusal func(*deviceHub)
	refusals := map[string]refusal{
		"the viewer's hub takes the subscription": func(*deviceHub) {},
		"the viewer's hub has none of the node's frames": func(h *deviceHub) {
			h.rawNotOffered(fixedReason("no tap at this size"))
			h.nativeNotOffered(fixedReason("the camera offers no JPEG at 640x480"))
		},
	}
	for _, tc := range []struct {
		mode   sharedMode
		own    *videoFrame
		others []*videoFrame
	}{
		{sfNativeMode, sfJPEGFrame(1, 1), []*videoFrame{sfDerivedPicture(1, 1), sfH264Frame(1, true)}},
		{sfH264Mode, sfH264Frame(1, true), []*videoFrame{sfDerivedPicture(1, 1), sfJPEGFrame(1, 1)}},
		{sfPixelsMode, sfDerivedPicture(1, 1), []*videoFrame{sfJPEGFrame(1, 1), sfH264Frame(1, true)}},
	} {
		for name, refuse := range refusals {
			t.Run(tc.mode.Format+"/"+name, func(t *testing.T) {
				svc := newTestVideoService(nil, nil)
				fp := installFakeProducers(svc)
				ctx := context.Background()
				viewerReq := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480}
				oldHub, viewerID, _, err := svc.getOrCreateHub(ctx, "/dev/video0", viewerReq, hubHolderStreamClient)
				if err != nil {
					t.Fatal(err)
				}
				defer oldHub.unsubscribe(viewerID)
				refuse(oldHub)

				share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, tc.mode, noPreset)
				if err != nil {
					t.Fatalf("share join: %v", err)
				}
				defer share.Leave()
				newHub := currentHub(svc, "/dev/video0")
				if newHub == nil || newHub == oldHub || fp.count() != 2 || hubParams(newHub) != [3]uint32{} {
					t.Fatalf("the camera was not restarted at its defaults: %d producers", fp.count())
				}
				for _, f := range append(tc.others, tc.own) {
					newHub.broadcast(f)
				}
				select {
				case f := <-share.Frames():
					if f != tc.own {
						t.Fatalf("the share received a %s frame, want only its format's", describeNodeFrame(f))
					}
				case <-time.After(2 * time.Second):
					t.Fatal("the share received no frame of its format")
				}
			})
		}
	}
}

// A hub at the shared mode that has none of the node's frames is not taken
// over: restarting the camera the same way would change nothing.
func TestJoinHubForShare_ARefusalAtTheSharedModeIsReportedNotTakenOver(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx := context.Background()
	hub, id, _, err := svc.getOrCreateHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0}, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.unsubscribe(id)
	hub.nativeNotOffered(fixedReason("JPEG: the camera offers no JPEG at 1280x720"))

	_, err = svc.joinHubForShare(ctx, "/dev/video0", 0, sfNativeMode, noPreset)
	if !isNativeRefusal(err) || refusalText(err) != "native frames are not available for this camera: JPEG: the camera offers no JPEG at 1280x720" {
		t.Fatalf("share join: %v, want the hub's refusal", err)
	}
	if fp.count() != 1 || currentHub(svc, "/dev/video0") != hub {
		t.Fatalf("the camera was restarted (%d producers) for a refusal a restart cannot lift", fp.count())
	}
}

func TestSharePump_TimeStampIsTheKernelCaptureTimeWhenTheFrameHasOne(t *testing.T) {
	const nowMono, nowWall = int64(50_000_000_000), int64(1_700_000_000_000_000_000)
	clocks := func(p *sharePump) {
		p.nowMono = func() int64 { return nowMono }
		p.nowWall = func() int64 { return nowWall }
	}
	sub := newSpFakeSub()
	h := startSharePump(t, newSpJoiner(spSubResult(sub)), clocks)
	h.node.send(t, 1)

	derived := sfDerivedPicture(5, 49_990_000_000)
	derived.tsNs = uint64(nowWall - 40_000_000)
	sub.frames <- derived
	if e := h.node.next(t); e.stamp != stampWithCaptureTime(49_990_000_000) {
		t.Errorf("stamp = %+v, want the kernel capture time", e.stamp)
	}

	for name, f := range map[string]func(*videoFrame){
		"a tap picture":    func(*videoFrame) {},
		"agent wall clock": func(f *videoFrame) { f.nativeNs, f.nativeClock = nowWall-40_000_000, "CLOCK_REALTIME_AGENT_CAPTURE" },
		"unknown clock":    func(f *videoFrame) { f.nativeNs, f.nativeClock = 123, "V4L2_TIMESTAMP_UNKNOWN" },
		"no kernel time":   func(f *videoFrame) { f.nativeClock = v4l2MonotonicClock },
	} {
		frame := sfTapPicture(uint64(nowWall - 40_000_000))
		f(frame)
		sub.frames <- frame
		if e := h.node.next(t); e.stamp != stampWithCaptureTime(nowMono-40_000_000) {
			t.Errorf("%s: stamp = %+v, want the wall-clock time rebased", name, e.stamp)
		}
	}
}

const (
	sfNoFrameNumber = `the camera's frames carry no frame number for the "frame" stamp`
	sfNotWholeH264  = "the camera's H.264 is not arriving as whole frames, so there is none to put in a buffer"
)

// While the camera cannot serve the node the pump stays out of the hub: with
// no reader, with a reader, and at every recheck.
func TestSharePump_ACameraThatCannotServeTheNodeIsNotJoined(t *testing.T) {
	sfRecheckEvery(t, 2*time.Millisecond)
	verdict := &sfVerdict{reason: "no frame numbers here"}
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j, sfPumpAs(spMode, appconfig.CameraStampFrame), sfUnserved(verdict))
	cannot := shareState{shareCannotServe, "no frame numbers here"}

	h.expectState(t, cannot)
	h.node.send(t, 1)
	asked := verdict.timesAsked()
	if !csWaitFor(spWait, func() bool { return verdict.timesAsked() >= asked+5 }) {
		t.Fatal("the pump does not look again while an app reads")
	}
	if j.count() != 0 || h.pump.State() != cannot {
		t.Fatalf("joins = %d, state = %+v; want no join and %+v", j.count(), h.pump.State(), cannot)
	}
	if n := h.logged("camera share: app1: cam0 cannot serve its node (format pixels, stamp frame): no frame numbers here; it gets no frames until the camera can, checked every 2ms"); n != 1 {
		t.Errorf("the reason was logged %d times, want once; logs: %v", n, h.logs.All())
	}

	// The reader gives up: the state still says why it got nothing.
	h.node.send(t, 0)
	asked = verdict.timesAsked()
	if !csWaitFor(spWait, func() bool { return verdict.timesAsked() >= asked+5 }) {
		t.Fatal("the pump does not look again with no reader")
	}
	if j.count() != 0 || h.pump.State() != cannot {
		t.Fatalf("with no reader: joins = %d, state = %+v", j.count(), h.pump.State())
	}

	// The camera can serve the node now. With no reader that makes it idle;
	// with one, the pump joins and the first frame makes it reading.
	verdict.set("")
	h.expectState(t, shareState{Phase: shareIdle})
	if j.count() != 0 {
		t.Fatalf("joined %d times with no reader", j.count())
	}
	h.node.send(t, 1)
	j.waitJoin(t)
	sub.frames <- sfDerivedPicture(1234567890123, 1)
	if e := h.node.next(t); e.stamp != stampWithSampleID(1234567890123) {
		t.Errorf("stamp = %+v, want sample ID 1234567890123", e.stamp)
	}
	h.expectState(t, shareState{Phase: shareReading})
}

// Each way a capture shows, after the join, that it cannot serve the node:
// nothing is written, the pump leaves the hub, the state is "cannot serve"
// with the reason and never "reading" until a frame is written, and the next
// look comes on the slow timer, not the backoff.
func TestSharePump_ACaptureThatCannotServeTheNodeIsLeftAndSaysWhy(t *testing.T) {
	chunk := sfH264Frame(1, true)
	chunk.auAligned = false
	vp8 := &videoFrame{data: []byte{1, 2, 3}, codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, sampleID: 1}
	ownH264 := sfH264Frame(1, true)
	ownH264.native = true
	refusal := errNativeUnavailable("JPEG: the camera offers no JPEG at 1280x720")
	const refused = "native frames are not available for this camera: JPEG: the camera offers no JPEG at 1280x720"

	cases := map[string]struct {
		mode   sharedMode
		stamp  string
		shows  func(sub *spFakeSub)
		reason string
		serves *videoFrame
		stamps shareFrameStamp
	}{
		"a picture with no frame number on a frame stamp node": {
			spMode, appconfig.CameraStampFrame,
			func(sub *spFakeSub) { sub.frames <- sfTapPicture(uint64(time.Now().UnixNano())) },
			sfNoFrameNumber, sfDerivedPicture(9, 1), stampWithSampleID(9),
		},
		"H.264 arriving in chunks": {
			sfH264Mode, appconfig.CameraStampTime,
			func(sub *spFakeSub) { sub.frames <- chunk },
			sfNotWholeH264, sfH264Frame(9, true), shareFrameStamp{},
		},
		"VP8 on an h264 node": {
			sfH264Mode, appconfig.CameraStampTime,
			func(sub *spFakeSub) { sub.frames <- vp8 },
			fmt.Sprintf(nodeModeNotDelivered, "VP8", "1280x720 H264, 30 fps"), sfH264Frame(9, true), shareFrameStamp{},
		},
		"the camera's own H.264 on a JPEG node": {
			sfNativeMode, appconfig.CameraStampFrame,
			func(sub *spFakeSub) { sub.frames <- ownH264 },
			fmt.Sprintf(nodeModeNotDelivered, "H.264", "1280x720 MJPG, 30 fps"), sfJPEGFrame(9, 1), stampWithSampleID(9),
		},
		"the producer turns native subscribers away": {
			sfNativeMode, appconfig.CameraStampFrame,
			func(sub *spFakeSub) { sub.err = refusal; close(sub.frames) },
			refused, sfJPEGFrame(9, 1), stampWithSampleID(9),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sfRecheckEvery(t, time.Hour)
			first, second, third := newSpFakeSub(), newSpFakeSub(), newSpFakeSub()
			j := newSpJoiner(spSubResult(first), spSubResult(second), spSubResult(third))
			h := startSharePump(t, j, sfPumpAs(tc.mode, tc.stamp))
			cannot := shareState{shareCannotServe, tc.reason}

			h.node.send(t, 1)
			j.waitJoin(t)
			h.expectState(t, shareState{Phase: shareReading})
			tc.shows(first)
			spWaitClosed(t, first.left, "the pump to leave the hub")
			h.expectState(t, cannot)

			// The backoff here is 2 and 4 ms; the recheck is an hour.
			phases := h.phasesSeen()
			time.Sleep(40 * time.Millisecond)
			if j.count() != 1 {
				t.Fatalf("joined %d times: a camera that cannot serve the node is retried on the backoff", j.count())
			}

			// A look that joins and finds the same holds the same state.
			h.pump.Nudge()
			j.waitJoin(t)
			tc.shows(second)
			spWaitClosed(t, second.left, "the pump to leave the hub again")
			h.pump.Nudge()
			j.waitJoin(t)
			time.Sleep(5 * time.Millisecond)
			if seen := phases(); len(seen) != 1 || !seen[shareCannotServe] || h.pump.State() != cannot {
				t.Fatalf("states seen while the camera could not serve the node = %v, now %+v; want only %+v", seen, h.pump.State(), cannot)
			}
			if n := h.logged("cannot serve its node"); n != 1 {
				t.Errorf("the reason was logged %d times, want once", n)
			}

			third.frames <- tc.serves
			e := h.node.next(t)
			if e.size != len(tc.serves.data) || e.stamp.holdsSampleID != tc.stamps.holdsSampleID || e.stamp.sampleID != tc.stamps.sampleID {
				t.Fatalf("the first write = %+v, want the %d-byte frame the camera can now serve", e, len(tc.serves.data))
			}
			h.expectState(t, shareState{Phase: shareReading})
			if all := h.node.all(); len(all) != 1 {
				t.Errorf("node writes = %+v, want only that frame", all)
			}
		})
	}
}

func TestSharePump_ARefusalOfNativeFramesAtTheJoinIsNotRetriedOnTheBackoff(t *testing.T) {
	sfRecheckEvery(t, time.Hour)
	refusal := errNativeUnavailable("JPEG: the camera offers no JPEG at 1280x720")
	served := newSpFakeSub()
	j := newSpJoiner(spErrResult(refusal), spSubResult(served))
	h := startSharePump(t, j, sfPumpAs(sfNativeMode, appconfig.CameraStampTime))

	h.node.send(t, 1)
	j.waitJoin(t)
	h.expectState(t, shareState{shareCannotServe, "native frames are not available for this camera: JPEG: the camera offers no JPEG at 1280x720"})
	time.Sleep(40 * time.Millisecond)
	if j.count() != 1 || h.logged("retrying in") != 0 {
		t.Fatalf("joins = %d, backoff lines = %d; want the slow recheck only", j.count(), h.logged("retrying in"))
	}

	h.pump.Nudge()
	j.waitJoin(t)
	served.frames <- sfJPEGFrame(9, 77_000_000_000)
	if e := h.node.next(t); e.stamp != stampWithCaptureTime(77_000_000_000) || e.size != len(sfJPEGFrame(9, 0).data) {
		t.Fatalf("write = %+v, want the camera's own frame at its kernel capture time", e)
	}
	h.expectState(t, shareState{Phase: shareReading})
}

// An ordinary failure after a camera could not serve the node is retried on
// the backoff again, and shows as itself.
func TestSharePump_AnOrdinaryFailureAfterCannotServeUsesTheBackoff(t *testing.T) {
	sfRecheckEvery(t, time.Hour)
	first, last := newSpFakeSub(), newSpFakeSub()
	j := newSpJoiner(spSubResult(first), spErrResult(errors.New("boom")), spSubResult(last))
	h := startSharePump(t, j, sfPumpAs(spMode, appconfig.CameraStampFrame))

	h.node.send(t, 1)
	j.waitJoin(t)
	first.frames <- sfTapPicture(1)
	h.expectState(t, shareState{shareCannotServe, sfNoFrameNumber})
	h.pump.Nudge()
	j.waitJoin(t)
	j.waitJoin(t) // after the backoff, with no nudge
	if n := h.logged("joining cam0 failed while the app reads: boom; retrying in"); n != 1 {
		t.Fatalf("backoff lines = %d, want 1; logs: %v", n, h.logs.All())
	}
	h.expectState(t, shareState{Phase: shareReading})
}

func TestSharePump_H264StartsOnARandomAccessFrameAndWritesWholeAccessUnits(t *testing.T) {
	sub := newSpFakeSub()
	h := startSharePump(t, newSpJoiner(spSubResult(sub)), sfPumpAs(sfH264Mode, appconfig.CameraStampFrame))
	h.node.send(t, 1)

	sub.frames <- sfH264Frame(1, false)
	sub.frames <- sfH264Frame(2, false)
	idr, inter := sfH264Frame(3, true), sfH264Frame(4, false)
	sub.frames <- idr
	sub.frames <- inter
	if e := h.node.next(t); e.stamp != stampWithSampleID(3) || e.size != len(idr.data) {
		t.Fatalf("first write = %+v, want the random-access frame 3 whole", e)
	}
	if e := h.node.next(t); e.stamp != stampWithSampleID(4) || e.size != len(inter.data) {
		t.Fatalf("second write = %+v, want frame 4 whole", e)
	}
	h.expectState(t, shareState{Phase: shareReading})

	// A frame that does not reach the app breaks the chain: the next written
	// is again one the app can decode alone.
	h.node.setWriteErr(fmt.Errorf("share node: %w", errShareFrameTooLarge))
	sub.frames <- sfH264Frame(5, false)
	if s := h.waitState(t, shareNoFrames); !strings.Contains(s.Detail, errShareFrameTooLarge.Error()) {
		t.Fatalf("Detail = %q, want the dropped frame's reason", s.Detail)
	}
	h.node.setWriteErr(nil)
	sub.frames <- sfH264Frame(6, false)
	sub.frames <- sfH264Frame(7, true)
	if e := h.node.next(t); e.stamp != stampWithSampleID(7) {
		t.Fatalf("write after a dropped frame = %+v, want the next random-access frame, 7", e)
	}
	h.expectState(t, shareState{Phase: shareReading})
}

// A jump in sample IDs is a frame the pump never got (the hub dropped it for a
// slow pump, or none was made from that camera frame): what follows cannot be
// decoded until the next random-access frame.
func TestSharePump_H264SkipsToARandomAccessFrameAfterAGapInSampleIDs(t *testing.T) {
	sub := newSpFakeSub()
	h := startSharePump(t, newSpJoiner(spSubResult(sub)), sfPumpAs(sfH264Mode, appconfig.CameraStampFrame))
	h.node.send(t, 1)

	for _, f := range []*videoFrame{
		sfH264Frame(10, true), sfH264Frame(11, false),
		sfH264Frame(13, false), sfH264Frame(14, false), // 12 never came
		sfH264Frame(15, true), sfH264Frame(16, false),
		sfH264Frame(18, true), // 17 never came, and this one stands alone
		sfH264Frame(19, false),
	} {
		sub.frames <- f
	}
	var written []uint64
	for range 6 {
		written = append(written, h.node.next(t).stamp.sampleID)
	}
	if want := []uint64{10, 11, 15, 16, 18, 19}; !reflect.DeepEqual(written, want) {
		t.Fatalf("written sample IDs = %v, want %v", written, want)
	}
	h.noWriteWithin(t, 20*time.Millisecond)
	h.expectState(t, shareState{Phase: shareReading})
}

// The camera's own frames each stand alone, so a gap costs only the frames
// that were lost.
func TestSharePump_AGapInSampleIDsCostsANativeNodeNothingMore(t *testing.T) {
	sub := newSpFakeSub()
	h := startSharePump(t, newSpJoiner(spSubResult(sub)), sfPumpAs(sfNativeMode, appconfig.CameraStampFrame))
	h.node.send(t, 1)
	for _, id := range []uint64{4, 5, 9, 10} {
		sub.frames <- sfJPEGFrame(id, 1)
	}
	var written []uint64
	for range 4 {
		written = append(written, h.node.next(t).stamp.sampleID)
	}
	if want := []uint64{4, 5, 9, 10}; !reflect.DeepEqual(written, want) {
		t.Fatalf("written sample IDs = %v, want %v", written, want)
	}
}

// sfWriter is a node writer over memory, with its queue ioctl recorded.
func sfWriter(mode sharedMode) (w *shareNodeWriter, queued *[]v4l2Buf) {
	queued = &[]v4l2Buf{}
	w = &shareNodeWriter{mode: mode, path: "/dev/video255", fd: -1, wakeR: -1, wakeW: -1, done: closedChan(),
		buffers: [][]byte{make([]byte, mode.bufferBytes()), make([]byte, mode.bufferBytes())}}
	w.ioctlFn = func(req uintptr, arg unsafe.Pointer) error {
		if req == vidiocQbuf {
			*queued = append(*queued, *(*v4l2Buf)(arg))
		}
		return nil
	}
	return w, queued
}

func sfBufferTimeval(b *v4l2Buf) (seconds, microseconds int64) {
	return *(*int64)(unsafe.Pointer(&b[24])), *(*int64)(unsafe.Pointer(&b[32]))
}

// The module sizes a compressed format's image itself (width x height x 4 in
// 0.15.4), so the node's maximum frame is whatever the kernel reports.
func TestShareNodeWriter_ACompressedNodeTakesItsMaximumFrameSizeFromTheKernel(t *testing.T) {
	stopAtBuffers := errors.New("stop before the buffers")
	for name, tc := range map[string]struct {
		mode      sharedMode
		taken     func(*v4l2Format)
		wantMax   uint32
		wantError string
	}{
		"jpeg, as asked":                 {mode: sfNativeMode, wantMax: maxFrameBytes},
		"h264, as asked":                 {mode: sfH264Mode, wantMax: maxFrameBytes},
		"kernel sizes the image larger":  {mode: sfNativeMode, taken: func(f *v4l2Format) { f.SizeImage = 1280 * 720 * 4 }, wantMax: 1280 * 720 * 4},
		"kernel sizes the image smaller": {mode: sfH264Mode, taken: func(f *v4l2Format) { f.SizeImage = 320 * 240 * 4 }, wantMax: 320 * 240 * 4},
		"kernel reports no image size":   {mode: sfNativeMode, taken: func(f *v4l2Format) { f.SizeImage = 0 }, wantError: "instead of 1280x720 MJPG, 30 fps"},
		"kernel takes another format":    {mode: sfH264Mode, taken: func(f *v4l2Format) { f.PixelFormat = v4l2PixFmtMJPEG }, wantError: "instead of 1280x720 H264, 30 fps"},
	} {
		t.Run(name, func(t *testing.T) {
			var asked v4l2Format
			w := &shareNodeWriter{mode: tc.mode, path: "/dev/video255", fd: -1, wakeR: -1, wakeW: -1, done: closedChan()}
			queued := 0
			w.ioctlFn = func(req uintptr, arg unsafe.Pointer) error {
				switch req {
				case vidiocSFmt:
					asked = *(*v4l2Format)(arg)
					if tc.taken != nil {
						tc.taken((*v4l2Format)(arg))
					}
				case vidiocReqbufs:
					return stopAtBuffers
				case vidiocQbuf:
					queued++
				}
				return nil
			}
			err := w.configure()
			if asked.Type != v4l2BufTypeVideoOutput || asked.Width != 1280 || asked.Height != 720 ||
				asked.PixelFormat != tc.mode.pixelFormat() || asked.BytesPerLine != 0 || asked.SizeImage != maxFrameBytes {
				t.Errorf("declared %dx%d %s, %d bytes per line, %d bytes; want 1280x720 %s, 0, %d",
					asked.Width, asked.Height, fourccName(asked.PixelFormat), asked.BytesPerLine, asked.SizeImage, tc.mode.Fourcc, maxFrameBytes)
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Errorf("error = %v, want one saying %q", err, tc.wantError)
				}
				return
			}
			if !errors.Is(err, stopAtBuffers) {
				t.Fatalf("the mode the node took was refused: %v", err)
			}
			want := tc.mode
			want.MaxFrameBytes = tc.wantMax
			if got := w.Mode(); got != want || got.bufferBytes() != int(tc.wantMax) {
				t.Fatalf("the writer's mode = %+v with %d-byte buffers, want %+v", got, got.bufferBytes(), want)
			}

			// The stand-in buffers are as large as the kernel's would be.
			w.buffers = [][]byte{make([]byte, tc.wantMax), make([]byte, tc.wantMax)}
			if err := w.WriteFrame(make([]byte, tc.wantMax), stampWithCaptureTime(1)); err != nil || queued != 1 {
				t.Errorf("a frame of the kernel's maximum, %d bytes: %v, %d queued", tc.wantMax, err, queued)
			}
			err = w.WriteFrame(make([]byte, tc.wantMax+1), stampWithCaptureTime(2))
			if !errors.Is(err, errShareFrameTooLarge) || w.oversizedFrames() != 1 || queued != 1 {
				t.Errorf("a frame one byte over the kernel's maximum: error %v, counted %d, %d queued", err, w.oversizedFrames(), queued)
			}
		})
	}
}

func TestShareNodeWriter_APictureNodeStillNeedsItsExactLayout(t *testing.T) {
	for name, taken := range map[string]func(*v4l2Format){
		"another image size":     func(f *v4l2Format) { f.SizeImage = 1280 * 720 * 4 },
		"another bytes per line": func(f *v4l2Format) { f.BytesPerLine = 2564 },
	} {
		w := &shareNodeWriter{mode: sfPixelsMode, path: "/dev/video255", fd: -1, wakeR: -1, wakeW: -1}
		w.ioctlFn = func(req uintptr, arg unsafe.Pointer) error {
			if req == vidiocSFmt {
				taken((*v4l2Format)(arg))
			}
			return nil
		}
		if err := w.configure(); err == nil || !strings.Contains(err.Error(), "instead of 1280x720 YUYV, 30 fps") {
			t.Errorf("%s: error = %v, want the layout refused", name, err)
		}
		if w.Mode() != sfPixelsMode {
			t.Errorf("%s: the writer's mode changed to %+v", name, w.Mode())
		}
	}
}

func TestShareNodeWriter_CompressedFramesAreWrittenAtTheirOwnLength(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: 16}
	w, queued := sfWriter(mode)

	for i, frame := range [][]byte{{1, 2, 3}, []byte("0123456789abcdef"), {9}} {
		if err := w.WriteFrame(frame, stampWithCaptureTime(int64(i+1)*1_000_000_000)); err != nil {
			t.Fatalf("frame of %d bytes: %v", len(frame), err)
		}
		q := (*queued)[i]
		if q.bytesUsed() != uint32(len(frame)) || q.index() != uint32(i%2) || string(w.buffers[i%2][:len(frame)]) != string(frame) {
			t.Errorf("frame %d queued as %d bytes in buffer %d, want its %d bytes in buffer %d", i, q.bytesUsed(), q.index(), len(frame), i%2)
		}
	}

	for dropped := uint64(1); dropped <= 2; dropped++ {
		err := w.WriteFrame(make([]byte, 17), stampWithCaptureTime(1))
		if !errors.Is(err, errShareFrameTooLarge) || w.oversizedFrames() != dropped {
			t.Fatalf("oversize frame: error %v, counted %d, want errShareFrameTooLarge and %d", err, w.oversizedFrames(), dropped)
		}
	}
	if err := w.WriteFrame(nil, stampWithCaptureTime(1)); err == nil {
		t.Error("an empty frame was accepted")
	}
	if len(*queued) != 3 {
		t.Errorf("%d buffers queued, want only the 3 frames that fit", len(*queued))
	}
	if err := w.WriteFrame([]byte{7}, stampWithCaptureTime(1)); err != nil {
		t.Errorf("a frame that fits after dropped ones: %v", err)
	}
}

func TestShareNodeWriter_PicturesStillNeedTheirExactSize(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "YUYV", BytesPerLine: 8, FPS: 30, Format: appconfig.CameraFormatPixels}
	w, queued := sfWriter(mode)
	for _, size := range []int{0, 15, 17} {
		if err := w.WriteFrame(make([]byte, size), stampWithCaptureTime(1)); err == nil || errors.Is(err, errShareFrameTooLarge) {
			t.Errorf("a %d-byte picture: error %v, want the exact-size refusal", size, err)
		}
	}
	if err := w.WriteFrame(make([]byte, 16), stampWithCaptureTime(1)); err != nil {
		t.Fatal(err)
	}
	if len(*queued) != 1 || (*queued)[0].bytesUsed() != 16 || w.oversizedFrames() != 0 {
		t.Errorf("queued %d buffers, counted %d oversize", len(*queued), w.oversizedFrames())
	}
}

func TestShareNodeWriter_BufferTimestampHoldsTheCaptureTimeOrTheSampleID(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "MJPG", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: 16}
	w, queued := sfWriter(mode)

	if err := w.WriteFrame([]byte{1}, stampWithCaptureTime(12_345_678_901_234)); err != nil {
		t.Fatal(err)
	}
	if sec, usec := sfBufferTimeval(&(*queued)[0]); sec != 12_345 || usec != 678_901 {
		t.Errorf("capture time stamped as %d s %d µs, want 12345 s 678901 µs", sec, usec)
	}

	// A reader recovers the ID as seconds*1e6 + microseconds.
	for id, want := range map[uint64][2]int64{
		1:                  {0, 1},
		999_999:            {0, 999_999},
		1_000_000:          {1, 0},
		1_234_567_890_123:  {1_234_567, 890_123},
		92_233_720_368_547: {92_233_720, 368_547},
	} {
		if err := w.WriteFrame([]byte{1}, stampWithSampleID(id)); err != nil {
			t.Fatalf("sample ID %d: %v", id, err)
		}
		last := (*queued)[len(*queued)-1]
		if sec, usec := sfBufferTimeval(&last); sec != want[0] || usec != want[1] {
			t.Errorf("sample ID %d stamped as %d s %d µs, want %d s %d µs", id, sec, usec, want[0], want[1])
		}
		if last.flags()&v4l2BufFlagTimestampCopy == 0 {
			t.Errorf("sample ID %d: the buffer does not say its timestamp is the writer's", id)
		}
	}

	before := len(*queued)
	if err := w.WriteFrame([]byte{1}, stampWithSampleID(0)); !errors.Is(err, errShareFrameWithoutSampleID) {
		t.Errorf("sample ID 0: error %v, want errShareFrameWithoutSampleID", err)
	}
	if len(*queued) != before {
		t.Error("a frame with sample ID 0 was queued")
	}
}

// A share's record holds the maximum frame size its node's kernel gave, and
// that size is not a difference between the record and the mode the policy
// decides: the container created again keeps its node, and an adopted record
// is not told to redeploy.
func TestCreateCameraShares_ACompressedSharesRecordHoldsTheKernelsMaximumFrameSize(t *testing.T) {
	for name, kernelBytes := range map[string]uint32{"smaller than asked": 1280 * 720, "larger than asked": 1280 * 720 * 4} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			sfCanDeriveFrames(h.s)
			h.kernelImageBytes = func(sharedMode) uint32 { return kernelBytes }
			entitlements := []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")}
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)

			want := sfNativeMode
			want.MaxFrameBytes = kernelBytes
			records := h.records("cv-a")
			if len(records) != 1 || records[0].mode != want || records[0].pump.mode != want {
				t.Fatalf("records = %+v, want one at %+v; logs: %v", records, want, h.logs.All())
			}
			if asked := records[0].io.(*csNode).mode; asked != sfNativeMode {
				t.Errorf("the node was opened in %+v, want what the policy decided, %+v", asked, sfNativeMode)
			}
			if mode := h.stateShares("cv-a")[0]["mode"].(map[string]any); mode["maxFrameBytes"] != float64(kernelBytes) {
				t.Errorf("state file mode = %v, want maxFrameBytes %d", mode, kernelBytes)
			}

			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
			if again := h.records("cv-a"); len(again) != 1 || again[0] != records[0] || len(h.openedNodes()) != 1 {
				t.Errorf("the container created again got another node: %d records, %d nodes opened", len(again), len(h.openedNodes()))
			}
			h.s.checkAdoptedMode(context.Background(), "cv-a", records[0])
			if note := h.reportOf("/dev/video0")["share_state"]; strings.Contains(note, "redeploy") {
				t.Errorf("the kernel's frame size is reported as a changed mode: %q", note)
			}
		})
	}
}

func TestSharedMode_AsksWhat(t *testing.T) {
	kernelSized := sfNativeMode
	kernelSized.MaxFrameBytes = 1280 * 720 * 4
	otherSize := sfNativeMode
	otherSize.Width = 640
	for name, tc := range map[string]struct {
		a, b sharedMode
		want bool
	}{
		"the same compressed mode":               {sfNativeMode, sfNativeMode, true},
		"a compressed mode at the kernel's size": {sfNativeMode, kernelSized, true},
		"another size":                           {sfNativeMode, otherSize, false},
		"another format":                         {sfNativeMode, sfH264Mode, false},
		"another format in the same codec":       {hcNativeMode, hcH264Mode, false},
		"pictures and a compressed mode":         {sfPixelsMode, kernelSized, false},
		"the same pictures":                      {sfPixelsMode, sfPixelsMode, true},
	} {
		if got := tc.a.asksWhat(tc.b); got != tc.want || tc.b.asksWhat(tc.a) != tc.want {
			t.Errorf("%s: asksWhat = %v, want %v", name, got, tc.want)
		}
	}
}

const sfBrioByID = "usb-Logitech_Brio_101_2331AP04K2S8-video-index0"

// sfNameBrioByID gives the camera at /dev/video0 the stable id a camera
// listing reports for it, "by-id:" and sfBrioByID.
func sfNameBrioByID(h *csHarness) {
	h.s.readStableNames = func() map[string]stableNames {
		return map[string]stableNames{"/dev/video0": {byID: sfBrioByID, byPath: "platform-usb1-usb-0:2.1:1.0-video-index0"}}
	}
}

func sfWarnings(h *csHarness, message string) int {
	n := 0
	for _, e := range h.logs.All() {
		if e.Message == message && e.Level.String() == "warn" {
			n++
		}
	}
	return n
}

func TestCreateCameraShares_AnEntryNamingACameraByItsStableIDOrCampaignPathDecidesItsNode(t *testing.T) {
	for _, entry := range []string{"by-id:" + sfBrioByID, sfBrioByID, "v4l2:/dev/video0"} {
		t.Run(entry, func(t *testing.T) {
			h := newCSHarness(t, csBrio(), csC920())
			sfCanDeriveFrames(h.s)
			sfNameBrioByID(h)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
				sfCameraEntitlement(appconfig.CameraFormatNative, appconfig.CameraStampFrame, entry),
			})

			want := map[string]string{
				"Brio 101": describeShareNode(sfNativeMode, appconfig.CameraStampFrame),
				"C920":     describeShareNode(sfPixelsMode, appconfig.CameraStampTime),
			}
			records := h.records("cv-a")
			if len(records) != len(want) {
				t.Fatalf("records = %d, want %d; logs: %v", len(records), len(want), h.logs.All())
			}
			for _, r := range records {
				if got := describeShareNode(r.mode, r.stamp); got != want[r.cameraName] || r.io.(*csNode).mode != r.mode || r.askedFormat != "" {
					t.Errorf("%s: record holds %s (asked %q) and its node declares %+v, want %s",
						r.cameraName, got, r.askedFormat, r.io.(*csNode).mode, want[r.cameraName])
				}
			}
			row := h.reportOf("/dev/video0")
			if got, want := row["share_state"], "cv-a [format native, stamp frame] (idle)"; got != want {
				t.Errorf("share_state = %q, want %q", got, want)
			}
			if got, want := row["stable_id"], "by-id:"+sfBrioByID; got != want {
				t.Errorf("the camera's row gives stable_id %q, want %q", got, want)
			}
			if id, said := h.reportOf("/dev/video4")["stable_id"]; !said || id != "" {
				t.Errorf("a camera without a stable id gives stable_id %q (present %v), want it present and empty: the key says the agent matches stable ids", id, said)
			}
		})
	}
}

// Two entries naming one camera by different spellings pass validation, which
// compares the allowlist strings, so the agent says what they cost the app.
func TestCreateCameraShares_EntriesThatDisagreeOnACameraAreWarnedOfOnceAndSaidInItsState(t *testing.T) {
	for name, tc := range map[string]struct {
		entitlements []appconfig.Entitlement
		mode         sharedMode
		said         string
	}{
		"two spellings of the path": {
			[]appconfig.Entitlement{
				sfCameraEntitlement(appconfig.CameraFormatNative, "", "/dev/video0"),
				sfCameraEntitlement(appconfig.CameraFormatH264, "", "v4l2:/dev/video0"),
			},
			sfPixelsMode,
			`format falls back to pixels: the entries deciding for this camera disagree (allowlist entry "/dev/video0" asks for native, allowlist entry "v4l2:/dev/video0" asks for h264)`,
		},
		"the stable id and the path": {
			[]appconfig.Entitlement{
				sfCameraEntitlement(appconfig.CameraFormatH264, "", "/dev/video0"),
				sfCameraEntitlement(appconfig.CameraFormatNative, "", sfBrioByID),
			},
			sfNativeMode,
			`format is native (asked by allowlist entry "` + sfBrioByID + `"); passed over: allowlist entry "/dev/video0" asks for h264`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio(), csC920())
			sfCanDeriveFrames(h.s)
			sfNameBrioByID(h)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", tc.entitlements)

			records := h.records("cv-a")
			if len(records) != 2 {
				t.Fatalf("records = %d, want a node for each camera; logs: %v", len(records), h.logs.All())
			}
			if r := records[0]; r.cameraName != "Brio 101" || r.mode != tc.mode || r.stamp != appconfig.CameraStampTime {
				t.Errorf("%s: record holds %s, want %s", r.cameraName, describeShareNode(r.mode, r.stamp), describeShareNode(tc.mode, appconfig.CameraStampTime))
			}
			if n := sfWarnings(h, "camera share: cv-a: Brio 101 (/dev/video0): "+tc.said); n != 1 {
				t.Errorf("warned %d times of the disagreement, want once; logs: %v", n, h.logs.All())
			}
			if n := h.logged("the entries deciding") + h.logged("passed over"); n != 1 {
				t.Errorf("%d lines tell of a disagreement, want the one for the camera the entries name; logs: %v", n, h.logs.All())
			}
			wantState := "cv-a [format " + tc.mode.Format + ", stamp time] (idle; " + tc.said + ")"
			if got := h.reportOf("/dev/video0")["share_state"]; got != wantState {
				t.Errorf("share_state = %q\nwant         %q", got, wantState)
			}
			if got, want := h.reportOf("/dev/video4")["share_state"], "cv-a [format pixels, stamp time] (idle)"; got != want {
				t.Errorf("the other camera's share_state = %q, want %q", got, want)
			}
			if sh := h.stateShares("cv-a")[0]; sh["settingsNote"] != tc.said {
				t.Errorf("state file share = %v, want settingsNote %q", sh, tc.said)
			}
		})
	}
}

func TestCreateCameraShares_ADisagreementOfEntriesSurvivesAnAgentRestartInTheState(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement("", appconfig.CameraStampFrame, "/dev/video0"),
		sfCameraEntitlement("", "", "v4l2:/dev/video0"),
	})
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	h.s.Shutdown()
	h = newCSHarness(t, csBrio())
	h.loop.aux[255] = "Brio 101"
	h.writeState(string(state))
	h.s.loadCameraShares()
	h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a"}})
	want := `cv-a [format pixels, stamp time] (idle; stamp falls back to time: the entries deciding for this camera disagree ` +
		`(allowlist entry "/dev/video0" asks for frame, allowlist entry "v4l2:/dev/video0" asks for time))`
	if got := h.reportOf("/dev/video0")["share_state"]; got != want {
		t.Errorf("share_state = %q\nwant         %q", got, want)
	}
}

// The node a disagreement left at the defaults is the node entries that agree
// on the defaults ask for, so it is kept, and its state stops telling of the
// disagreement.
func TestCreateCameraShares_AContainerCreatedAgainWithEntriesThatAgreeNoLongerTellsOfADisagreement(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, "", "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "v4l2:/dev/video0"),
	})
	kept := h.records("cv-a")[0]
	if _, told := h.stateShares("cv-a")[0]["settingsNote"]; !told {
		t.Fatalf("the disagreement is not in the state file: %v", h.stateShares("cv-a"))
	}

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement("", "", "v4l2:/dev/video0")})
	if records := h.records("cv-a"); len(records) != 1 || records[0] != kept {
		t.Fatalf("a create with the same settings made the share again: %+v", records)
	}
	if got, want := h.reportOf("/dev/video0")["share_state"], "cv-a [format pixels, stamp time] (idle)"; got != want {
		t.Errorf("share_state = %q, want %q", got, want)
	}
	if note, told := h.stateShares("cv-a")[0]["settingsNote"]; told {
		t.Errorf("the state file still tells of the disagreement: %q", note)
	}
	if n := h.logged("the entries deciding"); n != 1 {
		t.Errorf("%d warnings of a disagreement, want only the first create's; logs: %v", n, h.logs.All())
	}
}

// A deploy removes a container's shares and creates them again, so each deploy
// of a manifest whose entries disagree warns once. A create that finds its
// record again with the same note, as within one deploy, adds no warning.
func TestCreateCameraShares_ADisagreementIsWarnedOfOncePerDeploy(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	disagreeing := []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, "", "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "v4l2:/dev/video0"),
	}
	for deploy := 1; deploy <= 3; deploy++ {
		h.s.RemoveCameraShares("cv-a")
		h.drainRemovals()
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", disagreeing)
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", disagreeing)
		if n := h.logged("the entries deciding"); n != deploy {
			t.Fatalf("after deploy %d the disagreement was warned of %d times, want once per deploy; logs: %v", deploy, n, h.logs.All())
		}
	}
	if _, told := h.stateShares("cv-a")[0]["settingsNote"]; !told {
		t.Errorf("the disagreement left the state file: %v", h.stateShares("cv-a"))
	}

	disagreeing[1].Stamp = appconfig.CameraStampFrame
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", disagreeing)
	if n := h.logged("stamp falls back to time"); n != 1 {
		t.Errorf("a disagreement that changed without a remove warned %d times, want once; logs: %v", n, h.logs.All())
	}
}

// A create that does not see a record's camera cannot say what its entries
// make of it, so the record stops telling of the last create's disagreement.
func TestCreateCameraShares_ACorrectedManifestCreatedWhileTheCameraIsAwayDropsTheDisagreement(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, "", "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "v4l2:/dev/video0"),
	})
	kept := h.records("cv-a")[0]
	if _, told := h.stateShares("cv-a")[0]["settingsNote"]; !told {
		t.Fatalf("the disagreement is not in the state file: %v", h.stateShares("cv-a"))
	}

	h.takeOut("video0")
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement("", "", "/dev/video0")})
	h.mu.Lock()
	h.cams["video0"] = csBrio()
	h.mu.Unlock()

	if records := h.records("cv-a"); len(records) != 1 || records[0] != kept {
		t.Fatalf("records = %+v, want the record made before the camera went", records)
	}
	if got, want := h.reportOf("/dev/video0")["share_state"], "cv-a [format pixels, stamp time] (idle)"; got != want {
		t.Errorf("share_state after the camera came back = %q, want %q", got, want)
	}
	if note, told := h.stateShares("cv-a")[0]["settingsNote"]; told {
		t.Errorf("the state file still tells of the disagreement: %q", note)
	}
}

// A network camera is read through its own node, not through a share node, so
// its row carries the stable id an allowlist entry may hold and no share mode.
func TestCameraShareReport_ANetworkCamerasRowGivesItsStableIDAndNoShareMode(t *testing.T) {
	h := newCSHarness(t, csBrio())
	dir := t.TempDir()
	h.s.registry = ipcam.NewRegistry(filepath.Join(dir, "cameras.json"))
	h.s.credentials = ipcam.NewCredentialStore(filepath.Join(dir, "credentials.json"))
	cam, err := h.s.registry.Upsert(ipcam.Camera{MAC: "aa:00:00:00:00:01", Model: "RLC-520A", Address: "10.98.0.50"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin"}); err != nil {
		t.Fatal(err)
	}
	h.loop.mu.Lock()
	h.loop.nodePaths[cam.ID] = "/dev/video200"
	h.loop.mu.Unlock()

	want := map[string]string{"stable_id": "ip-mac:aa:00:00:00:00:01"}
	if got := h.reportOf("/dev/video200"); !reflect.DeepEqual(got, want) {
		t.Errorf("the network camera's row = %v, want %v", got, want)
	}
}

// The note follows the manifest of the last create for every camera that
// create resolved settings for, also one it then could not share.
func TestCreateCameraShares_ACameraACreateSeesButCannotShareStillTakesItsEntriesDisagreement(t *testing.T) {
	h := newCSHarness(t, csBrio())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement("", "", "/dev/video0")})
	kept := h.records("cv-a")[0]

	unreadable := csBrio()
	unreadable.formats = nil
	h.mu.Lock()
	h.cams["video0"] = unreadable
	h.mu.Unlock()
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, "", "/dev/video0"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "v4l2:/dev/video0"),
	})

	if records := h.records("cv-a"); len(records) != 1 || records[0] != kept {
		t.Fatalf("records = %+v, want the record made while the camera could be shared; logs: %v", records, h.logs.All())
	}
	const said = `format falls back to pixels: the entries deciding for this camera disagree (allowlist entry "/dev/video0" asks for native, allowlist entry "v4l2:/dev/video0" asks for h264)`
	if note := h.stateShares("cv-a")[0]["settingsNote"]; note != said {
		t.Errorf("state file settingsNote = %q, want %q", note, said)
	}
	if n := sfWarnings(h, "camera share: cv-a: Brio 101 (/dev/video0): "+said); n != 1 {
		t.Errorf("warned %d times of the disagreement, want once; logs: %v", n, h.logs.All())
	}
}

// A create that cannot get as far as the cameras has still replaced the
// manifest, so what the earlier one's entries disagreed on is no longer said.
func TestCreateCameraShares_ACreateThatStopsBeforeTheCamerasDropsTheEarlierManifestsDisagreement(t *testing.T) {
	for name, stop := range map[string]func(*csHarness){
		"no loopback module": func(h *csHarness) { h.loop.availableErr = errors.New("no module") },
		"listing cameras fails": func(h *csHarness) {
			h.s.globDevices = func() ([]string, error) { return nil, errors.New("glob failed") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			sfCanDeriveFrames(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{
				sfCameraEntitlement(appconfig.CameraFormatNative, "", "/dev/video0"),
				sfCameraEntitlement(appconfig.CameraFormatH264, "", "v4l2:/dev/video0"),
			})
			if _, told := h.stateShares("cv-a")[0]["settingsNote"]; !told {
				t.Fatalf("the disagreement is not in the state file: %v", h.stateShares("cv-a"))
			}

			stop(h)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement("", "", "/dev/video0")})
			if h.logged("cv-a gets the real cameras") != 1 {
				t.Fatalf("the second create did not stop before the cameras; logs: %v", h.logs.All())
			}
			if records := h.records("cv-a"); len(records) != 1 || records[0].settingsNote != "" {
				t.Errorf("the record still tells of the disagreement: %+v", records)
			}
			if note, told := h.stateShares("cv-a")[0]["settingsNote"]; told {
				t.Errorf("the state file still tells of the disagreement: %q", note)
			}
		})
	}
}

const (
	sfSoftwareEncoderNote    = "fed by the software encoder x264enc"
	sfSoftwareEncoderIdle    = sfSoftwareEncoderNote + " when the app reads, at preset ultrafast or the fastest one a consumer of the camera states"
	sfSoftwareEncoderWarning = `camera share: cv-a: C920 (/dev/video4): its h264 node is fed by the software encoder x264enc, at preset ultrafast or the fastest one a consumer of the camera states, for as long as the app reads it; "format": "native" or "pixels" run no encoder`
)

// An h264 node of a webcam is filled by an encoder this device runs. Where
// that encoder is software the node's state says so, and its create warns
// once. The h264 node of a camera that produces H.264 carries the camera's own
// frames and says nothing, and neither does a node of another format on the
// webcam. The state file holds none of it: what the node says is asked of the
// device each time.
func TestCreateCameraShares_AnH264NodeFedByASoftwareEncoderSaysSoAndIsWarnedOfOnce(t *testing.T) {
	h := newCSHarness(t, csBrio(), csH264Cam(), csC920())
	hcCanDecode(h.s, "avdec_h264", "jpegdec", "x264enc")
	entitlements := []appconfig.Entitlement{
		sfCameraEntitlement(appconfig.CameraFormatNative, "", "Brio 101"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "Logi H264"),
		sfCameraEntitlement(appconfig.CameraFormatH264, "", "C920"),
	}
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)

	if records := h.records("cv-a"); len(records) != 3 {
		t.Fatalf("records = %d, want a node for each camera; logs: %v", len(records), h.logs.All())
	}
	for path, state := range map[string]string{
		"/dev/video0": "cv-a [format native, stamp time] (idle)",
		"/dev/video2": "cv-a [format h264, stamp time] (idle)",
		"/dev/video4": "cv-a [format h264, stamp time] (idle; " + sfSoftwareEncoderIdle + ")",
	} {
		if got := h.reportOf(path)["share_state"]; got != state {
			t.Errorf("%s: share_state = %q\nwant %q", path, got, state)
		}
	}
	if state, err := os.ReadFile(cameraSharesPath); err != nil || strings.Contains(string(state), "ncoder") {
		t.Errorf("the state file tells of an encoder (read error %v): %s", err, state)
	}
	if n := sfWarnings(h, sfSoftwareEncoderWarning); n != 1 {
		t.Errorf("warned %d times of the software encoder, want once; logs: %v", n, h.logs.All())
	}
	if n := h.logged("software encoder"); n != 1 {
		t.Errorf("%d lines tell of a software encoder, want the one for the webcam's h264 node; logs: %v", n, h.logs.All())
	}

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
	if n := h.logged("software encoder"); n != 1 {
		t.Errorf("a create that found its node again warned of the software encoder: %d lines, want 1; logs: %v", n, h.logs.All())
	}
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", entitlements)
	if n := sfWarnings(h, sfSoftwareEncoderWarning); n != 2 {
		t.Errorf("after a second deploy the software encoder was warned of %d times, want once per deploy; logs: %v", n, h.logs.All())
	}
}

func TestCreateCameraShares_AnH264NodeFedByAHardwareEncoderSaysNothingOfIt(t *testing.T) {
	h := newCSHarness(t, csC920())
	hcCanDecode(h.s, "jpegdec", "v4l2h264enc", "x264enc")
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatH264, "")})
	if records := h.records("cv-a"); len(records) != 1 || records[0].mode != sfH264Mode {
		t.Fatalf("records = %+v, want one h264 node; logs: %v", records, h.logs.All())
	}
	if got, want := h.reportOf("/dev/video4")["share_state"], "cv-a [format h264, stamp time] (idle)"; got != want {
		t.Errorf("share_state = %q, want %q", got, want)
	}
	if n := h.logged("encoder"); n != 0 {
		t.Errorf("%d lines tell of an encoder; logs: %v", n, h.logs.All())
	}
}

// What an h264 node says of its encoder is the device's answer when the state
// is reported, not what held when the node was made: a share adopted from the
// state file gains the note on a device that encodes in software, whichever
// agent wrote the file, and has none once a hardware encoder is installed.
func TestCameraShareReport_AnAdoptedH264NodeSaysOfItsEncoderWhatHoldsNow(t *testing.T) {
	h := newCSHarness(t, csC920())
	sfCanDeriveFrames(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatH264, "")})
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	h.s.Shutdown()
	for name, tc := range map[string]struct {
		elements []string
		want     string
	}{
		"the device encodes in software":   {[]string{"jpegdec", "x264enc"}, "cv-a [format h264, stamp time] (idle; " + sfSoftwareEncoderIdle + ")"},
		"a hardware encoder was installed": {[]string{"jpegdec", "v4l2h264enc", "x264enc"}, "cv-a [format h264, stamp time] (idle)"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csC920())
			hcCanDecode(h.s, tc.elements...)
			h.loop.aux[255] = "C920"
			h.writeState(string(state))
			h.s.loadCameraShares()
			h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a"}})
			if got := h.reportOf("/dev/video4")["share_state"]; got != tc.want {
				t.Errorf("share_state = %q\nwant         %q", got, tc.want)
			}
			if n := h.logged("software encoder"); n != 0 {
				t.Errorf("adopting the share warned of its encoder: %d lines; logs: %v", n, h.logs.All())
			}
		})
	}
}

// The note is said only where the software encoder feeds the node or will
// when the app reads: not of an app that holds the real camera, and not while
// the node gets no frames.
func TestCameraShareReport_AnH264NodeNamesItsSoftwareEncoderOnlyWhileItIsReadOrIdle(t *testing.T) {
	h := newCSHarness(t, csC920())
	sfCanDeriveFrames(h.s)
	h264 := []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatH264, "")}
	states := map[string]func(r *shareRecord){
		"cv-cannot":  func(r *shareRecord) { r.pump = &sharePump{state: shareState{Phase: shareCannotServe, Detail: "why"}} },
		"cv-cover":   func(r *shareRecord) { r.coverFailed = "no namespace" },
		"cv-idle":    func(r *shareRecord) { r.pump = &sharePump{state: shareState{Phase: shareIdle}} },
		"cv-noframe": func(r *shareRecord) { r.pump = &sharePump{state: shareState{Phase: shareNoFrames, Detail: "why"}} },
		"cv-reading": func(r *shareRecord) { r.pump = &sharePump{state: shareState{Phase: shareReading}} },
		"cv-took":    func(r *shareRecord) { r.tookRealCamera = "python3" },
		"cv-unread":  func(*shareRecord) {},
		"cv-waiting": func(r *shareRecord) { r.pump = &sharePump{state: shareState{Phase: shareWaiting, Detail: "why"}} },
	}
	for id := range states {
		h.s.CreateCameraShares(context.Background(), id, "sh.wendy."+id, h264)
	}
	state, err := os.ReadFile(cameraSharesPath)
	if err != nil {
		t.Fatal(err)
	}
	h.s.Shutdown()
	h = newCSHarness(t, csC920())
	sfCanDeriveFrames(h.s)
	h.writeState(string(state))
	h.s.loadCameraShares()
	h.s.shares.mu.Lock()
	for id, set := range states {
		set(h.s.shares.containers[id].shares[0])
	}
	h.s.shares.mu.Unlock()

	const settings = " [format h264, stamp time] "
	want := strings.Join([]string{
		"cv-cannot" + settings + "(cannot serve: why)",
		"cv-cover" + settings + "(not sharing: its start's cover failed: no namespace)",
		"cv-idle" + settings + "(idle; " + sfSoftwareEncoderIdle + ")",
		"cv-noframe" + settings + "(no frames: why)",
		"cv-reading" + settings + "(reading; " + sfSoftwareEncoderNote + ")",
		"cv-took" + settings + "(not sharing: took the real camera after a replug)",
		"cv-unread" + settings + "(idle; " + sfSoftwareEncoderIdle + ")",
		"cv-waiting" + settings + "(waiting: why)",
	}, ", ")
	if got := h.reportOf("/dev/video4")["share_state"]; got != want {
		t.Errorf("share_state = %q\nwant         %q", got, want)
	}
}
