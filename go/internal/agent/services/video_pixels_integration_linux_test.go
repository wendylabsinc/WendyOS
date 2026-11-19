//go:build linux && regression_camera_fixture

package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	pixelsFixtureWidth  = 320
	pixelsFixtureHeight = 240
	pixelsFixtureFPS    = 15
)

// An MJPEG camera through the agent's own producer: a v4l2loopback node fed
// JPEG frames stands in for the camera, and everything from v4l2src on is the
// production path (runProducer, the shaped GStreamer pipelines, the tap pump,
// the hub). It proves what the unit tests cannot: that real GStreamer hands
// in-agent consumers decoded frames, runs the encoder only while a viewer
// watches, and keeps encoding while nobody wants the tap's frames and the pump
// only drains it at its idle rate.
//
// The CI media job runs it after merge; on a device it runs as root beside the
// agent, so it creates its own node below the agent's bands (96-255) and never
// builds the service with NewVideoService, which loads the agent's camera
// registry and removes loopback nodes it thinks are stale.
func TestMJPEGCameraPixelsAndEncoderOnDemand(t *testing.T) {
	unavailable := func(format string, args ...any) {
		t.Helper()
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatalf("required "+format, args...)
		}
		t.Skipf(format+" (set WENDY_GSTREAMER_REQUIRED=1 to fail instead of skipping)", args...)
	}
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		unavailable("GStreamer is not installed: %v", err)
	}
	inspect, err := exec.LookPath("gst-inspect-1.0")
	if err != nil {
		unavailable("GStreamer is not installed: %v", err)
	}
	for _, element := range []string{"v4l2src", "jpegdec", "videoconvert", "tee", "queue", "x264enc", "h264parse", "avdec_h264", "fdsrc", "fdsink"} {
		if out, err := exec.Command(inspect, element).CombinedOutput(); err != nil {
			unavailable("GStreamer element %s is unavailable: %v: %s", element, err, bytes.TrimSpace(out))
		}
	}
	if os.Geteuid() != 0 {
		unavailable("root, to create a v4l2loopback node")
	}
	// Checked here so the loopback manager below never tries to load the module.
	if info, err := os.Stat("/dev/v4l2loopback"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		unavailable("loaded v4l2loopback module (/dev/v4l2loopback): %v", err)
	}
	if version, err := os.ReadFile("/sys/module/v4l2loopback/version"); err != nil || !strings.HasPrefix(strings.TrimSpace(string(version)), "0.15.") {
		unavailable("v4l2loopback 0.15.x, whose control ABI ipcam uses: version=%q err=%v", bytes.TrimSpace(version), err)
	}

	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	// Cleanups run last to first. This one runs last: the agent's own account
	// of what it did, once everything has stopped.
	t.Cleanup(func() {
		if t.Failed() {
			for _, entry := range logs.All() {
				t.Logf("agent log: %s %s %v", entry.Level, entry.Message, entry.ContextMap())
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	path := createPixelsFixtureNode(t, ctx, logger)
	feed := feedMJPEG(t, ctx, path, redGreenJPEG(t, pixelsFixtureWidth, pixelsFixtureHeight))
	// The producer chooses MJPEG capture only once the node advertises the
	// format at its default size, which it does after the first write.
	deadline := time.Now().Add(5 * time.Second)
	for {
		w, h := bestDefaultFrameSizeForDevice(path)
		if w == pixelsFixtureWidth && h == pixelsFixtureHeight && deviceSupportsMJPEGSize(path, w, h) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become a %dx%d MJPEG camera: default size %dx%d; feed: %v", path, pixelsFixtureWidth, pixelsFixtureHeight, w, h, feed.err())
		}
		time.Sleep(20 * time.Millisecond)
	}

	svcCtx, svcCancel := context.WithCancel(ctx)
	var tapReads atomic.Int64 // whole frames the tap pumps have read off fd 3
	svc := &VideoService{
		tapFrameRead:      func(string) { tapReads.Add(1) },
		logger:            logger,
		ctx:               svcCtx,
		cancel:            svcCancel,
		hubs:              map[string]*deviceHub{},
		sampleSeqs:        map[string]*atomic.Uint64{},
		encoderIdleGrace:  300 * time.Millisecond,
		classifyTransport: camera.Classify,
		readDeviceName: func(base string) (string, error) {
			b, err := os.ReadFile(filepath.Join("/sys/class/video4linux", base, "name"))
			return strings.TrimSpace(string(b)), err
		},
		// A node busy at start must fail this test, not reach the device's
		// PipeWire session through the contention fallback.
		findCameraSource: func(context.Context, string) (uint64, bool) { return 0, false },
	}
	// NewVideoService's wiring of the producer.
	svc.startProducer = func(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) {
		svc.wg.Add(1)
		go svc.runProducer(ctx, h, path, req)
	}
	// Runs first: every pipeline exits and releases the node, then the feed
	// closes it, and only then is the node removed.
	t.Cleanup(func() {
		svc.Shutdown()
		if err := feed.stop(); err != nil {
			t.Errorf("MJPEG feed failed: %v", err)
		}
		var launches []any
		for _, entry := range logs.FilterMessage("starting camera pipeline").All() {
			launches = append(launches, entry.ContextMap()["encodes"])
		}
		t.Logf("pipeline launches, whether each encodes: %v", launches)
	})

	// Step 2: a pixel subscriber alone gets decoded frames of the picture.
	hub, pixelID, pixelCh, err := svc.joinHubPixels(ctx, path, &agentpb.StreamVideoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	pixels := drainHubFrames(ctx, pixelCh)
	first, problem := pixels.next(ctx, 10*time.Second, time.Time{})
	if problem == "" {
		problem = pixelFrameProblem(first)
	}
	if problem != "" {
		t.Fatalf("decoded frame does not show the source picture: %s; hub error: %v; feed: %v", problem, hub.terminalErr(), feed.err())
	}
	left, right := fixtureLuma(first)
	t.Logf("pixel subscriber received a %dx%d %s frame showing the source picture: luma %d at (10,120), %d at (310,120)",
		first.rawFmt.GetWidth(), first.rawFmt.GetHeight(), first.rawFmt.GetFourcc(), left, right)

	// Step 3: nobody wants encoded video, so the pipeline has no encoder.
	if hub.pipelineEncodes() {
		t.Fatal("pipeline is encoding with no encoded subscriber")
	}

	// Step 4: a viewer arrives; the pipeline restarts with the encoder.
	viewerHub, viewerID, viewerCh, err := svc.joinHub(ctx, path, &agentpb.StreamVideoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if viewerHub != hub {
		t.Fatal("viewer did not join the camera's running hub")
	}
	viewer := drainHubFrames(ctx, viewerCh)
	stream, problem := viewer.h264Keyframes(ctx, 10*time.Second, 2)
	if problem != "" {
		t.Fatalf("viewer did not receive H.264: %s; hub error: %v", problem, hub.terminalErr())
	}
	starts := annexBKeyframeStarts(stream)
	// One whole keyframe interval: both ends are access-unit boundaries.
	decoded, stderr, err := decodeH264ToRGB(ctx, launch, nil, stream[starts[0]:starts[1]])
	const rgbFrameBytes = pixelsFixtureWidth * pixelsFixtureHeight * 3
	if err != nil || len(decoded) < rgbFrameBytes || len(decoded)%rgbFrameBytes != 0 {
		t.Fatalf("viewer's H.264 did not decode to %dx%d frames: bytes=%d err=%v stderr=%s", pixelsFixtureWidth, pixelsFixtureHeight, len(decoded), err, stderr)
	}
	if err := redGreenMismatch(decoded, pixelsFixtureWidth, pixelsFixtureHeight); err != nil {
		t.Fatalf("viewer's H.264 does not show the source picture: %v", err)
	}
	t.Logf("viewer's H.264 decoded to %d %dx%d frames showing the source picture", len(decoded)/rgbFrameBytes, pixelsFixtureWidth, pixelsFixtureHeight)
	if !hub.pipelineEncodes() {
		t.Fatal("pipeline is not encoding with a viewer")
	}
	// Published after H.264 first arrived, so it came from the encoding pipeline.
	frame, problem := pixels.next(ctx, 5*time.Second, viewer.firstAt())
	if problem == "" {
		problem = pixelFrameProblem(frame)
	}
	if problem != "" {
		t.Fatalf("pixel subscriber lost the picture across the encoder restart: %s; hub error: %v", problem, hub.terminalErr())
	}

	// Step 5: nobody wants the tap's frames now. The pump drains it at its idle
	// rate, far below the camera's, and the encoder must not notice.
	hub.unsubscribe(pixelID)
	pixels.stop()
	const window = 3 * time.Second
	before, beforeBytes := viewer.counts()
	readsBefore := tapReads.Load()
	time.Sleep(window)
	drained := tapReads.Load() - readsBefore
	after, afterBytes := viewer.counts()
	// x264enc writes an access unit delimiter ahead of every picture.
	pictures := bytes.Count(viewer.streamSince(beforeBytes)[:afterBytes-beforeBytes], []byte{0, 0, 1, 9})
	t.Logf("with nobody wanting the tap, the viewer received %d chunks, %d bytes, %d pictures in %s (the feed writes %d)",
		after-before, afterBytes-beforeBytes, pictures, window, int(window/time.Second)*pixelsFixtureFPS)
	if after-before < 10 {
		t.Fatalf("an unread tap stalled the encoded branch: %d chunks in %s; hub error: %v", after-before, window, hub.terminalErr())
	}
	// One frame per tapIdleDrainEvery, plus one the pump may have been reading
	// for the pixel subscriber as it left and one for timer slack.
	maxDrained := int64(window/tapIdleDrainEvery) + 2
	t.Logf("with nobody wanting the tap, the pump read %d frames off it in %s (idle rate: one every %s, at most %d; the feed writes %d)",
		drained, window, tapIdleDrainEvery, maxDrained, int(window/time.Second)*pixelsFixtureFPS)
	if drained > maxDrained {
		t.Fatalf("the pump read %d tap frames in %s with nobody wanting them, more than the idle rate allows (%d)", drained, window, maxDrained)
	}
	// A quarter of the idle rate: a pump that stopped reading altogether is the
	// stall this rate exists to prevent.
	if minDrained := int64(window/tapIdleDrainEvery) / 4; drained < minDrained {
		t.Fatalf("the pump read %d tap frames in %s with nobody wanting them, want at least %d: an idle tap must still be drained", drained, window, minDrained)
	}

	// Step 6: pixels again, then the viewer leaves: after the grace the
	// pipeline drops the encoder and the pixel subscriber keeps its frames.
	againHub, againID, againCh, err := svc.joinHubPixels(ctx, path, &agentpb.StreamVideoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if againHub != hub {
		t.Fatal("pixel subscriber did not join the camera's running hub")
	}
	defer againHub.unsubscribe(againID)
	again := drainHubFrames(ctx, againCh)
	defer again.stop()
	hub.unsubscribe(viewerID)
	viewer.stop()
	deadline = time.Now().Add(5 * time.Second)
	for hub.pipelineEncodes() {
		if time.Now().After(deadline) {
			t.Fatalf("pipeline kept the encoder 5 s after the last viewer left; grace is %s", svc.encoderIdleGrace)
		}
		time.Sleep(20 * time.Millisecond)
	}
	dropped := time.Now()
	for i := range 5 {
		frame, problem := again.next(ctx, 5*time.Second, dropped)
		if problem == "" {
			problem = pixelFrameProblem(frame)
		}
		if problem != "" {
			t.Fatalf("pixel frame %d after the encoder was dropped: %s; hub error: %v", i+1, problem, hub.terminalErr())
		}
		dropped = time.Unix(0, int64(frame.tsNs))
	}
	t.Log("pixel frames kept arriving after the encoder was dropped")
}

// createPixelsFixtureNode creates a v4l2loopback node with a label of its own,
// at the highest free number from 90 down, and removes it when the test ends.
// Nothing at 96 or above is touched: those are the agent's camera bands.
func createPixelsFixtureNode(t *testing.T, ctx context.Context, logger *zap.Logger) string {
	t.Helper()
	return createFixtureNode(t, ctx, logger, false)
}

// createShareFixtureNode is createPixelsFixtureNode with the production share
// node shape: announce_all_caps=1, as Loopback.EnsureShareNode creates it.
func createShareFixtureNode(t *testing.T, ctx context.Context, logger *zap.Logger) string {
	t.Helper()
	return createFixtureNode(t, ctx, logger, true)
}

func createFixtureNode(t *testing.T, ctx context.Context, logger *zap.Logger, share bool) string {
	t.Helper()
	nr := -1
	for candidate := 90; candidate >= 64; candidate-- {
		_, devErr := os.Lstat(fmt.Sprintf("/dev/video%d", candidate))
		_, sysErr := os.Lstat(fmt.Sprintf("/sys/class/video4linux/video%d", candidate))
		if errors.Is(devErr, os.ErrNotExist) && errors.Is(sysErr, os.ErrNotExist) {
			nr = candidate
			break
		}
	}
	if nr < 0 {
		t.Fatal("no free v4l2loopback node number between 64 and 90")
	}
	path := fmt.Sprintf("/dev/video%d", nr)
	label := fmt.Sprintf("wendy-test-pixels-%d", os.Getpid())
	nameFile := fmt.Sprintf("/sys/class/video4linux/video%d/name", nr)
	ours := func() bool {
		name, err := os.ReadFile(nameFile)
		return err == nil && strings.TrimSpace(string(name)) == label
	}
	loopback := ipcam.NewLoopback(ctx, logger, nil, nil, nil)
	ensure := loopback.EnsureAuxNode
	if share {
		ensure = loopback.EnsureShareNode
	}
	if err := ensure(ctx, nr, label); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	// EnsureAuxNode accepts a node someone else created meanwhile; the label
	// says whether this one is ours to remove.
	if !ours() {
		t.Fatalf("%s is not the node this test created (label %q)", path, label)
	}
	t.Cleanup(func() {
		if !ours() {
			t.Errorf("%s changed label before removal; leaving it", path)
			return
		}
		loopback.RemoveAuxNode(nr)
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				t.Logf("removed %s", path)
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%s remained after removal", path)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear after creating the node", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("created %s (%s)", path, label)
	return path
}

// mjpegFeed writes the fixture's JPEG frames into the node.
type mjpegFeed struct {
	cancel context.CancelFunc
	done   chan struct{}
	result error // set before done is closed
}

// feedMJPEG writes jpeg to the node at pixelsFixtureFPS through the ROS 2 camera
// writer until stopped or the writer fails.
func feedMJPEG(t *testing.T, ctx context.Context, path string, jpeg []byte) *mjpegFeed {
	t.Helper()
	feedCtx, cancel := context.WithCancel(ctx)
	feed := &mjpegFeed{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(feed.done)
		writer := ros2camera.NewFrameWriter(path)
		frame := ros2camera.Frame{Data: jpeg, Width: pixelsFixtureWidth, Height: pixelsFixtureHeight, Codec: ros2camera.CodecMJPEG}
		ticker := time.NewTicker(time.Second / pixelsFixtureFPS)
		defer ticker.Stop()
		var err error
		for err == nil {
			if err = writer.WriteFrame(frame); err != nil {
				break
			}
			select {
			case <-feedCtx.Done():
				err = feedCtx.Err()
			case <-ticker.C:
			}
		}
		if closeErr := writer.Close(); closeErr != nil && errors.Is(err, context.Canceled) {
			err = closeErr
		}
		if !errors.Is(err, context.Canceled) {
			feed.result = err
		}
	}()
	t.Cleanup(func() { _ = feed.stop() })
	return feed
}

// stop ends the feed, closes the node and reports why the feed ended early, if
// it did. It may be called more than once.
func (f *mjpegFeed) stop() error {
	f.cancel()
	<-f.done
	return f.result
}

// err reports why the feed ended, without stopping it: nil while it runs.
func (f *mjpegFeed) err() error {
	select {
	case <-f.done:
		if f.result == nil {
			return errors.New("stopped")
		}
		return f.result
	default:
		return nil
	}
}

// hubFrames drains one subscriber's channel as the hub fills it, so the hub
// never drops frames for this subscriber, and keeps what the steps ask about.
type hubFrames struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	changed  chan struct{}        // closed and replaced on every frame
	frames   []*videoFrame        // the latest raw frames, oldest first
	codecs   []agentpb.VideoCodec // each encoded codec seen, once
	stream   []byte               // encoded bytes, in order
	chunks   int
	first    time.Time // when the first encoded chunk arrived
	finished bool      // the hub closed the channel
}

// keptRawFrames bounds what a drain holds. next asks for a frame published
// after some moment, and the newest frames are the ones that can answer it.
const keptRawFrames = 8

func drainHubFrames(ctx context.Context, ch chan *videoFrame) *hubFrames {
	ctx, cancel := context.WithCancel(ctx)
	f := &hubFrames{cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
	go func() {
		defer close(f.done)
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-ch:
				f.mu.Lock()
				if !ok {
					f.finished = true
				} else if frame.codec == agentpb.VideoCodec_VIDEO_CODEC_RAW {
					if len(f.frames) == keptRawFrames {
						f.frames = append(f.frames[:0], f.frames[1:]...)
					}
					f.frames = append(f.frames, frame)
				} else {
					if f.chunks == 0 {
						f.first = time.Now()
					}
					if !slices.Contains(f.codecs, frame.codec) {
						f.codecs = append(f.codecs, frame.codec)
					}
					f.chunks++
					f.stream = append(f.stream, frame.data...)
				}
				close(f.changed)
				f.changed = make(chan struct{})
				f.mu.Unlock()
				if !ok {
					return
				}
			}
		}
	}()
	return f
}

// stop ends the drain. Call it after unsubscribing: the hub does not close the
// channel of a subscriber that left.
func (f *hubFrames) stop() {
	f.cancel()
	<-f.done
}

// waitFor calls check with the state held until it reports done, the hub
// closes the channel, or within passes. It returns why it gave up, or "".
func (f *hubFrames) waitFor(ctx context.Context, within time.Duration, what string, check func() bool) string {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		f.mu.Lock()
		if check() {
			f.mu.Unlock()
			return ""
		}
		finished, changed := f.finished, f.changed
		f.mu.Unlock()
		if finished {
			return "the hub closed the subscription before " + what
		}
		select {
		case <-changed:
		case <-timer.C:
			return fmt.Sprintf("no %s within %s", what, within)
		case <-ctx.Done():
			return fmt.Sprintf("test deadline before %s: %v", what, ctx.Err())
		}
	}
}

// next returns a raw frame the hub published after since (any frame when since
// is zero), waiting up to within for one, and why there is none if so.
func (f *hubFrames) next(ctx context.Context, within time.Duration, since time.Time) (*videoFrame, string) {
	var found *videoFrame
	problem := f.waitFor(ctx, within, "decoded frame", func() bool {
		for _, frame := range f.frames {
			if since.IsZero() || int64(frame.tsNs) > since.UnixNano() {
				found = frame
				return true
			}
		}
		return false
	})
	return found, problem
}

// h264Keyframes waits up to within for an encoded stream with n independently
// decodable keyframes, and returns that stream or why there is none.
func (f *hubFrames) h264Keyframes(ctx context.Context, within time.Duration, n int) ([]byte, string) {
	var stream []byte
	problem := f.waitFor(ctx, within, fmt.Sprintf("H.264 with %d keyframes", n), func() bool {
		if len(annexBKeyframeStarts(f.stream)) >= n {
			stream = append([]byte(nil), f.stream...)
			return true
		}
		return false
	})
	if problem != "" {
		f.mu.Lock()
		problem = fmt.Sprintf("%s (%d chunks, %d bytes, codecs %v)", problem, f.chunks, len(f.stream), f.codecs)
		f.mu.Unlock()
	}
	return stream, problem
}

// firstAt is when the first encoded chunk arrived.
func (f *hubFrames) firstAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.first
}

// counts reports the encoded chunks received so far, and the length of the
// stream they make.
func (f *hubFrames) counts() (chunks, size int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunks, len(f.stream)
}

// streamSince returns the encoded bytes received after the first offset.
func (f *hubFrames) streamSince(offset int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.stream[offset:]...)
}

// pixelFrameProblem says why frame is not a YUYV frame of the redGreenJPEG
// picture, or "" when it is: the left of its middle row must differ in luma
// from the right by more than 40 (red is about 80, green about 140).
func pixelFrameProblem(frame *videoFrame) string {
	format := frame.rawFmt
	if frame.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || format == nil {
		return fmt.Sprintf("codec %v with format %v", frame.codec, format)
	}
	if format.GetFourcc() != fourccYUYV || format.GetWidth() != pixelsFixtureWidth || format.GetHeight() != pixelsFixtureHeight ||
		format.GetBytesPerLine() != pixelsFixtureWidth*2 || len(frame.data) != pixelsFixtureWidth*pixelsFixtureHeight*2 {
		return fmt.Sprintf("format %s with %d bytes, want %dx%d YUYV in %d", describeRawFormat(format), len(frame.data),
			pixelsFixtureWidth, pixelsFixtureHeight, pixelsFixtureWidth*pixelsFixtureHeight*2)
	}
	left, right := fixtureLuma(frame)
	if diff := left - right; diff <= 40 && diff >= -40 {
		return fmt.Sprintf("luma at (10,120) is %d and at (310,120) is %d", left, right)
	}
	return ""
}

// fixtureLuma reads the luma of a YUYV frame at (10,120) and (310,120), in the
// red and the green half of the picture. In YUYV the luma of pixel x is byte 2x
// of its row.
func fixtureLuma(frame *videoFrame) (left, right int) {
	row := 120 * int(frame.rawFmt.GetBytesPerLine())
	return int(frame.data[row+2*10]), int(frame.data[row+2*310])
}
