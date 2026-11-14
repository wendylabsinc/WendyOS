package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/rawvideo"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// inProcessTestID is a device ID no test host has a node for.
const inProcessTestID = 90

func mustInProcessInput(t *testing.T, f rawvideo.Frame) inProcessInput {
	t.Helper()
	in, err := inProcessInputFor(f)
	if err != nil {
		t.Fatalf("inProcessInputFor(%s): %v", f.Describe(), err)
	}
	return in
}

func grayFrame(w, h int, fill byte) rawvideo.Frame {
	return rawvideo.Frame{Data: bytes.Repeat([]byte{fill}, w*h), Width: w, Height: h, Format: rawvideo.FormatGRAY8, Timestamp: time.Now()}
}

func TestPlanInProcessPipeline(t *testing.T) {
	x264 := gstEncoderResult{element: "x264enc", hasH264Parse: true}
	thermal := rawvideo.Frame{Data: make([]byte, 256*192*2), Width: 256, Height: 192, Format: rawvideo.FormatYUY2}
	for _, tc := range []struct {
		name    string
		frame   rawvideo.Frame
		req     *agentpb.StreamVideoRequest
		enc     gstEncoderResult
		fps     uint32
		want    string
		without []string
	}{
		{
			// The thermal camera this exists for, end to end: frames parsed
			// from stdin, then exactly the hub's usual encoding tail.
			name: "yuy2 thermal", frame: thermal, req: &agentpb.StreamVideoRequest{}, enc: x264, fps: 25,
			want: "gst -q fdsrc fd=0 do-timestamp=true ! video/x-raw,format=YUY2,width=256,height=192,framerate=25/1 ! " +
				"rawvideoparse use-sink-caps=true ! " + leakyRawQueue + " ! " +
				encoderSegment("x264enc", true, keyframeIntervalFrames(25)) + " ! fdsink fd=1",
			without: []string{"videoscale", "v4l2src"},
		},
		{
			// GRAY16 is stretched to 8 bits in the agent, so the pipeline
			// reads GRAY8.
			name: "gray16 enters as gray8", frame: rawvideo.Frame{Data: make([]byte, 160*120*2), Width: 160, Height: 120, Format: rawvideo.FormatGRAY16LE},
			req: &agentpb.StreamVideoRequest{}, enc: x264, fps: 9,
			want: "video/x-raw,format=GRAY8,width=160,height=120,framerate=9/1 ! rawvideoparse",
		},
		{
			name: "keyframe interval follows the rate", frame: thermal, req: &agentpb.StreamVideoRequest{}, enc: x264, fps: 15,
			want: "key-int-max=7 ",
		},
		{
			// 4:2:0 encoders need even dimensions.
			name: "odd size rounds up to even", frame: grayFrame(255, 191, 0), req: &agentpb.StreamVideoRequest{}, enc: x264, fps: 25,
			want: "leaky=downstream ! videoscale ! video/x-raw,width=256,height=192 ! videoconvert",
		},
		{
			name: "requested size scales", frame: thermal, req: &agentpb.StreamVideoRequest{Width: 640, Height: 480}, enc: x264, fps: 25,
			want: "videoscale ! video/x-raw,width=640,height=480",
		},
		{
			name: "jetson hardware encoder", frame: thermal, req: &agentpb.StreamVideoRequest{}, enc: gstEncoderResult{element: "nvv4l2h264enc", hasH264Parse: true}, fps: 30,
			want: "nvvidconv ! video/x-raw(memory:NVMM),format=NV12 ! nvv4l2h264enc iframeinterval=15 ! h264parse",
		},
		{
			name: "vp8 when h264parse is missing", frame: thermal, req: &agentpb.StreamVideoRequest{}, enc: gstEncoderResult{element: "vp8enc"}, fps: 25,
			want: "vp8enc deadline=1 keyframe-max-dist=12 ! webmmux streamable=true ! fdsink fd=1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planInProcessPipeline("gst", tc.req, tc.enc, mustInProcessInput(t, tc.frame), tc.fps)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(plan.args, " ")
			if !strings.Contains(args, tc.want) {
				t.Errorf("pipeline\n  %s\nmissing\n  %s", args, tc.want)
			}
			for _, bad := range tc.without {
				if strings.Contains(args, bad) {
					t.Errorf("pipeline %s contains %s", args, bad)
				}
			}
			if plan.raw != nil || plan.rawWhy != inProcessRawWhy {
				t.Errorf("plan promises raw frames: raw=%v why=%q", plan.raw, plan.rawWhy)
			}
		})
	}
	if _, err := planInProcessPipeline("gst", &agentpb.StreamVideoRequest{}, gstEncoderResult{element: "x264enc ! filesink"}, mustInProcessInput(t, thermal), 25); err == nil {
		t.Error("an encoder name carrying pipeline tokens was accepted")
	}
}

