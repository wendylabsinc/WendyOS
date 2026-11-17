package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

const (
	sendOnceWidth      = 64
	sendOnceHeight     = 48
	sendOncePictures   = 90
	sendOnceFrameBytes = sendOnceWidth * sendOnceHeight * 3 // one decoded RGB picture
)

// Decode what a late viewer and a late recording get from a camera that sends
// SPS and PPS once.
//
// The stream is a real encoder's, made here, one access unit per buffer as
// V4L2 delivers it, with the parameter sets in the first buffer only. Each
// buffer goes through the native path's emit callback into a hub the service
// created. A viewer and a recording join in the middle of a group of pictures,
// and a real decoder decodes what the viewer was sent and what the recording
// wrote to its segment file.
//
// The second arm is the control: the same buffers forwarded as they came, which
// is what the native path did before it repeated parameter sets. There the late
// viewer's decoder produces no picture and the recording never starts. Nothing
// else differs between the arms.
//
// Real here: the encoder and its bitstream, nativeH264Emit, the hub and its
// lifecycle, the viewer's send loop, the capture adapter and its segment file,
// the decoder. Not real: the V4L2 dequeue (no device is opened) and the gRPC
// transport.
func TestNativeH264SendOnceStreamDecodesForLateViewerAndRecording(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	launch := gstreamerForSendOnceTest(ctx, t)

	buffers := encodeSendOnceH264(ctx, t, launch)
	keyframes := requireSendOnceH264(t, buffers)
	isKeyframe := make(map[int]bool, len(keyframes))
	for _, index := range keyframes {
		isKeyframe[index] = true
	}
	// Join halfway through a group of pictures, long after the camera's only
	// SPS and PPS went by. The first picture a joiner can decode is the next
	// keyframe.
	joinAt := (keyframes[1] + keyframes[2]) / 2
	startAt := keyframes[2]

	// What the pictures are, as decoded by someone who has the whole stream.
	reference, diagnostics, err := decodeH264(ctx, launch, bytes.Join(buffers, nil))
	if err != nil || len(reference) != len(buffers) {
		t.Fatalf("the whole stream decoded to %d pictures, want %d: %v\n%s", len(reference), len(buffers), err, diagnostics)
	}
	for i := 1; i < len(reference); i++ {
		if bytes.Equal(reference[i-1], reference[i]) {
			t.Fatalf("pictures %d and %d are identical, so comparing pictures could not tell them apart", i-1, i)
		}
	}
	want := reference[startAt:]
	t.Logf("stream: %d buffers, keyframes at %v, parameter sets in buffer 0 only; late consumers join before buffer %d and can start at buffer %d, %d pictures from the end",
		len(buffers), keyframes, joinAt, startAt, len(want))

	t.Run("with reinsertion", func(t *testing.T) {
		play := playSendOnceH264(ctx, t, buffers, joinAt, startAt, true)

		requireH264Decodes(ctx, t, launch, "first viewer", bytes.Join(play.first, nil), reference)

		if len(play.late) != len(buffers)-joinAt {
			t.Fatalf("late viewer was sent %d buffers, want the %d that followed its join", len(play.late), len(buffers)-joinAt)
		}
		for i, message := range play.late {
			if changed := !bytes.Equal(message, buffers[joinAt+i]); changed != isKeyframe[joinAt+i] {
				t.Errorf("buffer %d: changed on the way to the viewer = %v, keyframe = %v; only keyframes gain parameter sets", joinAt+i, changed, isKeyframe[joinAt+i])
			}
		}
		requireH264Decodes(ctx, t, launch, "late viewer", bytes.Join(play.late, nil), want)

		if play.recordingErr != nil {
			t.Fatalf("late recording did not start: %v", play.recordingErr)
		}
		if play.recorded != uint64(len(want)) {
			t.Errorf("late recording counts %d frames, want the %d from its first keyframe on", play.recorded, len(want))
		}
		requireH264Decodes(ctx, t, launch, "late recording", play.segments, want)
	})

	t.Run("without reinsertion", func(t *testing.T) {
		play := playSendOnceH264(ctx, t, buffers, joinAt, startAt, false)

		// The stream is sound: whoever has it from the start decodes all of it.
		requireH264Decodes(ctx, t, launch, "first viewer", bytes.Join(play.first, nil), reference)

		if len(play.late) != len(buffers)-joinAt {
			t.Fatalf("late viewer was sent %d buffers, want the %d that followed its join", len(play.late), len(buffers)-joinAt)
		}
		for i, message := range play.late {
			if !bytes.Equal(message, buffers[joinAt+i]) {
				t.Fatalf("buffer %d was changed on the way to the viewer; the control must forward the camera's bytes", joinAt+i)
			}
		}
		frames, diagnostics, err := decodeH264(ctx, launch, bytes.Join(play.late, nil))
		if len(frames) != 0 {
			t.Errorf("late viewer decoded %d frames from buffers that carry no parameter sets; the control no longer shows the defect", len(frames))
		}
		t.Logf("late viewer was sent %d buffers and its decoder produced %d pictures: %v\n%s", len(play.late), len(frames), err, diagnostics)

		if play.recordingErr == nil {
			t.Errorf("late recording started and counts %d frames on buffers that carry no parameter sets; the control no longer shows the defect", play.recorded)
		}
		if len(play.segments) != 0 {
			t.Errorf("late recording wrote %d bytes to its segment files, want none", len(play.segments))
		}
		t.Logf("late recording was offered %d buffers and never started: %v", len(buffers)-joinAt, play.recordingErr)
	})
}

