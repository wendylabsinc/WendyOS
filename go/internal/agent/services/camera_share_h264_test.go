package services

// Tests for a camera that produces H.264 itself: its own frames fill native
// and h264 nodes, pictures decoded from them fill pixel nodes, and where no
// picture can be made the node serves the camera's H.264 and says so.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var (
	hcPixelsMode = sharedMode{1280, 720, "YUYV", 2560, 30, "pixels", 0}
	hcNativeMode = sharedMode{Width: 1280, Height: 720, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes}
	hcH264Mode   = sharedMode{Width: 1280, Height: 720, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatH264, MaxFrameBytes: maxFrameBytes}
)

const (
	hcNoDecoder = "pictures cannot be decoded from its frames on this device: GStreamer lacks a decoder " +
		"(nvv4l2decoder with nvvidconv, v4l2h264dec, avdec_h264, openh264dec)"
	hcNoGStreamerParts = "pictures cannot be decoded from its frames on this device: GStreamer lacks gdpdepay, gdppay, h264parse, a decoder " +
		"(nvv4l2decoder with nvvidconv, v4l2h264dec, avdec_h264, openh264dec)"
)

// hcCanDecode gives the service a GStreamer that frames H.264 and has these
// decoder elements.
func hcCanDecode(s *VideoService, decoderElements ...string) {
	available := map[string]bool{"gdpdepay": true, "gdppay": true, "h264parse": true}
	for _, element := range decoderElements {
		available[element] = true
	}
	s.gstMemo.mu.Lock()
	s.gstMemo.available = available
	s.gstMemo.mu.Unlock()
}

func hcService(t *testing.T, decoderElements ...string) *VideoService {
	modes := [][2]uint32{{640, 480}, {1280, 720}, {1920, 1080}}
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtH264},
		map[uint32][][2]uint32{v4l2PixFmtYUYV: {{640, 480}}, v4l2PixFmtH264: modes}, []uint32{15, 30})
	hcCanDecode(s, decoderElements...)
	return s
}

// hcOwnFrame is one H.264 access unit as the camera's native capture hands it
// on.
func hcOwnFrame(sampleID uint64, randomAccess bool, kernelNs int64) *videoFrame {
	f := sfH264Frame(sampleID, randomAccess)
	f.native, f.nativeNs, f.nativeClock = true, kernelNs, v4l2MonotonicClock
	return f
}

func TestShareDecision_H264CameraWithADecoderIsDeclaredInEveryFormat(t *testing.T) {
	s := hcService(t, "avdec_h264")
	for format, want := range map[string]sharedMode{
		appconfig.CameraFormatPixels: hcPixelsMode,
		appconfig.CameraFormatNative: hcNativeMode,
		appconfig.CameraFormatH264:   hcH264Mode,
	} {
		d := s.shareDecisionFor(context.Background(), usbCam(), format)
		if d != (shareDecision{Shared: true, Mode: want}) {
			t.Errorf("%s: decision = %+v, want %+v as asked", format, d, want)
		}
		if d.Mode.pixelFormat() == 0 || d.Mode.bufferBytes() <= 0 {
			t.Errorf("%s: no node can declare %+v", format, d.Mode)
		}
	}
	if want := (rateQuery{v4l2PixFmtH264, 1280, 720}); latestRateQuery() != want {
		t.Errorf("rate query = %+v, want the H.264 capture's size, %+v", latestRateQuery(), want)
	}
	if hcNativeMode.pixelFormat() != v4l2PixFmtH264 || hcNativeMode.bufferBytes() != maxFrameBytes {
		t.Errorf("the native node declares %#x with %d-byte buffers", hcNativeMode.pixelFormat(), hcNativeMode.bufferBytes())
	}
}

