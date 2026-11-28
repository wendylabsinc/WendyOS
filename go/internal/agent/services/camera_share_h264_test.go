package services

// Tests for a camera that produces H.264 itself: its own frames fill native
// and h264 nodes, pictures decoded from them fill pixel nodes, and where no
// picture can be made the node serves the camera's H.264 and says so.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
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

// hcLargeOnlyService describes a camera whose H.264 sizes are all above the
// default capture limit: an Elgato Facecam Pro lists these, largest first.
func hcLargeOnlyService(t *testing.T, decoderElements ...string) *VideoService {
	const nv12 = 0x3231564E
	large := [][2]uint32{{3840, 2160}, {1920, 1080}}
	s := shareTestService(t, []uint32{v4l2PixFmtH264, v4l2PixFmtMJPEG, nv12},
		map[uint32][][2]uint32{v4l2PixFmtH264: large, v4l2PixFmtMJPEG: large}, []uint32{15, 30})
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

// hcPresetIgnoredFor is what the decision for a node of format says of a
// preset on a camera that produces H.264: its h264 node carries the camera's
// own frames, so no encoder is there to take one.
func hcPresetIgnoredFor(format string) string {
	if format == appconfig.CameraFormatH264 {
		return presetIgnoredOwnH264
	}
	return ""
}

func TestShareDecision_H264CameraWithADecoderIsDeclaredInEveryFormat(t *testing.T) {
	s := hcService(t, "avdec_h264")
	for format, want := range map[string]sharedMode{
		appconfig.CameraFormatPixels: hcPixelsMode,
		appconfig.CameraFormatNative: hcNativeMode,
		appconfig.CameraFormatH264:   hcH264Mode,
	} {
		d := s.shareDecisionFor(context.Background(), usbCam(), format)
		if d != (shareDecision{Shared: true, Mode: want, PresetIgnored: hcPresetIgnoredFor(format)}) {
			t.Errorf("%s: decision = %+v, want %+v as asked", format, d, want)
		}
		wantPixelFormat, wantBufferBytes := uint32(v4l2PixFmtH264), maxFrameBytes
		if format == appconfig.CameraFormatPixels {
			wantPixelFormat, wantBufferBytes = v4l2PixFmtYUYV, 1280*2*720
		}
		if d.Mode.pixelFormat() != wantPixelFormat || d.Mode.bufferBytes() != wantBufferBytes {
			t.Errorf("%s: the decided mode declares %#x with %d-byte buffers, want %#x with %d", format,
				d.Mode.pixelFormat(), d.Mode.bufferBytes(), wantPixelFormat, wantBufferBytes)
		}
	}
	if want := (rateQuery{v4l2PixFmtH264, 1280, 720}); latestRateQuery() != want {
		t.Errorf("rate query = %+v, want the H.264 capture's size, %+v", latestRateQuery(), want)
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
		"a picture over the picture limit": {
			func(t *testing.T) *VideoService {
				s := hcService(t, "avdec_h264")
				captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return 3840, 2160 }
				return s
			},
			sharedMode{Width: 3840, Height: 2160, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes},
			"a 3840x2160 picture is 16588800 bytes, and a node carries one of at most 4147200 bytes, which is 1920x1080 YUYV",
		},
		"a picture eight columns over the picture limit": {
			func(t *testing.T) *VideoService {
				s := hcService(t, "avdec_h264")
				captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return 1928, 1080 }
				return s
			},
			sharedMode{Width: 1928, Height: 1080, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes},
			"a 1928x1080 picture is 4164480 bytes, and a node carries one of at most 4147200 bytes, which is 1920x1080 YUYV",
		},
		"decoding failed recently": {
			func(t *testing.T) *VideoService {
				s := hcService(t, "avdec_h264")
				s.derivedFailures.remember(s.cameraIdentityKey(usbCam().GetPath()), "the decoder gave no picture", false)
				return s
			}, hcNativeMode, "decoding this camera's frames failed: the decoder gave no picture",
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
				if d := s.shareDecisionFor(context.Background(), usbCam(), format); d != (shareDecision{Shared: true, Mode: asked, PresetIgnored: hcPresetIgnoredFor(format)}) {
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
		{nil, "h264parse ! nvv4l2decoder ! nvvidconv ! video/x-raw,format=I420"},
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

func TestOwnFrames_AnH264CameraWithNoSizeWithinTheDefaultLimitDeclaresItsSmallest(t *testing.T) {
	s := hcLargeOnlyService(t, "avdec_h264")
	own, declined, native := s.ownFramesAtDefaults(usbCam().GetPath())
	want := ownFrames{codec: agentpb.VideoCodec_VIDEO_CODEC_H264, width: 1920, height: 1080, framerate: 30}
	if own != want || declined != "" || !native {
		t.Fatalf("own frames = %+v (declined %q, native %v), want %+v", own, declined, native, want)
	}
	if want := (rateQuery{v4l2PixFmtH264, 1920, 1080}); latestRateQuery() != want {
		t.Errorf("rate query = %+v, want the size declared, %+v", latestRateQuery(), want)
	}
}

// hcSizeWithinDefaultLimit is the largest of sizes within the default capture
// limit, which is what captureSizeFor answers for a request that names none.
func hcSizeWithinDefaultLimit(sizes [][2]uint32) (width, height uint32) {
	for _, size := range sizes {
		if size[0] <= defaultMaxDefaultWidth && size[1] <= defaultMaxDefaultHeight && size[0]*size[1] > width*height {
			width, height = size[0], size[1]
		}
	}
	return width, height
}

func TestNativeH264CaptureSize(t *testing.T) {
	large := [][2]uint32{{3840, 2160}, {1920, 1080}, {2560, 1440}}
	unnamed := &agentpb.StreamVideoRequest{}
	for name, tc := range map[string]struct {
		listed         [][2]uint32
		req            *agentpb.StreamVideoRequest
		want           [2]uint32
		wantAboveLimit bool
	}{
		"a size within the default limit is the largest such": {[][2]uint32{{640, 480}, {1280, 720}, {1920, 1080}}, unnamed, [2]uint32{1280, 720}, false},
		"none within the limit is the smallest listed":        {large, unnamed, [2]uint32{1920, 1080}, true},
		"a standard picture aspect is preferred to a smaller size without one": {
			[][2]uint32{{1920, 1080}, {4096, 16}, {720, 1280}}, unnamed, [2]uint32{1920, 1080}, true},
		"no standard picture aspect listed is the smallest of any": {
			[][2]uint32{{2000, 900}, {4096, 16}, {1080, 1920}}, unnamed, [2]uint32{4096, 16}, true},
		// captureSizeFor is the production one here: it keeps a named size
		// without opening the camera, and finds no camera for an unnamed one.
		"a size the request names is kept": {large, &agentpb.StreamVideoRequest{Width: 3840, Height: 2160}, [2]uint32{3840, 2160}, false},
		"no discrete size listed":          {nil, unnamed, [2]uint32{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtH264: tc.listed})
			if tc.req == unnamed {
				prev := captureSizeFor
				captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return hcSizeWithinDefaultLimit(tc.listed) }
				t.Cleanup(func() { captureSizeFor = prev })
			}
			w, h, aboveLimit := nativeH264CaptureSize("/dev/video4", tc.req)
			if [2]uint32{w, h} != tc.want || aboveLimit != tc.wantAboveLimit {
				t.Errorf("size = %dx%d (above the default limit: %v), want %dx%d (%v)", w, h, aboveLimit, tc.want[0], tc.want[1], tc.wantAboveLimit)
			}
		})
	}
}

