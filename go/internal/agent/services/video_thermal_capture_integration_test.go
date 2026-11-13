package services

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

// The physical USB source and its advertised modes are fixtures. The production
// plan selects the capture format; a real GStreamer process tees its bytes to
// fd 3, and runCameraPipeline, pumpRawTap and the hub deliver them to subscribers.
func TestThermalRawSubscribersReceiveUnmodified16BitMeasurements(t *testing.T) {
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("GStreamer is not installed")
	}
	const width, height = 160, 120
	const cameraPath = "/dev/video1987"
	if _, err := os.Lstat(cameraPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture requires an absent %s, got %v", cameraPath, err)
	}
	withRawModes(t, map[uint32][][2]uint32{
		v4l2PixFmtUYVY: {{width, height}},
		v4l2PixFmtY16:  {{width, height}},
		v4l2PixFmtGrey: {{width, height}},
	})
	previousSize := bestDefaultFrameSizeForDevice
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return width, height }
	t.Cleanup(func() { bestDefaultFrameSizeForDevice = previousSize })
	plan, err := planGStreamerPipeline(launch, cameraPath, &agentpb.StreamVideoRequest{Framerate: 30},
		"x264enc", true, camera.TransportUSB, "", pipeWireSource{}, map[string]bool{"x264enc": true, "h264parse": true})
	if err != nil {
		t.Fatal(err)
	}
	// Replace only the physical source, leaving the selected capture caps, tee,
	// queues and both sinks untouched. videoconvert lets the source offer the
	// same alternatives as the camera: an old UYVY preference produces preview
	// bytes and must fail the subscriber assertions, rather than negotiation.
	if len(plan.args) < 5 || plan.args[2] != "v4l2src" || plan.args[3] != "device="+cameraPath || plan.args[4] != "!" {
		t.Fatalf("unexpected physical source: %v", plan.args)
	}
	fixture := strings.Fields("fdsrc fd=0 ! rawvideoparse format=gray16-le width=160 height=120 framerate=30/1 ! videoconvert")
	plan.args = append(append(append([]string{}, plan.args[:2]...), fixture...), plan.args[4:]...)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hub, stopHub := newTestHub(t)
	defer stopHub()
	hub.thermalProfile = &agentpb.ThermalCamera{Adapter: thermal.PureThermal}
	firstID, firstFrames, err := hub.subscribeKind(true)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.unsubscribe(firstID)
	input, writer := io.Pipe()
	producerDone := make(chan error, 1)
	service := &VideoService{logger: zap.NewNop()}
	go func() {
		producerDone <- service.runCameraPipeline(ctx, func(data []byte, ts frameTimestamp, codec agentpb.VideoCodec) bool {
			return hub.broadcast(&videoFrame{data: data, tsNs: ts.wallNs, codec: codec})
		}, cameraPath, gstEncoderResult{element: "x264enc", codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: true}, plan, nil, hub, input)
	}()
	defer func() {
		cancel()
		_ = writer.Close()
		select {
		case err := <-producerDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("thermal producer shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("thermal producer did not stop")
		}
	}()
	patterns := make([][]byte, 2)
	for phase := range patterns {
		patterns[phase] = make([]byte, width*height*2)
		for pixel := 0; pixel < width*height; pixel++ {
			// Every row and both bytes vary, so a crop, endian swap, reduction to
			// eight bits, or a wrong stride changes observable measurements.
			binary.LittleEndian.PutUint16(patterns[phase][2*pixel:], uint16(0x6001+phase+pixel*257))
		}
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(time.Second / 30)
		defer ticker.Stop()
		for frame := 0; ; frame++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := writer.Write(patterns[frame%len(patterns)]); err != nil {
					return
				}
			}
		}
	}()
	defer func() { cancel(); _ = writer.Close(); <-writerDone }()
	receive := func(ch <-chan *videoFrame) *videoFrame {
		select {
		case frame, ok := <-ch:
			if !ok {
				t.Fatal("thermal raw subscriber closed before receiving measurements")
			}
			return frame
		case <-ctx.Done():
			t.Fatal("thermal raw subscriber did not receive measurements before deadline")
			return nil
		}
	}
	assertFrame := func(frame *videoFrame) int {
		if frame.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || frame.tsNs == 0 || frame.rawFmt.GetFourcc() != "Y16 " ||
			frame.rawFmt.GetWidth() != width || frame.rawFmt.GetHeight() != height || frame.rawFmt.GetBytesPerLine() != width*2 {
			t.Fatalf("thermal raw subscriber received preview or wrong layout: codec=%v timestamp=%d format=%v", frame.codec, frame.tsNs, frame.rawFmt)
		}
		if len(frame.data) != width*height*2 {
			t.Fatalf("thermal raw subscriber frame size = %d, want %d", len(frame.data), width*height*2)
		}
		phase := int(binary.LittleEndian.Uint16(frame.data)) - 0x6001
		if phase < 0 || phase >= len(patterns) {
			t.Fatalf("thermal raw subscriber samples changed: first measurement = %04x", binary.LittleEndian.Uint16(frame.data))
		}
		for pixel := 0; pixel < width*height; pixel++ {
			got := binary.LittleEndian.Uint16(frame.data[2*pixel:])
			want := binary.LittleEndian.Uint16(patterns[phase][2*pixel:])
			if got != want {
				t.Fatalf("thermal raw subscriber samples changed at (%d,%d): got %04x, want %04x", pixel%width, pixel/width, got, want)
			}
		}
		return phase
	}
	retained := receive(firstFrames)
	retainedPhase := assertFrame(retained)
	// A subscriber joining an active producer receives measurements too.
	secondID, secondFrames, err := hub.subscribeKind(true)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.unsubscribe(secondID)
	for _, frames := range []<-chan *videoFrame{firstFrames, secondFrames} {
		seen := [2]bool{}
		for !seen[0] || !seen[1] {
			seen[assertFrame(receive(frames))] = true
		}
	}
	if assertFrame(retained) != retainedPhase {
		t.Fatal("later pipe reads overwrote the retained thermal measurement frame")
	}
}
