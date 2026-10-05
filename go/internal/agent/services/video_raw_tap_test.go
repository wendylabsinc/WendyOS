package services

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- hub routing ---

func TestDeviceHub_RawAndEncodedSubscribersGetDifferentFrames(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	idEnc, chEnc, _ := h.subscribe()
	idRaw, chRaw, _ := h.subscribeKind(true)
	defer h.unsubscribe(idEnc)
	defer h.unsubscribe(idRaw)

	format := &agentpb.RawFormat{Width: 2, Height: 1, Fourcc: fourccYUYV, BytesPerLine: 4}
	if !h.broadcast(&videoFrame{data: []byte{1}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}) {
		t.Fatal("encoded broadcast reported no subscribers")
	}
	if !h.publishRaw([]byte{9, 9, 9, 9}, 7, format) {
		t.Fatal("raw broadcast reported no subscribers")
	}

	select {
	case f := <-chEnc:
		if f.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || f.rawFmt != nil {
			t.Errorf("encoded subscriber got %v (rawFmt=%v)", f.codec, f.rawFmt)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("encoded subscriber received nothing")
	}
	select {
	case f := <-chRaw:
		if f.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || f.rawFmt != format || f.tsNs != 7 {
			t.Errorf("raw subscriber got %v (rawFmt=%v ts=%d)", f.codec, f.rawFmt, f.tsNs)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("raw subscriber received nothing")
	}
	// And nothing crossed over.
	if len(chEnc) != 0 || len(chRaw) != 0 {
		t.Errorf("frames crossed between kinds: enc=%d raw=%d pending", len(chEnc), len(chRaw))
	}
}

func TestDeviceHub_RawNotOffered_TurnsAwayOnlyRawSubscribers(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	idEnc, chEnc, _ := h.subscribe()
	idRaw, chRaw, _ := h.subscribeKind(true)
	defer h.unsubscribe(idEnc)
	defer h.unsubscribe(idRaw)

	h.rawNotOffered("camera streams H.264 natively; raw frames are not offered")

	// The raw channel is closed and carries the reason; the encoded one is untouched.
	if _, ok := <-chRaw; ok {
		t.Fatal("raw subscriber channel should be closed")
	}
	err := h.subscriberErr(idRaw)
	if err == nil {
		t.Fatal("raw subscriber has no error attached")
	}
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %v", err)
	}
	if !streamreason.Has(err, streamreason.RawUnavailable) {
		t.Errorf("error must carry reason %s: %v", streamreason.RawUnavailable, err)
	}
	if h.subscriberErr(idEnc) != nil {
		t.Error("encoded subscriber must not inherit the raw refusal")
	}
	if h.wantRaw() {
		t.Error("a turned-away raw subscriber must not count as wanting raw")
	}

	// Broadcast after the refusal must neither panic on the closed channel nor
	// stop the producer: the viewer is still there.
	if !h.broadcast(&videoFrame{data: []byte{1}}) {
		t.Fatal("broadcast stopped the producer while an encoded subscriber remains")
	}
	select {
	case <-chEnc:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("encoded subscriber stopped receiving after the raw refusal")
	}
}

func TestDeviceHub_ProducerTeardownSkipsAlreadyClosedRawSubscriber(t *testing.T) {
	h, _ := newTestHub(t)
	_, _, _ = h.subscribeKind(true)
	h.rawNotOffered("no raw")
	// runProducer's teardown closes every subscriber it has not closed already.
	// A second close would panic; this is the loop it runs.
	h.mu.Lock()
	for _, sub := range h.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	h.mu.Unlock()
}

func TestGetOrCreateHub_DeviceDefaultRequestJoinsWhateverIsPlaying(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	installFakeProducers(svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pinned := &agentpb.StreamVideoRequest{Width: 512, Height: 484, Framerate: 25}
	h, id, _, err := svc.getOrCreateHub(ctx, "/dev/video4", pinned, hubHolderStreamClient)
	if err != nil {
		t.Fatalf("first getOrCreateHub failed: %v", err)
	}
	defer h.unsubscribe(id)

	// A bare `camera view` names nothing and must be able to join the app that
	// pinned the mode, instead of being locked out of its own camera.
	h2, id2, _, err := svc.getOrCreateHub(ctx, "/dev/video4", &agentpb.StreamVideoRequest{}, hubHolderStreamClient)
	if err != nil {
		t.Fatalf("device-default request was refused: %v", err)
	}
	defer h2.unsubscribe(id2)
	if h2 != h {
		t.Fatal("device-default request created a second hub instead of joining")
	}

	// An explicit, different size is still a refusal — handing 512x484 to a
	// caller that asked for 1280x720 would be worse than saying no.
	_, _, _, err = svc.getOrCreateHub(ctx, "/dev/video4", &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 25}, hubHolderStreamClient)
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.InvalidArgument {
		t.Errorf("explicit mismatch must still be InvalidArgument, got %v", err)
	}
}

func TestGetOrCreateHub_RawRefusedUpFrontOnceProducerDeclined(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	installFakeProducers(svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, id, _, err := svc.getOrCreateHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{}, hubHolderStreamClient)
	if err != nil {
		t.Fatalf("getOrCreateHub failed: %v", err)
	}
	defer h.unsubscribe(id)
	h.rawNotOffered("camera is captured as MJPEG at 1280x720; raw frames are not offered")

	_, _, _, err = svc.getOrCreateHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW}, hubHolderStreamClient)
	if err == nil {
		t.Fatal("raw request joined a hub that already declined raw")
	}
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "MJPEG") {
		t.Errorf("refusal must carry the producer's reason: %v", err)
	}
}