func TestShareDecision_H264CameraWithNoSizeWithinTheDefaultLimitGetsANodeInEveryFormat(t *testing.T) {
	own := sharedMode{Width: 1920, Height: 1080, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes}
	pictures := sharedMode{Width: 1920, Height: 1080, Fourcc: "YUYV", BytesPerLine: 3840, FPS: 30, Format: appconfig.CameraFormatPixels}
	for name, tc := range map[string]struct {
		decoder []string
		pixels  shareDecision
	}{
		"with a decoder":    {[]string{"avdec_h264"}, shareDecision{Shared: true, Mode: pictures}},
		"without a decoder": {nil, shareDecision{Shared: true, Mode: own, AskedFormat: appconfig.CameraFormatPixels, AskedWhyNot: hcNoDecoder}},
	} {
		t.Run(name, func(t *testing.T) {
			s := hcLargeOnlyService(t, tc.decoder...)
			for _, format := range []string{appconfig.CameraFormatNative, appconfig.CameraFormatH264} {
				asked := own
				asked.Format = format
				if d := s.shareDecisionFor(context.Background(), usbCam(), format); d != (shareDecision{Shared: true, Mode: asked, PresetIgnored: hcPresetIgnoredFor(format)}) {
					t.Errorf("%s: decision = %+v, want %+v as asked", format, d, asked)
				}
			}
			d := s.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatPixels)
			if d != tc.pixels {
				t.Errorf("pixels: decision = %+v\nwant               %+v", d, tc.pixels)
			}
			if d.Mode.Format == appconfig.CameraFormatPixels && (d.Mode.pixelFormat() != v4l2PixFmtYUYV || d.Mode.bufferBytes() != 1920*2*1080) {
				t.Errorf("pixels: the decided mode declares %#x with %d-byte buffers, want YUYV with %d", d.Mode.pixelFormat(), d.Mode.bufferBytes(), 1920*2*1080)
			}
		})
	}
}

