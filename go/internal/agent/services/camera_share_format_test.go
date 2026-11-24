package services

// Tests for a camera node that follows its app's entitlement: the format
// chooses the node's declared mode and the hub subscription that fills it, and
// the stamp chooses what each buffer's timestamp field holds.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

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
				s.derivedFailures.remember(usbCam().GetPath(), "the encoder gave no frame")
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
	}
	nodes := map[string]*csNode{}
	for _, app := range apps {
		h.s.CreateCameraShares(context.Background(), app.id, "sh.wendy."+app.id, []appconfig.Entitlement{sfCameraEntitlement(app.format, app.stamp)})
		records := h.records(app.id)
		if len(records) != 1 || records[0].mode.Format != app.format {
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
	for _, f := range []*videoFrame{jpeg, picture, accessUnit} {
		hub.broadcast(f)
	}
	if e := nodes["cv-a"].next(t); e.size != len(picture.data) || e.stamp != stampWithCaptureTime(9_000_000_000) {
		t.Errorf("pixels node got %+v, want the %d-byte picture with its kernel capture time", e, len(picture.data))
	}
	if e := nodes["cv-b"].next(t); e.size != len(jpeg.data) || e.stamp != stampWithSampleID(7) {
		t.Errorf("native node got %+v, want the %d-byte JPEG stamped with sample ID 7", e, len(jpeg.data))
	}
	if e := nodes["cv-c"].next(t); e.size != len(accessUnit.data) || e.stamp != stampWithSampleID(7) {
		t.Errorf("h264 node got %+v, want the %d-byte access unit stamped with sample ID 7", e, len(accessUnit.data))
	}

	// The camera's one shared mode, for viewers and recordings, is its size
	// and rate whatever each app's format.
	mode, sharers, ok := h.s.sharedModeOf("/dev/video0")
	if !ok || !reflect.DeepEqual(sharers, []string{"cv-a", "cv-b", "cv-c"}) {
		t.Fatalf("sharedModeOf = %v, %v", sharers, ok)
	}
	if _, err := h.s.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{Width: mode.Width, Height: mode.Height, Framerate: mode.FPS}); err != nil {
		t.Errorf("a viewer asking the shared size and rate was refused: %v", err)
	}
	if _, err := h.s.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}); err == nil {
		t.Error("a viewer asking another size was admitted")
	}
	want := "cv-a [format pixels, stamp time] (reading), cv-b [format native, stamp frame] (reading), cv-c [format h264, stamp frame] (reading)"
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

				share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, tc.mode)
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

	_, err = svc.joinHubForShare(ctx, "/dev/video0", 0, sfNativeMode)
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
			"camera frames are VP8, the node declares 1280x720 H264, 30 fps", sfH264Frame(9, true), shareFrameStamp{},
		},
		"the camera's own H.264 on a JPEG node": {
			sfNativeMode, appconfig.CameraStampFrame,
			func(sub *spFakeSub) { sub.frames <- ownH264 },
			"camera frames are H.264, the node declares 1280x720 MJPG, 30 fps", sfJPEGFrame(9, 1), stampWithSampleID(9),
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
		"pictures and a compressed mode":         {sfPixelsMode, kernelSized, false},
		"the same pictures":                      {sfPixelsMode, sfPixelsMode, true},
	} {
		if got := tc.a.asksWhat(tc.b); got != tc.want || tc.b.asksWhat(tc.a) != tc.want {
			t.Errorf("%s: asksWhat = %v, want %v", name, got, tc.want)
		}
	}
}