// --- pipeline planning ---

// withYUYVModes describes a camera that advertises these modes in YUYV and
// nothing in any other raw format -- the shape of every module measured, and
// the case the stacked-mode geometry reasons about.
func withYUYVModes(t *testing.T, modes [][2]uint32) {
	t.Helper()
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtYUYV: modes})
}

// withRawModes describes a camera per pixel format, for the cases where which
// format the tap picks is the point.
func withRawModes(t *testing.T, byFormat map[uint32][][2]uint32) {
	t.Helper()
	prev := enumerateRawFrameSizes
	enumerateRawFrameSizes = func(_ string, pixfmt uint32) [][2]uint32 { return byFormat[pixfmt] }
	t.Cleanup(func() { enumerateRawFrameSizes = prev })
}

func mustPlan(t *testing.T, req *agentpb.StreamVideoRequest, available map[string]bool) gstPipelinePlan {
	t.Helper()
	plan, err := planGStreamerPipeline("gst", "/dev/video4", req, "x264enc", true, camera.TransportUSB, "", pipeWireSource{}, available)
	if err != nil {
		t.Fatalf("planGStreamerPipeline: %v", err)
	}
	return plan
}

func TestPlan_TeesRawWhenTheDeviceAdvertisesYUYVAtTheCaptureSize(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{256, 192}, {512, 384}, {512, 484}})
	// validateStreamParams consults the device too; on this host /dev/video4 does
	// not exist, so it falls back to the allowlist — request nothing and let the
	// plan pick the size through the (faked) enumeration instead.
	prev := bestDefaultFrameSizeForDevice
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return 512, 484 }
	t.Cleanup(func() { bestDefaultFrameSizeForDevice = prev })

	plan := mustPlan(t, &agentpb.StreamVideoRequest{}, map[string]bool{"x264enc": true, "h264parse": true})
	joined := strings.Join(plan.args, " ")

	if plan.raw == nil {
		t.Fatalf("expected a raw tap, got none: %s", plan.rawWhy)
	}
	if plan.raw.GetWidth() != 512 || plan.raw.GetHeight() != 484 || plan.raw.GetFourcc() != fourccYUYV || plan.raw.GetBytesPerLine() != 1024 {
		t.Errorf("unexpected raw format %v", plan.raw)
	}
	for _, want := range []string{
		"format=YUY2",                    // the promised layout is pinned on the capture
		"tee name=rawtap",                // branched off before the queue/crop/encoder
		"rawtap. ! queue",                // the raw branch has its own leaky queue
		"leaky=downstream ! fdsink fd=3", // and writes to the tap fd
		"fdsink fd=1",                    // the encoded branch is still there
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("pipeline missing %q: %s", want, joined)
		}
	}
	// The tee must come before the encoder branch's queue, and the raw branch
	// must be attached at the very end (gst-launch syntax).
	if strings.Index(joined, "tee name=rawtap") > strings.Index(joined, "x264enc") {
		t.Errorf("tee must precede the encoder: %s", joined)
	}
	if !strings.HasSuffix(joined, "fdsink fd=3") {
		t.Errorf("raw branch must be the trailing branch: %s", joined)
	}
}

