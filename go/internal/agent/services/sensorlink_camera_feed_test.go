package services

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// recordingCameraWriter stands in for the v4l2loopback node writer.
type recordingCameraWriter struct {
	mu     sync.Mutex
	frames []ros2camera.Frame
	err    error
	closed bool
}

func (w *recordingCameraWriter) WriteFrame(f ros2camera.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = append(w.frames, f)
	return w.err
}

func (w *recordingCameraWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func receiveSensorFrame(t *testing.T, frames <-chan ros2camera.Frame) ros2camera.Frame {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame reached the subscriber")
		return ros2camera.Frame{}
	}
}

func TestSensorFrameFeedTeesEveryFrameToTheNode(t *testing.T) {
	feed := NewSensorFrameFeed()
	node := &recordingCameraWriter{err: errors.New("no v4l2loopback")}
	writer := feed.WrapWriters(func(path string) ros2camera.CameraWriter {
		if path != "/dev/video96" {
			t.Errorf("writer opened for %q", path)
		}
		return node
	})("/dev/video96")
	frames, unsubscribe := feed.subscribe("/dev/video96")
	defer unsubscribe()

	err := writer.WriteFrame(ros2camera.Frame{Data: []byte("jpeg-1"), Codec: ros2camera.CodecMJPEG})
	if err == nil || err.Error() != "no v4l2loopback" {
		t.Errorf("WriteFrame error = %v, want the node writer's own error", err)
	}
	// The hub keeps its frames even when the node cannot be written.
	if got := receiveSensorFrame(t, frames); string(got.Data) != "jpeg-1" {
		t.Errorf("subscriber got %q", got.Data)
	}
	if len(node.frames) != 1 {
		t.Errorf("node writer saw %d frames, want 1", len(node.frames))
	}

	// A link that reconnects gets a new writer for the same node; the
	// subscriber's stream carries on across it.
	if err := writer.Close(); err != nil || !node.closed {
		t.Fatalf("Close = %v, node closed = %v", err, node.closed)
	}
	again := feed.WrapWriters(func(string) ros2camera.CameraWriter { return &recordingCameraWriter{} })("/dev/video96")
	_ = again.WriteFrame(ros2camera.Frame{Data: []byte("jpeg-2"), Codec: ros2camera.CodecMJPEG})
	if got := receiveSensorFrame(t, frames); string(got.Data) != "jpeg-2" {
		t.Errorf("after reconnect subscriber got %q, want jpeg-2", got.Data)
	}

	// Frames for another node never cross over.
	other := feed.WrapWriters(func(string) ros2camera.CameraWriter { return &recordingCameraWriter{} })("/dev/video97")
	_ = other.WriteFrame(ros2camera.Frame{Data: []byte("other"), Codec: ros2camera.CodecMJPEG})
	select {
	case f := <-frames:
		t.Errorf("subscriber of video96 got video97's frame %q", f.Data)
	default:
	}
}

// TestSensorFrameFeedNeverBlocksTheLink pins the property that keeps a slow
// encoder from stalling the SensorLink reader, which also feeds the node apps
// read: publish drops the subscriber's oldest frame instead of waiting, and
// hands out a copy the transport cannot overwrite.
func TestSensorFrameFeedNeverBlocksTheLink(t *testing.T) {
	feed := NewSensorFrameFeed()
	frames, unsubscribe := feed.subscribe("/dev/video96")
	defer unsubscribe()

	buf := []byte("frame-0")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < sensorFrameBuffer+3; i++ {
			buf[len(buf)-1] = byte('0' + i)
			feed.publish("/dev/video96", ros2camera.Frame{Data: buf})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a subscriber that is not reading")
	}
	buf[len(buf)-1] = 'X' // the transport reuses its buffer

	var got []string
	for len(got) < sensorFrameBuffer {
		got = append(got, string(receiveSensorFrame(t, frames).Data))
	}
	want := []string{"frame-3", "frame-4", "frame-5", "frame-6"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("subscriber kept %v, want the newest %v, each its own copy", got, want)
	}
}

func TestSensorFrameFeedUnsubscribe(t *testing.T) {
	feed := NewSensorFrameFeed()
	_, unsubscribe := feed.subscribe("/dev/video96")
	unsubscribe()
	feed.publish("/dev/video96", ros2camera.Frame{Data: []byte("x")})
	if len(feed.subs) != 0 {
		t.Errorf("subscriptions left after unsubscribe: %v", feed.subs)
	}
}

func TestIsSensorLinkCameraPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/dev/video96":  true,
		"/dev/video127": true,
		"/dev/video95":  false, // below the MCU band
		"/dev/video128": false, // ROS 2 band
		"/dev/video4":   false,
		"/dev/video":    false,
		"ip:200":        false,
	} {
		if got := isSensorLinkCameraPath(path); got != want {
			t.Errorf("isSensorLinkCameraPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestPumpSensorFramesEndsOnFormatChangeOrSilence(t *testing.T) {
	stall := sensorFrameStall
	sensorFrameStall = 50 * time.Millisecond
	t.Cleanup(func() { sensorFrameStall = stall })

	first := ros2camera.Frame{Data: []byte("a"), Width: 800, Height: 600, Codec: ros2camera.CodecMJPEG}
	frames := make(chan ros2camera.Frame, 2)
	frames <- ros2camera.Frame{Data: []byte("b"), Width: 800, Height: 600, Codec: ros2camera.CodecMJPEG}
	frames <- ros2camera.Frame{Data: []byte("c"), Width: 640, Height: 480, Codec: ros2camera.CodecMJPEG}
	var out bytes.Buffer
	err := pumpSensorFrames(context.Background(), &out, frames, first)
	if err == nil || !strings.Contains(err.Error(), "800x600 to 640x480") {
		t.Errorf("format change: err = %v", err)
	}
	if out.String() != "ab" {
		t.Errorf("wrote %q, want the two frames before the change", out.String())
	}

	out.Reset()
	err = pumpSensorFrames(context.Background(), &out, make(chan ros2camera.Frame), first)
	if err == nil || !strings.Contains(err.Error(), "no frame for") {
		t.Errorf("silence: err = %v", err)
	}
}

func TestStreamSensorLinkCameraRefusals(t *testing.T) {
	stall := sensorFrameStall
	sensorFrameStall = 50 * time.Millisecond
	t.Cleanup(func() { sensorFrameStall = stall })
	discard := func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return true }

	feed := NewSensorFrameFeed()
	svc := &VideoService{logger: zap.NewNop(), sensorFrames: feed}

	// A paired camera that sends nothing is reported, not waited on forever.
	err := svc.streamSensorLinkCamera(context.Background(), discard, "/dev/video96", &agentpb.StreamVideoRequest{}, noRawSink{})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "is not streaming") {
		t.Errorf("silent camera: err = %v", err)
	}

	publishWhileStreaming := func(frame ros2camera.Frame, req *agentpb.StreamVideoRequest) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			for ctx.Err() == nil {
				feed.publish("/dev/video96", frame)
				time.Sleep(5 * time.Millisecond)
			}
		}()
		return svc.streamSensorLinkCamera(ctx, discard, "/dev/video96", req, noRawSink{})
	}
	// H.264 goes back to the node path, which handles it today.
	if err := publishWhileStreaming(ros2camera.Frame{Data: []byte{0, 0, 0, 1}, Codec: ros2camera.CodecH264}, &agentpb.StreamVideoRequest{}); !errors.Is(err, errSensorFeedNotJPEG) {
		t.Errorf("H.264 camera: err = %v, want errSensorFeedNotJPEG", err)
	}
	jpegFrame := ros2camera.Frame{Data: sharedTestJPEG(t), Width: 64, Height: 48, Codec: ros2camera.CodecMJPEG}
	if err := publishWhileStreaming(jpegFrame, &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW}); status.Code(err) == codes.OK {
		t.Error("raw request was accepted for a JPEG-only camera")
	}
}

// TestSensorLinkCameraHubEncodesFromTheFeed is the end-to-end regression: a
// hub on an MCU-band path captures from the feed alone. No /dev/video96 exists
// on the test host, so a hub that opened the node could not deliver a frame.
func TestSensorLinkCameraHubEncodesFromTheFeed(t *testing.T) {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("GStreamer is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := NewVideoService(ctx, zap.NewNop(), nil)
	defer svc.Shutdown()
	feed := NewSensorFrameFeed()
	svc.SetSensorFrameFeed(feed)

	writer := feed.WrapWriters(func(string) ros2camera.CameraWriter { return &recordingCameraWriter{} })("/dev/video96")
	frame := ros2camera.Frame{Data: sharedTestJPEG(t), Width: 64, Height: 48, Codec: ros2camera.CodecMJPEG}
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = writer.WriteFrame(frame)
			}
		}
	}()

	hub, id, frames, err := svc.joinHub(ctx, "/dev/video96", &agentpb.StreamVideoRequest{DeviceId: 96})
	if err != nil {
		t.Fatalf("joinHub: %v", err)
	}
	defer hub.unsubscribe(id)
	var encoded int
	for encoded < 3 {
		select {
		case f, ok := <-frames:
			if !ok {
				hub.mu.Lock()
				herr := hub.err
				hub.mu.Unlock()
				t.Fatalf("hub ended after %d frames: %v", encoded, herr)
			}
			if f.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || len(f.data) == 0 {
				t.Fatalf("hub frame codec %v, %d bytes", f.codec, len(f.data))
			}
			encoded++
		case <-ctx.Done():
			t.Fatalf("only %d encoded frames before the deadline", encoded)
		}
	}
}