// What cannot be declared in any format stays the real camera: the native
// capture takes this camera, and nothing says at what size before it runs.
func TestShareDecision_H264CameraWithNoListedSizeStaysUnshared(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtH264},
		map[uint32][][2]uint32{v4l2PixFmtYUYV: {{640, 480}}}, []uint32{15, 30})
	hcCanDecode(s, "avdec_h264")
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
	s := hcService(t, "avdec_h264")
	stopAtBuffers := errors.New("stop before the buffers")
	for _, format := range []string{appconfig.CameraFormatNative, appconfig.CameraFormatH264} {
		decision := s.shareDecisionFor(context.Background(), usbCam(), format)
		var asked v4l2Format
		var queued []v4l2Buf
		w := &shareNodeWriter{mode: decision.Mode, path: "/dev/video255", fd: -1, wakeR: -1, wakeW: -1, done: closedChan()}
		w.ioctlFn = func(req uintptr, arg unsafe.Pointer) error {
			switch req {
			case vidiocSFmt:
				asked = *(*v4l2Format)(arg)
			case vidiocReqbufs:
				return stopAtBuffers
			case vidiocQbuf:
				queued = append(queued, *(*v4l2Buf)(arg))
			}
			return nil
		}
		if err := w.configure(); !errors.Is(err, stopAtBuffers) {
			t.Fatalf("%s: the node refused the decided mode: %v", format, err)
		}
		if asked.Type != v4l2BufTypeVideoOutput || asked.PixelFormat != v4l2PixFmtH264 || asked.Width != 1280 || asked.Height != 720 || asked.SizeImage != maxFrameBytes {
			t.Errorf("%s: the node was asked for %dx%d %s with an image of %d bytes, want 1280x720 H264 with %d",
				format, asked.Width, asked.Height, fourccName(asked.PixelFormat), asked.SizeImage, maxFrameBytes)
		}

		w.buffers = [][]byte{make([]byte, maxFrameBytes), make([]byte, maxFrameBytes)}
		unit := hcOwnFrame(9, true, 1)
		for i, stamp := range []shareFrameStamp{stampWithCaptureTime(12_000_000_000), stampWithSampleID(9)} {
			if err := w.WriteFrame(unit.data, stamp); err != nil {
				t.Fatalf("%s: %v", format, err)
			}
			if got := queued[i].bytesUsed(); int(got) != len(unit.data) || !bytes.Equal(w.buffers[i][:got], unit.data) {
				t.Errorf("%s: buffer %d holds %d bytes, want the whole %d-byte access unit", format, i, got, len(unit.data))
			}
		}
		if sec, usec := sfBufferTimeval(&queued[0]); sec != 12 || usec != 0 {
			t.Errorf("%s: capture time stamped as %d s %d µs", format, sec, usec)
		}
		if sec, usec := sfBufferTimeval(&queued[1]); sec != 0 || usec != 9 {
			t.Errorf("%s: sample ID stamped as %d s %d µs", format, sec, usec)
		}
	}
}