func TestShareDecision_H264CameraWithoutPicturesServesItsOwnFramesAndSaysWhy(t *testing.T) {
	cases := map[string]struct {
		service func(t *testing.T) *VideoService
		native  sharedMode
		whyNot  string
	}{
		"no decoder": {func(t *testing.T) *VideoService { return hcService(t) }, hcNativeMode, hcNoDecoder},
		"the decoder lacks the element that hands its pictures on": {
			func(t *testing.T) *VideoService { return hcService(t, "nvv4l2decoder") }, hcNativeMode, hcNoDecoder,
		},
		"a GStreamer that cannot frame H.264": {
			func(t *testing.T) *VideoService {
				s := hcService(t)
				s.gstMemo.available = map[string]bool{"jpegdec": true}
				return s
			}, hcNativeMode, hcNoGStreamerParts,
		},
		"a picture over the raw frame limit": {
			func(t *testing.T) *VideoService {
				s := hcService(t, "avdec_h264")
				captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return 1920, 1080 }
				return s
			},
			sharedMode{Width: 1920, Height: 1080, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes},
			"a 1920x1080 picture exceeds the 4128768-byte raw frame limit",
		},
		"decoding failed recently": {
			func(t *testing.T) *VideoService {
				s := hcService(t, "avdec_h264")
				s.derivedFailures.remember(usbCam().GetPath(), "the decoder gave no picture")
				return s
			}, hcNativeMode, "decoding this camera's frames failed recently: the decoder gave no picture",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := tc.service(t)
			pixels := s.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatPixels)
			want := shareDecision{Shared: true, Mode: tc.native, AskedFormat: appconfig.CameraFormatPixels, AskedWhyNot: tc.whyNot}
			if pixels != want {
				t.Errorf("pixels: decision = %+v\nwant               %+v", pixels, want)
			}
			if alone := s.shareDecisionsFor(usbCam(), appconfig.CameraFormatPixels)[appconfig.CameraFormatPixels]; alone.Shared || alone.Reason != tc.whyNot {
				t.Errorf("pixels on their own: %+v, want the refusal %q", alone, tc.whyNot)
			}
			if got := describeServedInstead(pixels.AskedFormat, pixels.Mode.Format, pixels.AskedWhyNot); got != "asked for pixels; serving native because "+tc.whyNot {
				t.Errorf("report = %q", got)
			}
			for _, format := range []string{appconfig.CameraFormatNative, appconfig.CameraFormatH264} {
				asked := tc.native
				asked.Format = format
				if d := s.shareDecisionFor(context.Background(), usbCam(), format); d != (shareDecision{Shared: true, Mode: asked}) {
					t.Errorf("%s: decision = %+v, want %+v as asked", format, d, asked)
				}
			}
		})
	}
}

func TestShareDecision_H264CameraIsDecodedByTheFirstDecoderPresent(t *testing.T) {
	everyDecoder := []string{"nvv4l2decoder", "nvvidconv", "v4l2h264dec", "avdec_h264", "openh264dec"}
	for _, tc := range []struct {
		missing []string
		want    string
	}{
		{nil, "h264parse ! nvv4l2decoder ! nvvidconv ! video/x-raw"},
		{[]string{"nvvidconv"}, "h264parse ! v4l2h264dec"},
		{[]string{"nvv4l2decoder"}, "h264parse ! v4l2h264dec"},
		{[]string{"nvv4l2decoder", "v4l2h264dec"}, "h264parse ! avdec_h264 thread-type=slice"},
		{[]string{"nvv4l2decoder", "v4l2h264dec", "avdec_h264"}, "h264parse ! openh264dec"},
	} {
		available := map[string]bool{"gdpdepay": true, "gdppay": true, "h264parse": true}
		for _, element := range everyDecoder {
			available[element] = true
		}
		for _, element := range tc.missing {
			delete(available, element)
		}
		decoder, err := derivedLayerDecoder(derivedTransforms[agentpb.VideoCodec_VIDEO_CODEC_H264], available)
		if err != nil || decoder.stages != tc.want {
			t.Errorf("without %v: decode stages = %q, %v; want %q", tc.missing, decoder.stages, err, tc.want)
		}
	}
}

// What cannot be declared in any format stays the real camera: the native
// capture takes this camera, and nothing says at what size before it runs.
func TestShareDecision_H264CameraWithNoListedSizeStaysUnshared(t *testing.T) {
	s := hcService(t, "avdec_h264")
	captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return 0, 0 }
	want := shareDecision{Reason: "the camera's own frames cannot be declared before they are captured: " +
		"H.264: the camera lists no H.264 frame size a capture without one chooses, so its size is known only once it runs"}
	for _, format := range appconfig.CameraFormats {
		if d := s.shareDecisionFor(context.Background(), usbCam(), format); d != want {
			t.Errorf("%s: decision = %+v, want %+v", format, d, want)
		}
	}
}