// gstreamerForSendOnceTest returns the gst-launch-1.0 to use. Without GStreamer
// or one of the elements the test needs it skips, unless
// WENDY_GSTREAMER_REQUIRED=1, as the Linux media CI job sets: there a missing
// tool fails the test.
func gstreamerForSendOnceTest(ctx context.Context, t *testing.T) string {
	t.Helper()
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
	for _, element := range []string{"videotestsrc", "x264enc", "h264parse", "multifilesink", "fdsrc", "fdsink", "avdec_h264", "videoconvert"} {
		if out, err := exec.CommandContext(ctx, inspect, element).CombinedOutput(); err != nil {
			unavailable("GStreamer element %s is unavailable: %v: %s", element, err, bytes.TrimSpace(out))
		}
	}
	return launch
}

// encodeSendOnceH264 encodes a moving test picture and returns the encoder's
// buffers, one access unit each.
//
// The parameter sets end up in the first buffer only without this test touching
// the bitstream. repeat-headers=0 stops x264 writing SPS and PPS into the stream
// at each keyframe, so in AVC form they travel beside it as codec data.
// h264parse, converting to a byte stream, writes that codec data ahead of the
// first picture and, with config-interval=0, never again. multifilesink writes
// one file per buffer, which keeps the encoder's own access unit boundaries.
func encodeSendOnceH264(ctx context.Context, t *testing.T, launch string) [][]byte {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, launch, "-q",
		"videotestsrc", fmt.Sprintf("num-buffers=%d", sendOncePictures), "pattern=ball", "animation-mode=frames", "!",
		fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,framerate=30/1", sendOnceWidth, sendOnceHeight), "!",
		"x264enc", "key-int-max=15", "bframes=0", "speed-preset=ultrafast", "aud=true", "option-string=repeat-headers=0", "!",
		"video/x-h264,stream-format=avc,alignment=au", "!",
		"h264parse", "config-interval=0", "!",
		"video/x-h264,stream-format=byte-stream,alignment=au", "!",
		"multifilesink", "location="+filepath.Join(dir, "au-%05d.h264"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encoding the test stream failed: %v\n%s", err, out)
	}
	names, err := filepath.Glob(filepath.Join(dir, "au-*.h264"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	buffers := make([][]byte, 0, len(names))
	for _, name := range names {
		buffer, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		buffers = append(buffers, buffer)
	}
	return buffers
}

// requireSendOnceH264 fails unless buffers are what a send-once camera
// delivers: SPS, PPS and an IDR picture in the first buffer, one picture and no
// parameter set in every later one. It returns the indexes of the keyframes.
func requireSendOnceH264(t *testing.T, buffers [][]byte) (keyframes []int) {
	t.Helper()
	if len(buffers) != sendOncePictures {
		t.Fatalf("the encoder produced %d buffers, want %d", len(buffers), sendOncePictures)
	}
	for i, buffer := range buffers {
		types := annexBUnitTypes(buffer)
		var sps, pps, idr, slices int
		for _, unit := range types {
			switch unit {
			case 7:
				sps++
			case 8:
				pps++
			case 5:
				idr++
				slices++
			case 1:
				slices++
			}
		}
		switch {
		case slices == 0:
			t.Fatalf("buffer %d holds no picture (NAL unit types %v)", i, types)
		case i == 0 && (sps != 1 || pps != 1 || idr == 0):
			t.Fatalf("the first buffer must hold one SPS, one PPS and an IDR picture; its NAL unit types are %v", types)
		case i > 0 && (sps != 0 || pps != 0):
			t.Fatalf("buffer %d repeats a parameter set (NAL unit types %v): the encoder pipeline did not produce a send-once stream", i, types)
		}
		if idr > 0 {
			keyframes = append(keyframes, i)
		}
	}
	if len(keyframes) < 4 || keyframes[2]-keyframes[1] < 2 {
		t.Fatalf("keyframes at %v leave no room to join inside a group of pictures", keyframes)
	}
	return keyframes
}

// annexBUnitTypes lists the nal_unit_type of each NAL unit in a buffer. It does
// not use the agent's own scanner, so the check on the test stream does not
// share a mistake with the code under test.
func annexBUnitTypes(buffer []byte) []byte {
	var types []byte
	for offset := 0; ; {
		at := bytes.Index(buffer[offset:], []byte{0, 0, 1})
		if at < 0 || offset+at+3 >= len(buffer) {
			return types
		}
		types = append(types, buffer[offset+at+3]&0x1f)
		offset += at + 3
	}
}

// decodeH264 decodes an Annex-B stream with GStreamer and returns its pictures
// as RGB frames, with whatever the decoder said on stderr.
func decodeH264(ctx context.Context, launch string, stream []byte) (frames [][]byte, diagnostics string, err error) {
	cmd := exec.CommandContext(ctx, launch, "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1")
	cmd.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pixels, err := cmd.Output()
	if len(pixels)%sendOnceFrameBytes != 0 {
		err = errors.Join(err, fmt.Errorf("decoder wrote %d bytes, not a whole number of %dx%d RGB frames", len(pixels), sendOnceWidth, sendOnceHeight))
	}
	for offset := 0; offset+sendOnceFrameBytes <= len(pixels); offset += sendOnceFrameBytes {
		frames = append(frames, pixels[offset:offset+sendOnceFrameBytes])
	}
	return frames, stderr.String(), err
}

// requireH264Decodes fails unless stream decodes to exactly the pictures in
// want, compared pixel for pixel.
func requireH264Decodes(ctx context.Context, t *testing.T, launch, who string, stream []byte, want [][]byte) {
	t.Helper()
	frames, diagnostics, err := decodeH264(ctx, launch, stream)
	if err != nil || len(frames) != len(want) {
		t.Errorf("%s decoded %d frames, want %d: %v\n%s", who, len(frames), len(want), err, diagnostics)
		return
	}
	for i := range want {
		if !bytes.Equal(frames[i], want[i]) {
			t.Errorf("%s: decoded frame %d of %d is not the picture the camera encoded", who, i, len(want))
			return
		}
	}
	t.Logf("%s decoded %d frames, each identical to the reference decode", who, len(frames))
}

// sendOncePlayback is what the consumers of one run got.
type sendOncePlayback struct {
	// first and late are the messages sent to a viewer there from the start and
	// to one that joined mid-stream.
	first, late [][]byte
	// recordingErr is why the late recording did not start; nil when it did.
	recordingErr error
	// recorded is the frame count in the recording's result, and segments its
	// segment files, in order.
	recorded uint64
	segments []byte
}

// playSendOnceH264 delivers buffers to a hub the service creates, with a viewer
// attached from the start and a viewer and a recording that join before buffer
// joinAt. keyframe is the first buffer the late consumers can start on. With
// reinsert the buffers go through nativeH264Emit; without, they are forwarded
// as they came.
func playSendOnceH264(ctx context.Context, t *testing.T, buffers [][]byte, joinAt, keyframe int, reinsert bool) sendOncePlayback {
	t.Helper()
	// A camera number nothing else uses. Its node is never opened.
	const devID = 45
	svc := NewVideoService(context.Background(), zap.NewNop(), nil)
	t.Cleanup(svc.Shutdown)
	// The producer seam: the service creates, shares and tears down the hub
	// itself, but no capture loop opens a device. The buffers are handed to the
	// hub from this goroutine instead of from a V4L2 dequeue.
	installFakeProducers(svc)
	src, err := svc.resolveSource(devID)
	if err != nil {
		t.Fatal(err)
	}

	first, hub := startSendOnceViewer(t, svc, src.key, devID)
	// What streamV4L2Native handed captureV4L2Native before parameter sets were
	// repeated: every buffer forwarded as it came.
	emit := func(buffer []byte, stamp frameTimestamp) bool {
		return hubBroadcast(hub)(buffer, stamp, agentpb.VideoCodec_VIDEO_CODEC_H264)
	}
	if reinsert {
		emit = svc.nativeH264Emit(src.path, hubBroadcast(hub))
	}
	camera := testCaptureLoop(emit)
	deliver := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			if !camera(buffers[i], true) {
				t.Fatalf("the hub told the camera to stop at buffer %d with consumers attached", i)
			}
			// The hub drops frames for a consumer that falls four behind. Pacing
			// the camera to its consumers keeps every count below exact.
			taken := hubReaches(hub, func() bool {
				for _, sub := range hub.subs {
					if len(sub.ch) > 0 {
						return false
					}
				}
				return true
			})
			if !taken {
				t.Fatalf("a consumer stopped taking frames at buffer %d", i)
			}
		}
	}

	deliver(0, joinAt)

	late, lateHub := startSendOnceViewer(t, svc, src.key, devID)
	if lateHub != hub {
		t.Fatal("the late viewer did not join the running hub")
	}
	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin}
	source := data.Source{ID: fmt.Sprintf("v4l2:/dev/video%d", devID), Kind: "camera"}
	type startResult struct {
		capture runningDataCapture
		err     error
	}
	started := make(chan startResult, 1)
	startCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	go func() {
		capture, err := (&cameraDataAdapter{video: svc}).Start(startCtx, session, []data.Source{source})
		started <- startResult{capture, err}
	}()
	if !hubReaches(hub, func() bool { return len(hub.subs) == 3 }) {
		select {
		case early := <-started:
			t.Fatalf("the recording ended before it joined the hub: %v", early.err)
		default:
			t.Fatal("the recording did not join the hub")
		}
	}

	// Start returns once the recording has written its first frame. With
	// reinsertion that is the keyframe; give it time to say so before going on.
	deliver(joinAt, keyframe+1)
	var result startResult
	returned := false
	if reinsert {
		select {
		case result = <-started:
			returned = true
		case <-time.After(10 * time.Second):
		}
	}
	deliver(keyframe+1, len(buffers))
	if !returned {
		// The recording is still waiting for a frame it can start on. End the
		// wait here rather than sit out the adapter's own 20 seconds.
		stopWaiting()
		result = <-started
	}

	play := sendOncePlayback{recordingErr: result.err}
	if result.err == nil {
		if result.capture == nil {
			t.Fatal("the capture adapter started nothing and reported no error")
		}
		results, err := result.capture.Stop(context.Background())
		if err != nil || len(results) != 1 {
			t.Fatalf("stopping the recording: %v (results %+v)", err, results)
		}
		play.recorded = results[0].Count
	}
	names, err := filepath.Glob(filepath.Join(session.Directory, "cameras", safeCaptureName(source.ID), "segment-*.h264"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, name := range names {
		segment, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		play.segments = append(play.segments, segment...)
	}

	play.late = late.stop()
	play.first = first.stop()
	// The last consumer has left, so the service ends the hub.
	select {
	case <-hub.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub was not torn down after its last consumer left")
	}
	return play
}

// hubReaches polls a condition on the hub, under its lock, and reports whether
// it came true within ten seconds.
func hubReaches(hub *deviceHub, reached func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for {
		hub.mu.Lock()
		ok := reached()
		hub.mu.Unlock()
		if ok || time.Now().After(deadline) {
			return ok
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// sendOnceViewer is one `camera view` client: the part of StreamVideo that
// follows device validation, which needs a real /dev/video node. It joins the
// device's hub as a stream client and runs the RPC's send loop.
type sendOnceViewer struct {
	stream *fakeVideoStream
	cancel context.CancelFunc
	done   chan struct{}
}

func startSendOnceViewer(t *testing.T, svc *VideoService, key string, devID uint32) (*sendOnceViewer, *deviceHub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub, id, frames, err := svc.getOrCreateHub(ctx, key, &agentpb.StreamVideoRequest{DeviceId: devID}, hubHolderStreamClient)
	if err != nil {
		cancel()
		t.Fatalf("viewer could not join the camera: %v", err)
	}
	viewer := &sendOnceViewer{stream: newFakeVideoStream(ctx), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(viewer.done)
		defer hub.unsubscribe(id)
		_ = svc.pumpFrames(viewer.stream, hub, id, frames)
	}()
	return viewer, hub
}

// stop ends the viewer's stream and returns the messages it was sent.
func (v *sendOnceViewer) stop() [][]byte {
	v.cancel()
	<-v.done
	return v.stream.frames
}