func TestPlan_NoRawWhenYUYVIsNotAdvertisedAtThatSize(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{640, 480}})
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, map[string]bool{"x264enc": true})
	if plan.raw != nil {
		t.Fatalf("raw offered for a size the device does not advertise: %v", plan.raw)
	}
	if !strings.Contains(plan.rawWhy, "YUYV") {
		t.Errorf("reason should name the missing format: %q", plan.rawWhy)
	}
	joined := strings.Join(plan.args, " ")
	if strings.Contains(joined, "tee") || strings.Contains(joined, "fd=3") || strings.Contains(joined, "format=YUY2") {
		t.Errorf("pipeline must be unchanged when raw is not offered: %s", joined)
	}
}

func TestPlan_NoRawWhenMJPEGCaptureIsSelected(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{1280, 720}})
	prev := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(string, uint32, uint32) bool { return true }
	t.Cleanup(func() { deviceSupportsMJPEGSize = prev })

	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, map[string]bool{"x264enc": true, "jpegdec": true})
	if plan.raw != nil {
		t.Fatalf("raw offered on an MJPEG capture: %v", plan.raw)
	}
	if !strings.Contains(plan.rawWhy, "MJPEG") {
		t.Errorf("reason should say MJPEG: %q", plan.rawWhy)
	}
	if !strings.Contains(strings.Join(plan.args, " "), "jpegdec") {
		t.Errorf("MJPEG pipeline lost its decoder: %v", plan.args)
	}
}

func TestPlan_NoRawThroughPipeWire(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{512, 484}})
	plan, err := planGStreamerPipeline("gst", "/dev/video4", &agentpb.StreamVideoRequest{}, "x264enc", true,
		camera.TransportUSB, "", pipeWireSource{serial: 42}, map[string]bool{"x264enc": true, "pipewiresrc": true})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.raw != nil {
		t.Fatalf("raw offered on the PipeWire path: %v", plan.raw)
	}
	if !strings.Contains(plan.rawWhy, "PipeWire") {
		t.Errorf("reason should say PipeWire: %q", plan.rawWhy)
	}
}

func TestPlan_RawFrameMustFitADefaultGRPCMessage(t *testing.T) {
	if maxRawFrameBytes >= 4*1024*1024 {
		t.Fatalf("maxRawFrameBytes %d does not leave room under grpc-go's 4 MiB default", maxRawFrameBytes)
	}
	withYUYVModes(t, [][2]uint32{{1920, 1080}})
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}, map[string]bool{"x264enc": true})
	if plan.raw != nil {
		t.Fatalf("a 1080p YUYV frame (%d bytes) must not be offered raw", 1920*1080*2)
	}
	if !strings.Contains(plan.rawWhy, "exceeds") {
		t.Errorf("reason should say the frame is too large: %q", plan.rawWhy)
	}
}

// --- stacked thermal modes ---

func TestStackedMetadataRows_TC001(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{256, 192}, {512, 384}, {512, 484}, {644, 384}})
	if got := stackedMetadataRows("/dev/video4", 512, 484); got != 100 {
		t.Errorf("512x484 over a 512x384 picture: want 100 metadata rows, got %d", got)
	}
	if got := stackedMetadataRows("/dev/video4", 512, 384); got != 0 {
		t.Errorf("the plain picture mode must not be cropped, got %d", got)
	}
	if got := stackedMetadataRows("/dev/video4", 644, 384); got != 0 {
		t.Errorf("a non-standard mode with no same-width sibling is not stacked, got %d", got)
	}
}