func TestCreateCameraShares_AnH264CamerasNodeFollowsItsEntitlement(t *testing.T) {
	type node struct {
		mode        sharedMode
		kind        frameKind
		askedFormat string
	}
	for name, tc := range map[string]struct {
		decoder []string
		want    map[string]node
	}{
		"with a decoder": {[]string{"avdec_h264"}, map[string]node{
			appconfig.CameraFormatPixels: {hcPixelsMode, pixelFrames, ""},
			appconfig.CameraFormatNative: {hcNativeMode, nativeFrames, ""},
			appconfig.CameraFormatH264:   {hcH264Mode, encodedFrames, ""},
		}},
		"without a decoder": {nil, map[string]node{
			appconfig.CameraFormatPixels: {hcNativeMode, nativeFrames, appconfig.CameraFormatPixels},
			appconfig.CameraFormatNative: {hcNativeMode, nativeFrames, ""},
			appconfig.CameraFormatH264:   {hcH264Mode, encodedFrames, ""},
		}},
	} {
		for format, want := range tc.want {
			for _, stamp := range appconfig.CameraStamps {
				t.Run(name+"/"+format+"/"+stamp, func(t *testing.T) {
					h := newCSHarness(t, csH264Cam())
					hcCanDecode(h.s, tc.decoder...)
					fp := installFakeProducers(h.s)
					h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(format, stamp)})

					records := h.records("cv-a")
					if len(records) != 1 {
						t.Fatalf("records = %d, want the camera's node; logs: %v", len(records), h.logs.All())
					}
					r := records[0]
					if r.mode != want.mode || r.stamp != stamp || r.askedFormat != want.askedFormat {
						t.Fatalf("record holds %s (asked %q), want %s", describeShareNode(r.mode, r.stamp), r.askedFormat, describeShareNode(want.mode, stamp))
					}
					if got := r.io.(*csNode).mode; got != want.mode {
						t.Errorf("the node declares %+v, want %+v", got, want.mode)
					}
					row := h.reportOf("/dev/video2")
					state := "cv-a [" + describeShareSettings(want.mode.Format, stamp) + "] (idle)"
					if want.askedFormat != "" {
						said := "asked for pixels; serving native because " + hcNoDecoder
						state = "cv-a [" + describeShareSettings(want.mode.Format, stamp) + "] (idle; " + said + ")"
						if n := h.logged("camera share: cv-a: Logi H264 (/dev/video2): " + said); n != 1 {
							t.Errorf("said %d times that the camera's own frames are served instead; logs: %v", n, h.logs.All())
						}
						if sh := h.stateShares("cv-a")[0]; sh["askedFormat"] != want.askedFormat || sh["askedWhyNot"] != hcNoDecoder {
							t.Errorf("state file share = %v", sh)
						}
					}
					wantPixels := map[bool][2]string{true: {"1280x720 YUYV, 30 fps", ""}, false: {"", hcNoDecoder}}[len(tc.decoder) > 0]
					if row["share_mode"] != wantPixels[0] || row["share_unavailable_pixels"] != wantPixels[1] || row["share_unavailable"] != "" {
						t.Errorf("the camera's row = %v, want pictures at %q or unavailable because %q", row, wantPixels[0], wantPixels[1])
					}
					if row["share_state"] != state || row["share_mode_native"] != "1280x720 H264, 30 fps" || row["share_mode_h264"] != "1280x720 H264, 30 fps" {
						t.Errorf("the camera's row = %v\nwant share_state %q", row, state)
					}

					r.io.(*csNode).send(t, 1)
					if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
						t.Fatalf("the camera did not start; logs: %v", h.logs.All())
					}
					hub := fp.hub(0)
					hub.mu.Lock()
					var kinds []frameKind
					for _, sub := range hub.subs {
						kinds = append(kinds, sub.kind)
					}
					hub.mu.Unlock()
					if !reflect.DeepEqual(kinds, []frameKind{want.kind}) {
						t.Fatalf("hub subscriptions = %v, want one of kind %v", kinds, want.kind)
					}

					const kernelNs = 7_000_000_000
					frame := hcOwnFrame(41, true, kernelNs)
					if want.mode.Format == appconfig.CameraFormatPixels {
						frame = sfDerivedPicture(41, kernelNs)
					}
					wantStamp := stampWithCaptureTime(kernelNs)
					if stamp == appconfig.CameraStampFrame {
						wantStamp = stampWithSampleID(41)
					}
					hub.broadcast(frame)
					if e := r.io.(*csNode).next(t); e.size != len(frame.data) || e.stamp != wantStamp {
						t.Errorf("the node got %+v, want the %d-byte frame stamped %+v", e, len(frame.data), wantStamp)
					}
				})
			}
		}
	}
}