// A node keeps the mode it was made with. When the pictures of a camera stop
// at run time, its picture node holds "cannot serve" with what to do about it,
// the camera is not started again for it, and the next container created gets
// the camera's own frames with the reason.
func TestCameraShares_PicturesGivenUpAtRunTimeHoldCannotServeAndTheNextCreateServesNative(t *testing.T) {
	for name, tc := range map[string]struct {
		gaveUp       derivedLayerGaveUp
		afterAWindow sharedMode
	}{
		// The stream is the camera's: asking again later meets it again.
		"a stream the decoder reorders":   {derivedLayerGaveUp{msg: errDerivedFramesReordered.Error(), lasting: true}, hcNativeMode},
		"a decoder that produced nothing": {derivedLayerGaveUp{msg: "the pipeline took frames and produced no usable one for 10s"}, hcPixelsMode},
	} {
		t.Run(name, func(t *testing.T) {
			sfRecheckEvery(t, 5*time.Millisecond)
			h := newCSHarness(t, csH264Cam())
			hcCanDecode(h.s, "avdec_h264")
			now := time.Unix(1_700_000_000, 0)
			h.s.derivedFailures.now = func() time.Time { return now }
			fp := installFakeProducers(h.s)
			h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
			records := h.records("cv-a")
			if len(records) != 1 || records[0].mode != hcPixelsMode {
				t.Fatalf("records = %+v, want a picture node; logs: %v", records, h.logs.All())
			}
			node := records[0].io.(*csNode)
			node.send(t, 1)
			if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
				t.Fatalf("the camera did not start; logs: %v", h.logs.All())
			}
			hub := fp.hub(0)
			hub.broadcast(sfDerivedPicture(1, 1_000_000_000))
			node.next(t)

			// What captureDeriving does when the camera's picture layer gives up.
			h.s.derivedFailures.remember(h.s.cameraIdentityKey("/dev/video2"), tc.gaveUp.msg, tc.gaveUp.lasting)
			hub.rawNotOffered(fixedReason("pictures cannot be decoded from this camera's frames: " + tc.gaveUp.msg))

			whyNoPictures := "decoding this camera's frames failed: " + tc.gaveUp.msg
			want := "cv-a [format pixels, stamp time] (cannot serve: " + fmt.Sprintf(picturesNotMade, whyNoPictures) + ")"
			if !csWaitFor(2*time.Second, func() bool { return h.reportOf("/dev/video2")["share_state"] == want }) {
				t.Fatalf("share_state = %q\nwant         %q", h.reportOf("/dev/video2")["share_state"], want)
			}
			time.Sleep(60 * time.Millisecond) // a dozen rechecks
			if got := h.reportOf("/dev/video2")["share_state"]; got != want || fp.count() != 1 {
				t.Fatalf("after the rechecks: share_state = %q, the camera started %d times; want the same state and one start", got, fp.count())
			}
			if n := h.logged("camera share: cv-a: Logi H264 cannot serve its node"); n != 1 {
				t.Errorf("cannot-serve line logged %d times, want 1; logs: %v", n, h.logs.All())
			}
			node.noWriteWithin(t, 20*time.Millisecond)

			h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", nil)
			next := h.records("cv-b")
			if len(next) != 1 || next[0].mode != hcNativeMode || next[0].askedFormat != appconfig.CameraFormatPixels || next[0].askedWhyNot != whyNoPictures {
				t.Fatalf("the next container's record = %+v, want the camera's H.264 served for pixels because %q", next, whyNoPictures)
			}
			said := "asked for pixels; serving native because " + whyNoPictures
			if state := h.reportOf("/dev/video2")["share_state"]; !strings.Contains(state, "cv-b [format native, stamp time] (idle; "+said+")") {
				t.Errorf("share_state = %q, want cv-b's node to say %q", state, said)
			}

			now = now.Add(captureFailureRetryAfter + time.Second)
			h.s.CreateCameraShares(context.Background(), "cv-c", "sh.wendy.cv-c", nil)
			if later := h.records("cv-c"); len(later) != 1 || later[0].mode != tc.afterAWindow {
				t.Fatalf("after %s the next container's record = %+v, want %+v", captureFailureRetryAfter, later, tc.afterAWindow)
			}
		})
	}
}