func TestInProcessInputPacksRowsTheWayRawvideoparseReadsThem(t *testing.T) {
	// 6 GRAY8 pixels per row: GStreamer's default stride is 8, so each row
	// gains two zero bytes.
	f := rawvideo.Frame{Data: []byte("abcdefghijkl"), Width: 6, Height: 2, Format: rawvideo.FormatGRAY8}
	in := mustInProcessInput(t, f)
	if in.rowBytes != 6 || in.stride != 8 {
		t.Fatalf("rowBytes, stride = %d, %d; want 6, 8", in.rowBytes, in.stride)
	}
	p := inProcessPacker{in: in}
	if got := p.pack(f); string(got) != "abcdef\x00\x00ghijkl\x00\x00" {
		t.Errorf("packed %q", got)
	}

	// Aligned rows go through untouched, without a copy.
	thermal := rawvideo.Frame{Data: make([]byte, 256*192*2), Width: 256, Height: 192, Format: rawvideo.FormatYUY2}
	p = inProcessPacker{in: mustInProcessInput(t, thermal)}
	if got := p.pack(thermal); &got[0] != &thermal.Data[0] {
		t.Error("an aligned frame was copied")
	}

	// GRAY16 is stretched first, then padded as GRAY8: 5 pixels -> stride 8.
	g16 := rawvideo.Frame{Data: make([]byte, 5*1*2), Width: 5, Height: 1, Format: rawvideo.FormatGRAY16LE}
	for i, v := range []uint16{1000, 1100, 1200, 1300, 1400} {
		binary.LittleEndian.PutUint16(g16.Data[2*i:], v)
	}
	in = mustInProcessInput(t, g16)
	if in.gstFormat != rawvideo.FormatGRAY8 || in.stride != 8 {
		t.Fatalf("gray16 input = %+v", in)
	}
	p = inProcessPacker{in: in}
	if got := p.pack(g16); !bytes.Equal(got, []byte{0, 64, 128, 191, 255, 0, 0, 0}) {
		t.Errorf("gray16 packed to %v", got)
	}

	if _, err := inProcessInputFor(rawvideo.Frame{Data: []byte{1}, Width: 2, Height: 1, Format: rawvideo.FormatGRAY8}); err == nil {
		t.Error("a malformed first frame was accepted")
	}
}

func TestInProcessFramerate(t *testing.T) {
	for interval, want := range map[time.Duration]uint32{
		0:                           defaultInProcessFramerate,
		-time.Millisecond:           defaultInProcessFramerate,
		40 * time.Millisecond:       25,
		66 * time.Millisecond:       15,
		time.Millisecond:            maxInProcessFramerate,
		5 * time.Second:             1,
		1000 / 9 * time.Millisecond: 9,
	} {
		if got := inProcessFramerate(interval); got != want {
			t.Errorf("inProcessFramerate(%v) = %d, want %d", interval, got, want)
		}
	}
}