func TestSharePump_AnH264CamerasOwnFramesStartOnARandomAccessFrameWithEitherStamp(t *testing.T) {
	for _, mode := range []sharedMode{hcNativeMode, hcH264Mode} {
		for _, stamp := range appconfig.CameraStamps {
			t.Run(mode.Format+"/"+stamp, func(t *testing.T) {
				sub := newSpFakeSub()
				h := startSharePump(t, newSpJoiner(spSubResult(sub)), sfPumpAs(mode, stamp))
				h.node.send(t, 1)

				kernelNs := func(sampleID uint64) int64 { return int64(sampleID) * 33_000_000 }
				frames := map[uint64]*videoFrame{}
				for _, f := range []struct {
					sampleID     uint64
					randomAccess bool
				}{{1, false}, {2, true}, {3, false}, {5, false}, {6, true}, {7, false}} {
					frames[f.sampleID] = hcOwnFrame(f.sampleID, f.randomAccess, kernelNs(f.sampleID))
					sub.frames <- frames[f.sampleID]
				}
				for _, sampleID := range []uint64{2, 3, 6, 7} {
					wantStamp := stampWithCaptureTime(kernelNs(sampleID))
					if stamp == appconfig.CameraStampFrame {
						wantStamp = stampWithSampleID(sampleID)
					}
					if e := h.node.next(t); e.stamp != wantStamp || e.size != len(frames[sampleID].data) {
						t.Fatalf("wrote %+v, want frame %d whole, stamped %+v", e, sampleID, wantStamp)
					}
				}
				h.noWriteWithin(t, 20*time.Millisecond)
				h.expectState(t, shareState{Phase: shareReading})
			})
		}
	}
}

func TestShareNodeWriter_AnH264CamerasNodeDeclaresH264AtItsCaptureSize(t *testing.T) {
	for _, mode := range []sharedMode{hcNativeMode, hcH264Mode} {
		w, queued := sfWriter(mode)
		unit := hcOwnFrame(9, true, 1)
		for i, stamp := range []shareFrameStamp{stampWithCaptureTime(12_000_000_000), stampWithSampleID(9)} {
			if err := w.WriteFrame(unit.data, stamp); err != nil {
				t.Fatalf("%s: %v", mode.Format, err)
			}
			if got := (*queued)[i].bytesUsed(); int(got) != len(unit.data) {
				t.Errorf("%s: buffer holds %d bytes, want the access unit's %d", mode.Format, got, len(unit.data))
			}
		}
		if sec, usec := sfBufferTimeval(&(*queued)[0]); sec != 12 || usec != 0 {
			t.Errorf("%s: capture time stamped as %d s %d µs", mode.Format, sec, usec)
		}
		if sec, usec := sfBufferTimeval(&(*queued)[1]); sec != 0 || usec != 9 {
			t.Errorf("%s: sample ID stamped as %d s %d µs", mode.Format, sec, usec)
		}
		if mode.pixelFormat() != v4l2PixFmtH264 || !strings.Contains(mode.String(), "1280x720 H264, 30 fps") {
			t.Errorf("%s: node declares %s as %#x", mode.Format, mode, mode.pixelFormat())
		}
	}
}