func TestStackedMetadataRows_OrdinaryWebcamIsNeverCropped(t *testing.T) {
	// 640x480 has a 640x360 sibling 120 rows shorter — and is a plain 4:3
	// picture. The standard-aspect test on the requested mode is what protects it.
	withYUYVModes(t, [][2]uint32{{640, 360}, {640, 480}, {1280, 720}})
	if got := stackedMetadataRows("/dev/video0", 640, 480); got != 0 {
		t.Errorf("640x480 must never be read as stacked, got %d", got)
	}
}

func TestPlan_CropsStackedRowsForViewersButNotForTheRawTap(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{512, 384}, {512, 484}})
	prev := bestDefaultFrameSizeForDevice
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return 512, 484 }
	t.Cleanup(func() { bestDefaultFrameSizeForDevice = prev })
	prevTop := thermalMetadataRowsOnTop
	thermalMetadataRowsOnTop = true
	t.Cleanup(func() { thermalMetadataRowsOnTop = prevTop })

	plan := mustPlan(t, &agentpb.StreamVideoRequest{}, map[string]bool{"x264enc": true, "videocrop": true})
	joined := strings.Join(plan.args, " ")
	if !strings.Contains(joined, "videocrop top=100") {
		t.Fatalf("viewers of the stacked mode must get the picture rows only: %s", joined)
	}
	// Order: tee (raw branch point) → queue → videocrop → encoder. The raw tap
	// therefore keeps all 484 rows.
	tee, crop, enc := strings.Index(joined, "tee name=rawtap"), strings.Index(joined, "videocrop"), strings.Index(joined, "x264enc")
	if !(tee < crop && crop < enc) {
		t.Errorf("expected tee < videocrop < encoder, got %d %d %d: %s", tee, crop, enc, joined)
	}
	if plan.raw == nil || plan.raw.GetHeight() != 484 {
		t.Errorf("raw tap must carry the whole frame, got %v", plan.raw)
	}
}

func TestPlan_DefaultCaptureOfAStackedThermalModulePromotesToTheStackedMode(t *testing.T) {
	// The device default picks the plain 4:3 picture (bestDefaultFrameSize's
	// standard-aspect rule). With videocrop available the plan captures the
	// stacked sibling instead, crops it back for viewers, and offers the whole
	// frame raw — so a viewer holding the default hub no longer locks a raw
	// subscriber out of the temperature rows.
	// 1280x820 stands in as the stacked sibling of 1280x720 here because an
	// explicit size must clear validateStreamParams, which on a host with no
	// /dev/video4 falls back to the fixed allowlist — and 512x384 is not on it.
	withYUYVModes(t, [][2]uint32{{256, 192}, {512, 384}, {512, 484}, {644, 384}, {1280, 720}, {1280, 820}})
	prev := bestDefaultFrameSizeForDevice
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return 512, 384 }
	t.Cleanup(func() { bestDefaultFrameSizeForDevice = prev })

	plan := mustPlan(t, &agentpb.StreamVideoRequest{}, map[string]bool{"x264enc": true, "videocrop": true})
	joined := strings.Join(plan.args, " ")
	if !strings.Contains(joined, "width=512,height=484") {
		t.Fatalf("default capture should be the stacked mode: %s", joined)
	}
	if !strings.Contains(joined, "videocrop top=100") {
		t.Errorf("viewers must get the picture rows only: %s", joined)
	}
	if plan.raw == nil || plan.raw.GetHeight() != 484 {
		t.Errorf("raw tap must carry the stacked frame, got %v", plan.raw)
	}

	// Naming the plain size is honoured verbatim — promotion is for defaults only.
	explicit := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, map[string]bool{"x264enc": true, "videocrop": true})
	if ej := strings.Join(explicit.args, " "); strings.Contains(ej, "820") || !strings.Contains(ej, "width=1280,height=720") {
		t.Errorf("an explicit 1280x720 request must not be promoted to its stacked sibling: %s", ej)
	}
}