func TestCheckInProcessRequest(t *testing.T) {
	if err := checkInProcessRequest(&agentpb.StreamVideoRequest{Framerate: 30}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("framerate: %v", err)
	}
	if err := checkInProcessRequest(&agentpb.StreamVideoRequest{Width: 320}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("width alone: %v", err)
	}
	if err := checkInProcessRequest(&agentpb.StreamVideoRequest{Width: 320, Height: 240}); err != nil {
		t.Errorf("size: %v", err)
	}
	// The native size is accepted even though no allowlist has heard of it,
	// and so is a common size to scale to; anything else is refused.
	if err := checkInProcessSize(&agentpb.StreamVideoRequest{Width: 256, Height: 192}, 256, 192); err != nil {
		t.Errorf("native size: %v", err)
	}
	if err := checkInProcessSize(&agentpb.StreamVideoRequest{Width: 640, Height: 480}, 256, 192); err != nil {
		t.Errorf("scaled size: %v", err)
	}
	if err := checkInProcessSize(&agentpb.StreamVideoRequest{Width: 300, Height: 200}, 256, 192); status.Code(err) != codes.InvalidArgument {
		t.Errorf("arbitrary size: %v", err)
	}
}

func shortenInProcessStall(t *testing.T, d time.Duration) {
	t.Helper()
	prev := inProcessFrameStall
	inProcessFrameStall = d
	t.Cleanup(func() { inProcessFrameStall = prev })
}