func (n *csNode) noWriteWithin(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case e := <-n.events:
		t.Fatalf("a frame was written to the node: %+v", e)
	case <-time.After(d):
	}
}

// The camera's listing says H.264, the capture finds out it delivers none and
// goes on to the next source. The policy and the nodes go by what the capture
// found, not by the listing.
func TestCameraShares_ASourceTheCameraRefusedAtCaptureIsNotDeclaredAgain(t *testing.T) {
	sfRecheckEvery(t, 5*time.Millisecond)
	h := newCSHarness(t, csH264Cam())
	h.s.gstMemo.available = map[string]bool{"gdpdepay": true, "gdppay": true, "h264parse": true, "avdec_h264": true, "jpegdec": true, "x264enc": true}
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")})
	h.s.CreateCameraShares(context.Background(), "cv-p", "sh.wendy.cv-p", nil)
	listed, pictures := h.records("cv-a"), h.records("cv-p")
	if len(listed) != 1 || listed[0].mode != hcNativeMode || len(pictures) != 1 || pictures[0].mode != hcPixelsMode {
		t.Fatalf("records = %+v and %+v, want an H.264 node and a picture node from the listing", listed, pictures)
	}

	// What captureNatively does when the H.264 source gives way.
	const refusal = "VIDIOC_S_FMT H264 rejected: invalid argument"
	h.s.sourceRefusals.remember(h.s.sourceRefusalKey("/dev/video2", "H.264"), refusal, false)

	if own, declined, native := h.s.ownFramesAtDefaults("/dev/video2"); declined != "" || !native || own.codec != videoCodecMJPEG {
		t.Fatalf("own frames = %+v (declined %q, native %v), want the JPEG source's", own, declined, native)
	}
	listed[0].io.(*csNode).send(t, 1)
	want := "cv-a [format native, stamp time] (cannot serve: " + fmt.Sprintf(nodeModeNotDelivered, "JPEG", "1280x720 H264, 30 fps") + ")"
	if !csWaitFor(2*time.Second, func() bool { return strings.Contains(h.reportOf("/dev/video2")["share_state"], want) }) {
		t.Fatalf("share_state = %q\nwant it to hold %q", h.reportOf("/dev/video2")["share_state"], want)
	}
	time.Sleep(60 * time.Millisecond)
	if fp.count() != 0 {
		t.Fatalf("the camera was started %d times for a node it cannot serve", fp.count())
	}
	// Pictures are pictures whichever source they are decoded from.
	if reason := h.s.shareUnserved(pictures[0]); reason != "" {
		t.Errorf("the picture node is unserved: %s", reason)
	}

	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")})
	if next := h.records("cv-b"); len(next) != 1 || next[0].mode != sfNativeMode || next[0].askedFormat != "" {
		t.Fatalf("the next container's record = %+v, want the JPEG frames the camera delivers", next)
	}
}