func TestPlan_NoPromotionWithoutVideocrop(t *testing.T) {
	// Without videocrop the stacked rows would reach viewers as a stripe of noise
	// — the bug the standard-aspect default fixed — so the default stays plain.
	withYUYVModes(t, [][2]uint32{{512, 384}, {512, 484}})
	prev := bestDefaultFrameSizeForDevice
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return 512, 384 }
	t.Cleanup(func() { bestDefaultFrameSizeForDevice = prev })

	plan := mustPlan(t, &agentpb.StreamVideoRequest{}, map[string]bool{"x264enc": true})
	joined := strings.Join(plan.args, " ")
	if !strings.Contains(joined, "width=512,height=384") || strings.Contains(joined, "484") {
		t.Errorf("default must stay the plain picture mode without videocrop: %s", joined)
	}
}

func TestStackedModeAbove(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{256, 192}, {512, 384}, {512, 484}, {644, 384}, {640, 360}, {640, 480}})
	if w, h := stackedModeAbove("/dev/video4", 512, 384); w != 512 || h != 484 {
		t.Errorf("512x384 → want 512x484, got %dx%d", w, h)
	}
	if w, h := stackedModeAbove("/dev/video4", 640, 360); w != 0 || h != 0 {
		t.Errorf("640x480 is a standard picture, not a stacked sibling of 640x360; got %dx%d", w, h)
	}
	if w, h := stackedModeAbove("/dev/video4", 512, 484); w != 0 || h != 0 {
		t.Errorf("a non-standard mode has no stacked sibling of its own; got %dx%d", w, h)
	}
}

func TestPlan_NoCropWithoutVideocropElement(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{512, 384}, {512, 484}})
	prev := bestDefaultFrameSizeForDevice
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return 512, 484 }
	t.Cleanup(func() { bestDefaultFrameSizeForDevice = prev })

	plan := mustPlan(t, &agentpb.StreamVideoRequest{}, map[string]bool{"x264enc": true})
	if strings.Contains(strings.Join(plan.args, " "), "videocrop") {
		t.Errorf("videocrop must not be used when gst-inspect does not list it: %v", plan.args)
	}
}

// --- request shape ---

func TestRequestsDeviceDefault(t *testing.T) {
	if !requestsDeviceDefault(&agentpb.StreamVideoRequest{DeviceId: 4, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW}) {
		t.Error("device id and raw do not make a request explicit about size or rate")
	}
	if requestsDeviceDefault(&agentpb.StreamVideoRequest{Framerate: 25}) {
		t.Error("a named framerate is an explicit request")
	}
}

func TestErrRawUnavailable_CarriesReasonAndCode(t *testing.T) {
	err := errRawUnavailable("because")
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition, got %v", err)
	}
	if !streamreason.Has(err, streamreason.RawUnavailable) {
		t.Errorf("want reason %s: %v", streamreason.RawUnavailable, err)
	}
	if !strings.Contains(err.Error(), "because") {
		t.Errorf("message must carry the cause: %v", err)
	}
}

// --- raw formats beyond YUYV ---

// A thermal core exposing 16-bit greyscale directly, rather than stacking it
// onto a picture, was refused before: the tap only ever looked for YUYV. Its
// bytes are exactly what raw exists to deliver.
func TestPlan_OffersRawForAY16OnlyCamera(t *testing.T) {
	// 640x480 rather than a thermal-native size: validateStreamParams applies
	// its own resolution rules upstream, and the format is what is under test.
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtY16: {{640, 480}}})
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, map[string]bool{})
	if plan.raw == nil {
		t.Fatalf("raw not offered for a Y16 camera: %s", plan.rawWhy)
	}
	if got := plan.raw.GetFourcc(); got != "Y16 " {
		t.Errorf("fourcc = %q, want %q", got, "Y16 ")
	}
	if got := plan.raw.GetBytesPerLine(); got != 640*2 {
		t.Errorf("bytes per line = %d, want %d", got, 640*2)
	}
	if !strings.Contains(strings.Join(plan.args, " "), "format=GRAY16_LE") {
		t.Errorf("capture caps must name the format for GStreamer: %v", plan.args)
	}
}