func TestPumpInProcessFramesEnds(t *testing.T) {
	shortenInProcessStall(t, 50*time.Millisecond)
	first := grayFrame(4, 1, 'a')
	in := mustInProcessInput(t, first)

	t.Run("format change", func(t *testing.T) {
		frames := make(chan rawvideo.Frame, 2)
		frames <- grayFrame(4, 1, 'b')
		frames <- grayFrame(6, 1, 'c')
		var out bytes.Buffer
		err := pumpInProcessFrames(context.Background(), &out, frames, nil, in, []rawvideo.Frame{first})
		if err == nil || !strings.Contains(err.Error(), "changed from 4x1 GRAY8 to 6x1 GRAY8") {
			t.Errorf("err = %v", err)
		}
		if out.String() != "aaaabbbb" {
			t.Errorf("wrote %q, want the frames before the change", out.String())
		}
	})
	t.Run("malformed frame", func(t *testing.T) {
		frames := make(chan rawvideo.Frame, 1)
		bad := grayFrame(4, 1, 'b')
		bad.Data = bad.Data[:3]
		frames <- bad
		err := pumpInProcessFrames(context.Background(), io.Discard, frames, nil, in, []rawvideo.Frame{first})
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("silence", func(t *testing.T) {
		err := pumpInProcessFrames(context.Background(), io.Discard, make(chan rawvideo.Frame), nil, in, []rawvideo.Frame{first})
		if err == nil || !strings.Contains(err.Error(), "no frame for") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unregistered", func(t *testing.T) {
		gone := make(chan struct{})
		close(gone)
		err := pumpInProcessFrames(context.Background(), io.Discard, make(chan rawvideo.Frame), gone, in, []rawvideo.Frame{first})
		if !errors.Is(err, errInProcessCameraGone) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := pumpInProcessFrames(ctx, io.Discard, make(chan rawvideo.Frame), nil, in, []rawvideo.Frame{first})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRegisterInProcessCamera(t *testing.T) {
	s := &VideoService{}
	feed := rawvideo.NewFeed()
	if _, err := s.RegisterInProcessCamera(1, nil); err == nil {
		t.Error("nil feed accepted")
	}
	if _, err := s.RegisterInProcessCamera(maxVideoDeviceID+1, feed); err == nil {
		t.Error("an ID StreamVideo refuses was accepted")
	}
	unregister, err := s.RegisterInProcessCamera(inProcessTestID, feed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterInProcessCamera(inProcessTestID, rawvideo.NewFeed()); err == nil {
		t.Error("a second camera took a registered ID")
	}
	if cam, ok := s.inProcessCameraForKey(inProcessHubKey(inProcessTestID)); !ok || cam.feed != feed {
		t.Fatal("hub key does not resolve to the registered camera")
	}
	unregister()
	unregister() // idempotent
	if _, ok := s.inProcessCameraByID(inProcessTestID); ok {
		t.Fatal("camera still registered")
	}
	again, err := s.RegisterInProcessCamera(inProcessTestID, feed)
	if err != nil {
		t.Fatalf("re-register after unregister: %v", err)
	}
	// A stale unregister must not remove the new registration.
	unregister()
	if _, ok := s.inProcessCameraByID(inProcessTestID); !ok {
		t.Fatal("a stale unregister removed a newer registration")
	}
	again()
}

// fakeGStreamer installs gst-inspect-1.0 and gst-launch-1.0 stand-ins on PATH.
// The launcher appends its arguments to the returned file, one line per run,
// then runs body. With body "exec /bin/cat" the "encoded" stream is exactly the
// bytes written to the pipeline, which is what the tests below check.
func fakeGStreamer(t *testing.T, body string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "launches")
	for name, script := range map[string]string{
		"gst-inspect-1.0": "echo 'x264: x264enc: H.264 encoder'\n" +
			"echo 'videoparsersbad: h264parse: H.264 parser'\n" +
			"echo 'rawparse: rawvideoparse: Raw video parser'\n",
		"gst-launch-1.0": "echo \"$*\" >> '" + argsFile + "'\n" + body + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	return argsFile
}

func launches(t *testing.T, argsFile string) []string {
	t.Helper()
	b, err := os.ReadFile(argsFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// publishUntil publishes next(i) every few milliseconds until ctx ends.
func publishUntil(ctx context.Context, feed *rawvideo.Feed, next func(i int) rawvideo.Frame) {
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				feed.Publish(next(i))
			}
		}
	}()
}

// inProcessViewer runs StreamVideo for the test camera until its context ends.
type inProcessViewer struct {
	stream *fakeVideoStream
	cancel context.CancelFunc
	done   chan error
}

func startInProcessViewer(s *VideoService, req *agentpb.StreamVideoRequest) *inProcessViewer {
	ctx, cancel := context.WithCancel(context.Background())
	v := &inProcessViewer{stream: newFakeVideoStream(ctx), cancel: cancel, done: make(chan error, 1)}
	if req == nil {
		req = &agentpb.StreamVideoRequest{}
	}
	req.DeviceId = inProcessTestID
	go func() { v.done <- s.StreamVideo(req, v.stream) }()
	return v
}

func (v *inProcessViewer) received() []byte {
	v.stream.mu.Lock()
	defer v.stream.mu.Unlock()
	return bytes.Join(v.stream.frames, nil)
}

func (v *inProcessViewer) chunks() int {
	v.stream.mu.Lock()
	defer v.stream.mu.Unlock()
	return len(v.stream.frames)
}

func (v *inProcessViewer) waitBytes(t *testing.T, n int) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := v.received(); len(got) >= n {
			return got
		}
		select {
		case err := <-v.done:
			t.Fatalf("stream ended after %d bytes: %v", len(v.received()), err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("only %d of %d bytes arrived", len(v.received()), n)
	return nil
}

func (v *inProcessViewer) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-v.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("StreamVideo did not return")
		return nil
	}
}

func newInProcessTestService(t *testing.T) (*VideoService, *rawvideo.Feed, func()) {
	t.Helper()
	s := NewVideoService(context.Background(), zap.NewNop(), nil)
	t.Cleanup(s.Shutdown)
	feed := rawvideo.NewFeed()
	unregister, err := s.RegisterInProcessCamera(inProcessTestID, feed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unregister)
	return s, feed, unregister
}

func waitUnwatched(t *testing.T, s *VideoService, feed *rawvideo.Feed) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, live := s.hubs[inProcessHubKey(inProcessTestID)]
		s.mu.Unlock()
		if !live && !feed.Watched() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("producer still running after the last viewer left")
}

// TestInProcessCameraLiveView is the end-to-end path: StreamVideo on a
// registered ID starts one pipeline fed from the feed, without touching any
// /dev/video node, delivers what the pipeline encodes, shares it between
// viewers, and stops it when the last viewer leaves.
func TestInProcessCameraLiveView(t *testing.T) {
	argsFile := fakeGStreamer(t, "exec /bin/cat")
	s, feed, _ := newInProcessTestService(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	// 6-pixel rows, so the bytes the pipeline reads must carry GStreamer's
	// row padding: "AAAAAA\0\0" per row.
	publishUntil(ctx, feed, func(int) rawvideo.Frame { return grayFrame(6, 2, 'A') })

	first := startInProcessViewer(s, nil)
	got := first.waitBytes(t, 32)
	if !bytes.HasPrefix(got, []byte("AAAAAA\x00\x00AAAAAA\x00\x00AAAAAA\x00\x00")) {
		t.Fatalf("pipeline input = %q", got[:32])
	}
	second := startInProcessViewer(s, nil)
	second.waitBytes(t, 16)

	if l := launches(t, argsFile); len(l) != 1 {
		t.Fatalf("pipelines started = %d, want one shared by both viewers: %q", len(l), l)
	} else if !strings.Contains(l[0], "fdsrc fd=0") || !strings.Contains(l[0], "video/x-raw,format=GRAY8,width=6,height=2,framerate=") ||
		strings.Contains(l[0], "/dev/video") {
		t.Fatalf("pipeline = %s", l[0])
	}

	first.cancel()
	if err := first.wait(t); !errors.Is(err, context.Canceled) {
		t.Errorf("first viewer: %v", err)
	}
	second.waitBytes(t, len(second.received())+16) // still streaming
	second.cancel()
	_ = second.wait(t)
	waitUnwatched(t, s, feed)
}

func TestInProcessCameraFormatChangeEndsTheStream(t *testing.T) {
	argsFile := fakeGStreamer(t, "exec /bin/cat")
	s, feed, _ := newInProcessTestService(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var mu sync.Mutex
	width := 6
	publishUntil(ctx, feed, func(int) rawvideo.Frame {
		mu.Lock()
		defer mu.Unlock()
		return grayFrame(width, 2, 'A')
	})

	v := startInProcessViewer(s, nil)
	v.waitBytes(t, 16)
	mu.Lock()
	width = 8
	mu.Unlock()
	err := v.wait(t)
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "changed from 6x2 GRAY8 to 8x2 GRAY8") {
		t.Fatalf("format change: %v", err)
	}

	// The next viewer gets a fresh pipeline built for the new format.
	again := startInProcessViewer(s, nil)
	again.waitBytes(t, 16)
	l := launches(t, argsFile)
	if len(l) != 2 || !strings.Contains(l[1], "format=GRAY8,width=8,height=2") {
		t.Fatalf("launches after the change = %q", l)
	}
	again.cancel()
	_ = again.wait(t)
}

func TestInProcessCameraEncoderCrash(t *testing.T) {
	argsFile := fakeGStreamer(t, "/usr/bin/head -c 16\necho 'ERROR: from element x264enc0: encoder exploded' >&2\nexit 1")
	s, feed, _ := newInProcessTestService(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	publishUntil(ctx, feed, func(int) rawvideo.Frame { return grayFrame(8, 2, 'A') })

	v := startInProcessViewer(s, nil)
	err := v.wait(t)
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "encoder exploded") {
		t.Fatalf("crash: %v", err)
	}
	if len(v.received()) == 0 {
		t.Error("output before the crash was not delivered")
	}
	// The crashed producer is gone; the next viewer starts a new pipeline.
	again := startInProcessViewer(s, nil)
	_ = again.wait(t)
	if l := launches(t, argsFile); len(l) != 2 {
		t.Fatalf("pipelines started = %d, want a restart for the second viewer", len(l))
	}
}

func TestInProcessCameraUnregisterEndsTheStream(t *testing.T) {
	fakeGStreamer(t, "exec /bin/cat")
	s, feed, unregister := newInProcessTestService(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	publishUntil(ctx, feed, func(int) rawvideo.Frame { return grayFrame(8, 2, 'A') })

	v := startInProcessViewer(s, nil)
	v.waitBytes(t, 16)
	unregister()
	if err := v.wait(t); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("unregistered camera: %v", err)
	}
	waitUnwatched(t, s, feed)

	// With the camera gone the ID is an ordinary V4L2 number again.
	after := startInProcessViewer(s, nil)
	if err := after.wait(t); status.Code(err) != codes.NotFound {
		t.Fatalf("after unregister: %v", err)
	}
}

// TestInProcessCameraStalledEncoderNeverBlocksTheDriver: an encoder that stops
// reading must not stall the driver's capture loop. Frames are large enough
// that two fill the pipe; the driver publishes many more.
func TestInProcessCameraStalledEncoderNeverBlocksTheDriver(t *testing.T) {
	fakeGStreamer(t, "printf encoded\nexec /bin/sleep 30")
	s, feed, _ := newInProcessTestService(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	frame := grayFrame(256, 192, 'A')
	publishUntil(ctx, feed, func(int) rawvideo.Frame {
		f := frame
		f.Timestamp = time.Now()
		return f
	})

	v := startInProcessViewer(s, nil)
	v.waitBytes(t, len("encoded"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			f := frame // the background publisher reads frame too
			f.Timestamp = time.Now()
			feed.Publish(f)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked behind a stalled encoder")
	}
	v.cancel()
	_ = v.wait(t)
	waitUnwatched(t, s, feed)
}

func TestInProcessCameraRefusals(t *testing.T) {
	fakeGStreamer(t, "exec /bin/cat")
	shortenInProcessStall(t, 50*time.Millisecond)
	s, _, _ := newInProcessTestService(t)

	err := startInProcessViewer(s, &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW}).wait(t)
	if status.Code(err) != codes.FailedPrecondition || !streamreason.Has(err, streamreason.RawUnavailable) {
		t.Errorf("raw request: %v", err)
	}
	if err := startInProcessViewer(s, &agentpb.StreamVideoRequest{Framerate: 30}).wait(t); status.Code(err) != codes.InvalidArgument {
		t.Errorf("framerate request: %v", err)
	}
	s.mu.Lock()
	hubs := len(s.hubs)
	s.mu.Unlock()
	if hubs != 0 {
		t.Errorf("a refused request started %d producer(s)", hubs)
	}

	// Registered but silent: reported, not waited on forever.
	if err := startInProcessViewer(s, nil).wait(t); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "is not streaming") {
		t.Errorf("silent camera: %v", err)
	}
}

// TestInProcessCameraGStreamer runs the real pipeline: GStreamer, the encoder
// this host would pick, and a decode of what comes out. Skipped where GStreamer
// or the decoder is not installed (CI); run it on a development machine or a
// device to prove the caps, the row layout and the GRAY16 stretch end to end.
func TestInProcessCameraGStreamer(t *testing.T) {
	gst, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		t.Skip("GStreamer is not installed")
	}
	for _, element := range []string{"rawvideoparse", "h264parse", "avdec_h264"} {
		if exec.Command("gst-inspect-1.0", element).Run() != nil {
			t.Skipf("GStreamer element %s is not installed", element)
		}
	}
	for _, tc := range []struct {
		name          string
		frame         func(i int) rawvideo.Frame
		width, height int
		check         func(t *testing.T, picture []byte)
	}{
		{
			// The TC001 Max shape: grey in the low byte, neutral chroma.
			name: "yuy2 256x192", width: 256, height: 192,
			frame: func(int) rawvideo.Frame {
				f := rawvideo.Frame{Data: make([]byte, 256*192*2), Width: 256, Height: 192, Format: rawvideo.FormatYUY2, Timestamp: time.Now()}
				for y := 0; y < 192; y++ {
					for x := 0; x < 256; x++ {
						f.Data[(y*256+x)*2], f.Data[(y*256+x)*2+1] = byte(x), 0x80
					}
				}
				return f
			},
			check: func(t *testing.T, picture []byte) {
				// A left-to-right ramp survives: dark left, bright right.
				if l, r := picture[96*256+8], picture[96*256+248]; l > 40 || r < 210 {
					t.Errorf("ramp decoded as left=%d right=%d", l, r)
				}
			},
		},
		{
			// Narrow-band 16-bit values: without the stretch this is flat grey.
			name: "gray16 stretched", width: 160, height: 120,
			frame: func(int) rawvideo.Frame {
				f := rawvideo.Frame{Data: make([]byte, 160*120*2), Width: 160, Height: 120, Format: rawvideo.FormatGRAY16LE, Timestamp: time.Now()}
				for y := 0; y < 120; y++ {
					for x := 0; x < 160; x++ {
						binary.LittleEndian.PutUint16(f.Data[(y*160+x)*2:], uint16(18000+x))
					}
				}
				return f
			},
			check: func(t *testing.T, picture []byte) {
				lo, hi := byte(255), byte(0)
				for _, v := range picture {
					lo, hi = min(lo, v), max(hi, v)
				}
				if hi-lo < 200 {
					t.Errorf("decoded range %d..%d; the 16-bit band was not stretched", lo, hi)
				}
			},
		},
		{
			// Odd size and unaligned rows: decoded at the next even size.
			name: "gray8 255x191", width: 256, height: 192,
			frame: func(i int) rawvideo.Frame { return grayFrame(255, 191, byte(64+i%128)) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewVideoService(context.Background(), zap.NewNop(), nil)
			defer s.Shutdown()
			feed := rawvideo.NewFeed()
			unregister, err := s.RegisterInProcessCamera(inProcessTestID, feed)
			if err != nil {
				t.Fatal(err)
			}
			defer unregister()
			ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			go func() {
				ticker := time.NewTicker(40 * time.Millisecond)
				defer ticker.Stop()
				for i := 0; ; i++ {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						feed.Publish(tc.frame(i))
					}
				}
			}()

			v := startInProcessViewer(s, nil)
			// Wait for several encoded frames, then stop like a viewer does.
			deadline := time.Now().Add(20 * time.Second)
			for v.chunks() < 15 && time.Now().Before(deadline) {
				select {
				case err := <-v.done:
					t.Fatalf("stream ended: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			v.cancel()
			_ = v.wait(t)
			encoded := v.received()
			if len(encoded) == 0 {
				t.Fatal("no video")
			}

			decode := exec.CommandContext(ctx, gst, "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!",
				"videoconvert", "!", "video/x-raw,format=GRAY8", "!", "fdsink", "fd=1")
			decode.Stdin = bytes.NewReader(encoded)
			var stderr bytes.Buffer
			decode.Stderr = &stderr
			decoded, err := decode.Output()
			if err != nil {
				t.Fatalf("decode: %v: %s", err, stderr.String())
			}
			// GRAY8 rows of an even width that is a multiple of 4 carry no
			// padding, so the output is whole pictures back to back.
			size := tc.width * tc.height
			if len(decoded) == 0 || len(decoded)%size != 0 {
				t.Fatalf("decoded %d bytes, not a whole number of %dx%d pictures", len(decoded), tc.width, tc.height)
			}
			if tc.check != nil {
				tc.check(t, decoded[len(decoded)-size:])
			}
		})
	}
}
