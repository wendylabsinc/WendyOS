package services

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The media job traces this test and its real GStreamer child processes to
// reject any open of /dev/video96. The node is deliberately absent. Only the
// hardware node writer is substituted; feed, producer, hub and codecs are real.
func TestSensorLinkSubscribersDecodeWithoutOpeningLoopback(t *testing.T) {
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("GStreamer is not installed")
	}
	const cameraPath = "/dev/video96"
	if _, err := os.Lstat(cameraPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture requires an absent %s, got %v", cameraPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service := NewVideoService(ctx, zap.NewNop(), nil)
	defer service.Shutdown()
	feed := NewSensorFrameFeed()
	service.SetSensorFrameFeed(feed)
	node := &recordingCameraWriter{err: os.ErrNotExist}
	writer := feed.WrapWriters(func(string) ros2camera.CameraWriter { return node })(cameraPath)
	defer writer.Close()
	first, firstID, firstFrames, err := service.getOrCreateHub(ctx, cameraPath, &agentpb.StreamVideoRequest{DeviceId: 96}, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer first.unsubscribe(firstID)
	second, secondID, secondFrames, err := service.getOrCreateHub(ctx, cameraPath, &agentpb.StreamVideoRequest{DeviceId: 96}, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer second.unsubscribe(secondID)
	if first != second || firstID == secondID {
		t.Fatal("two SensorLink viewers did not share one producer hub")
	}
	// An incompatible viewer cannot start another producer or disrupt the two
	// established consumers while the source is publishing real encoded video.
	_, _, _, err = service.getOrCreateHub(ctx, cameraPath, &agentpb.StreamVideoRequest{DeviceId: 96, Width: 1280, Height: 720}, hubHolderStreamClient)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("incompatible viewer error = %v", err)
	}
	frames := []ros2camera.Frame{sensorLinkColorFrame(t, color.RGBA{R: 240, A: 255}), sensorLinkColorFrame(t, color.RGBA{G: 240, A: 255})}
	var phase atomic.Int32
	publisherCtx, stopPublisher := context.WithCancel(ctx)
	publisherDone := make(chan struct{})
	defer func() { stopPublisher(); <-publisherDone }()
	go func() {
		defer close(publisherDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-publisherCtx.Done():
				return
			case <-ticker.C:
				_ = writer.WriteFrame(frames[phase.Load()])
			}
		}
	}()
	var initial [2][]byte
	for len(annexBKeyframeStarts(initial[0])) < 4 || len(annexBKeyframeStarts(initial[1])) < 4 {
		select {
		case frame, ok := <-firstFrames:
			initial[0] = appendSensorLinkEncoded(t, first, initial[0], frame, ok)
		case frame, ok := <-secondFrames:
			initial[1] = appendSensorLinkEncoded(t, second, initial[1], frame, ok)
		case <-ctx.Done():
			t.Fatal("SensorLink subscribers did not receive decodable frames before deadline")
		}
	}
	for _, stream := range initial {
		assertSensorLinkDecodedColor(t, ctx, launch, stream, 0)
	}
	first.unsubscribe(firstID)
	if second.ctx.Err() != nil {
		t.Fatal("unsubscribing one viewer stopped the remaining viewer")
	}
	phase.Store(1)
	var after []byte
	for len(annexBKeyframeStarts(after)) < 5 {
		select {
		case frame, ok := <-secondFrames:
			after = appendSensorLinkEncoded(t, second, after, frame, ok)
		case <-ctx.Done():
			t.Fatal("remaining SensorLink subscriber stopped receiving fresh frames")
		}
	}
	assertSensorLinkDecodedColor(t, ctx, launch, after, 1)
	second.unsubscribe(secondID)
	select {
	case <-second.done:
	case <-time.After(3 * time.Second):
		t.Fatal("last viewer did not stop the encoder and unsubscribe from the feed")
	}
	feed.mu.Lock()
	subscriberCount := len(feed.subs[cameraPath])
	feed.mu.Unlock()
	if subscriberCount != 0 {
		t.Fatalf("capture retained %d feed subscribers after teardown", subscriberCount)
	}
}

func sensorLinkColorFrame(t *testing.T, left color.RGBA) ros2camera.Frame {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			value := left
			if x >= 32 {
				value = color.RGBA{B: 240, A: 255}
			}
			frame.SetRGBA(x, y, value)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return ros2camera.Frame{Data: encoded.Bytes(), Width: 64, Height: 48, Codec: ros2camera.CodecMJPEG}
}

func appendSensorLinkEncoded(t *testing.T, hub *deviceHub, stream []byte, frame *videoFrame, ok bool) []byte {
	t.Helper()
	if !ok {
		t.Fatalf("SensorLink subscribers did not receive decodable frames: %v", hub.terminalErr())
	}
	if frame.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || len(frame.data) == 0 {
		t.Fatalf("SensorLink subscriber received codec=%v bytes=%d", frame.codec, len(frame.data))
	}
	return append(stream, frame.data...)
}

func assertSensorLinkDecodedColor(t *testing.T, ctx context.Context, launch string, stream []byte, leftChannel int) {
	t.Helper()
	keyframes := annexBKeyframeStarts(stream)
	// Decode a complete interval at the end of the observation, excluding any
	// initial partial access unit and the final potentially partial pipe read.
	stream = stream[keyframes[len(keyframes)-2]:keyframes[len(keyframes)-1]]
	decode := exec.CommandContext(ctx, launch, "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1")
	decode.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	decode.Stderr = &stderr
	pixels, err := decode.Output()
	const frameBytes = 64 * 48 * 3
	if err != nil || len(pixels) < frameBytes || len(pixels)%frameBytes != 0 {
		t.Fatalf("SensorLink viewer decode failed: bytes=%d err=%v stderr=%s", len(pixels), err, stderr.String())
	}
	for offset := 0; offset < len(pixels); offset += frameBytes {
		for _, point := range []struct{ x, channel int }{{16, leftChannel}, {48, 2}} {
			index := offset + (24*64+point.x)*3
			for channel, value := range pixels[index : index+3] {
				if (channel == point.channel && value < 200) || (channel != point.channel && value > 45) {
					t.Fatalf("SensorLink decoded stale/incorrect image at x=%d: %v", point.x, pixels[index:index+3])
				}
			}
		}
	}
	t.Logf("decoded %d fresh 64x48 RGB frames, left channel %d", len(pixels)/frameBytes, leftChannel)
}