// GREY is one byte per pixel, so bytes_per_line must not assume two -- a
// subscriber decoding on a wrong stride reads a sheared image and no error.
func TestPlan_GreyIsOneBytePerPixel(t *testing.T) {
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtGrey: {{640, 480}}})
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, map[string]bool{})
	if plan.raw == nil {
		t.Fatalf("raw not offered for a GREY camera: %s", plan.rawWhy)
	}
	if got := plan.raw.GetBytesPerLine(); got != 640 {
		t.Errorf("bytes per line = %d, want %d", got, 640)
	}
}

// YUYV stays preferred when a camera offers several, so the common path -- every
// UVC webcam, and the thermal modules -- is unchanged by widening the table.
func TestPlan_PrefersYUYVWhenACameraOffersSeveral(t *testing.T) {
	withRawModes(t, map[uint32][][2]uint32{
		v4l2PixFmtYUYV: {{640, 480}},
		v4l2PixFmtY16:  {{640, 480}},
	})
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, map[string]bool{})
	if plan.raw == nil || plan.raw.GetFourcc() != "YUYV" {
		t.Fatalf("want YUYV preferred, got %+v (%s)", plan.raw, plan.rawWhy)
	}
}

// A camera offering only formats the tap cannot build a pipeline for is refused
// with a reason naming what it CAN do -- "unsupported" alone tells nobody what
// to try next.
func TestPlan_RefusalNamesTheFormatsRawSupports(t *testing.T) {
	withRawModes(t, map[uint32][][2]uint32{})
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, map[string]bool{})
	if plan.raw != nil {
		t.Fatalf("raw offered for a camera advertising nothing: %+v", plan.raw)
	}
	for _, want := range []string{"YUYV", "Y16", "GREY"} {
		if !strings.Contains(plan.rawWhy, want) {
			t.Errorf("refusal %q should name %s", plan.rawWhy, want)
		}
	}
}

// --- raw frame stamping ---

// receiveFrame takes one frame off a subscriber channel or fails the test.
func receiveFrame(t *testing.T, ch <-chan *videoFrame, what string) *videoFrame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed", what)
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: nothing received", what)
	}
	return nil
}