func TestCameraShareState_NamesTheDecoderOfAPictureNodesPictures(t *testing.T) {
	h := newCSHarness(t, csH264Cam())
	hcCanDecode(h.s, "avdec_h264")
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a", nil)
	h.s.CreateCameraShares(context.Background(), "cv-n", "sh.wendy.cv-n", []appconfig.Entitlement{sfCameraEntitlement(appconfig.CameraFormatNative, "")})
	for _, id := range []string{"cv-a", "cv-n"} {
		h.records(id)[0].io.(*csNode).send(t, 1)
	}
	if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 && hubSubscriberCount(fp.hub(0)) == 2 }) {
		t.Fatalf("the camera did not start for both nodes; logs: %v", h.logs.All())
	}
	layer, whyNot := h.s.pictureLayerFor(fp.hub(0), "/dev/video2", ownFrames{codec: agentpb.VideoCodec_VIDEO_CODEC_H264, width: 1280, height: 720, framerate: 30})
	if layer == nil {
		t.Fatal(whyNot)
	}
	defer fp.hub(0).feedDerivedLayer(layer)()

	want := "cv-a [format pixels, stamp time] (reading; pictures decoded by avdec_h264), cv-n [format native, stamp time] (reading)"
	if !csWaitFor(2*time.Second, func() bool { return h.reportOf("/dev/video2")["share_state"] == want }) {
		t.Fatalf("share_state = %q\nwant         %q", h.reportOf("/dev/video2")["share_state"], want)
	}
}

// What failed is remembered for the camera, not for the path it was at: another
// camera plugged in there starts clean, and the first one still has its
// failure when it comes back, wherever udev puts it.
func TestCaptureFailures_AreRememberedForTheCameraNotItsPath(t *testing.T) {
	h := newCSHarness(t, csH264Cam())
	hcCanDecode(h.s, "avdec_h264")
	reorders, other := csH264Cam(), csH264Cam()
	other.name, other.id = "Other H264", camera.Identity{Device: csPort("1-2.2"), Index: "0", Name: "Other H264"}
	plugAt := func(base string, cam csCamera) {
		cam.base = base
		h.mu.Lock()
		h.cams = map[string]csCamera{base: cam}
		h.mu.Unlock()
	}
	servedAt := func(path string) string {
		t.Helper()
		dev := &agentpb.VideoDevice{Path: path, Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB}
		return h.s.shareDecisionFor(context.Background(), dev, appconfig.CameraFormatPixels).Mode.Format
	}

	h.s.derivedFailures.remember(h.s.cameraIdentityKey("/dev/video2"), errDerivedFramesReordered.Error(), true)
	h.s.sourceRefusals.remember(h.s.sourceRefusalKey("/dev/video2", "JPEG"), "no usable frame", false)
	if got := servedAt("/dev/video2"); got != appconfig.CameraFormatNative {
		t.Fatalf("the camera whose stream reorders is served %s, want its own frames", got)
	}

	plugAt("video2", other)
	if got := servedAt("/dev/video2"); got != appconfig.CameraFormatPixels {
		t.Errorf("another camera at the same path is served %s: it inherited the first one's failure", got)
	}
	if _, refused := h.s.sourceRefusals.recent(h.s.sourceRefusalKey("/dev/video2", "JPEG")); refused {
		t.Error("another camera at the same path inherited the first one's source refusal")
	}

	plugAt("video6", reorders)
	if got := servedAt("/dev/video6"); got != appconfig.CameraFormatNative {
		t.Errorf("the first camera, back at another path, is served %s: its failure was forgotten", got)
	}
}

func TestCameraIdentityKey_PrefersTheSerialThenTheSysfsIdentityThenThePath(t *testing.T) {
	s := &VideoService{}
	s.cameraIdentity = func(base string) (camera.Identity, error) {
		if base == "video4" {
			return camera.Identity{Device: csPort("1-3"), Index: "0", Name: "Brio 101"}, nil
		}
		return camera.Identity{}, errors.New("no device link")
	}
	s.readStableNames = func() map[string]stableNames {
		return map[string]stableNames{"/dev/video0": {byID: "usb-Acme_Camera_SN1-video-index0", byPath: "pci-0000:00:14.0-usb-0:2:1.0"},
			"/dev/video4": {byPath: "pci-0000:00:14.0-usb-0:3:1.0"}}
	}
	for path, want := range map[string]string{
		"/dev/video0": "by-id:usb-Acme_Camera_SN1-video-index0",
		"/dev/video4": "sysfs\x00" + csPort("1-3") + "\x000\x00Brio 101",
		"/dev/video9": "/dev/video9",
	} {
		if got := s.cameraIdentityKey(path); got != want {
			t.Errorf("key of %s = %q, want %q", path, got, want)
		}
	}
}