// TestRawTapFramesCarryTheCanonicalReceiptAndTheirOwnDeviceCounter drives real
// frames through pumpRawTap, the tap's pipe reader, into a hub. A raw frame must
// arrive stamped on the canonical timeline (a bracketed boot-clock receipt taken
// between the write into the pipe and the delivery) and named by the device's
// raw counter, which continues across a producer restart. The encoded plane on
// the same hub must keep its own dense identifiers while raw frames interleave:
// a hole there is what a loopback reader is told to read as a drop.
func TestRawTapFramesCarryTheCanonicalReceiptAndTheirOwnDeviceCounter(t *testing.T) {
	const path = "/dev/video9"
	svc := &VideoService{logger: zap.NewNop()}
	format := &agentpb.RawFormat{Width: 2, Height: 2, Fourcc: "Y16 ", BytesPerLine: 4}
	frameBytes := int(format.GetBytesPerLine() * format.GetHeight())

	newHub := func() (*deviceHub, context.CancelFunc) {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		return &deviceHub{
			subs: map[int]*hubSubscriber{}, subDrops: map[int]uint64{},
			ctx: ctx, cancel: cancel, done: make(chan struct{}),
			sampleSeq:    svc.sampleSeqLocked(path),
			rawSampleSeq: svc.rawSampleSeqLocked(path),
		}, cancel
	}
	// runTap starts the tap reader on a fresh pipe and returns the write end
	// and a channel closed when the reader has returned.
	runTap := func(h *deviceHub) (*os.File, chan struct{}) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			svc.pumpRawTap(context.Background(), r, format, h, path)
		}()
		return w, done
	}
	writeFrame := func(w *os.File, fill byte) {
		t.Helper()
		if _, err := w.Write(bytes.Repeat([]byte{fill}, frameBytes)); err != nil {
			t.Fatal(err)
		}
	}

	hub, cancel := newHub()
	_, enc, _ := hub.subscribe()
	_, raw, _ := hub.subscribeKind(true)
	w, done := runTap(hub)

	var lastRaw uint64
	for i := 1; i <= 2; i++ {
		before, _, _, err := data.CaptureReceipt()
		if err != nil {
			t.Fatal(err)
		}
		writeFrame(w, byte(i))
		got := receiveFrame(t, raw, "raw subscriber")
		_, _, after, _ := data.CaptureReceipt()
		if !hub.produce(&videoFrame{data: []byte{byte(i)}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}) {
			t.Fatal("encoded produce reported no subscribers")
		}
		encoded := receiveFrame(t, enc, "encoded subscriber")

		if got.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || got.rawFmt != format {
			t.Fatalf("raw frame %d: codec %v format %v", i, got.codec, got.rawFmt)
		}
		if !bytes.Equal(got.data, bytes.Repeat([]byte{byte(i)}, frameBytes)) {
			t.Fatalf("raw frame %d: payload is not the bytes written to the tap", i)
		}
		if got.receiptBootNanos == 0 {
			t.Fatalf("raw frame %d carries no boot-clock receipt", i)
		}
		if got.receiptBootNanos < before || got.receiptBootNanos > after {
			t.Fatalf("raw frame %d receipt %d lies outside the bracket [%d, %d] of its write and delivery",
				i, got.receiptBootNanos, before, after)
		}
		if got.receiptUncertaintyNanos < 0 {
			t.Fatalf("raw frame %d uncertainty %d is negative", i, got.receiptUncertaintyNanos)
		}
		if got.tsNs == 0 {
			t.Fatalf("raw frame %d lost its wall-clock read time", i)
		}
		if got.sampleID != uint64(i) {
			t.Fatalf("raw frame %d has sample id %d, want %d", i, got.sampleID, i)
		}
		if encoded.sampleID != uint64(i) {
			t.Fatalf("encoded frame %d has sample id %d, want %d: raw frames punched a hole in the encoded identifiers",
				i, encoded.sampleID, i)
		}
		lastRaw = got.sampleID
	}
	w.Close() //nolint:errcheck
	<-done
	cancel()

	// A producer restart builds a new hub for the same device key; the raw
	// counter must pick up where it stopped, not reissue identifier 1.
	restarted, restartedCancel := newHub()
	defer restartedCancel()
	_, raw2, _ := restarted.subscribeKind(true)
	w2, done2 := runTap(restarted)
	writeFrame(w2, 7)
	if got := receiveFrame(t, raw2, "raw subscriber after restart"); got.sampleID != lastRaw+1 {
		t.Fatalf("raw sample id after producer restart = %d, want %d", got.sampleID, lastRaw+1)
	}
	w2.Close() //nolint:errcheck
	<-done2
}

// TestPublishRawDropsOversizedFramesWithoutAnIdentity keeps a gap in the raw
// identifiers meaningful, exactly as produce does for encoded frames.
func TestPublishRawDropsOversizedFramesWithoutAnIdentity(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	hub.rawSampleSeq = new(atomic.Uint64)
	_, raw, _ := hub.subscribeKind(true)
	format := &agentpb.RawFormat{Width: 1, Height: 1, Fourcc: fourccYUYV, BytesPerLine: 2}
	hub.publishRaw(make([]byte, maxRawFrameBytes+1), 1, format)
	hub.publishRaw([]byte{1, 2}, 2, format)
	if got := receiveFrame(t, raw, "raw subscriber"); got.sampleID != 1 {
		t.Fatalf("raw sample id = %d, want 1: the dropped oversized frame consumed an identifier", got.sampleID)
	}
	if hub.sampleSeq.Load() != 0 {
		t.Fatalf("raw frames advanced the encoded counter to %d", hub.sampleSeq.Load())
	}
}
