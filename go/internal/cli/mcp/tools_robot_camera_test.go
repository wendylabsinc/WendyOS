package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/jpeg"
	"io"
	"os/exec"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/testfixture/go2video"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type robotCameraFake struct {
	grpc.ClientStream
	frames    []*pb.UnitreeStreamMessage
	ctx       context.Context
	request   *pb.UnitreeStreamRequest
	clock     int64
	clockErr  error
	clockRead bool
	eof       bool
}

func (f *robotCameraFake) GetClock(context.Context, *pb.GetClockRequest, ...grpc.CallOption) (*pb.GetClockResponse, error) {
	f.clockRead = true
	return &pb.GetClockResponse{UnixNanos: f.clock}, f.clockErr
}

func (f *robotCameraFake) Stream(ctx context.Context, req *pb.UnitreeStreamRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.UnitreeStreamMessage], error) {
	if !f.clockRead {
		return nil, errors.New("stream opened before clock check")
	}
	f.ctx, f.request = ctx, req
	return f, nil
}

func (f *robotCameraFake) Recv() (*pb.UnitreeStreamMessage, error) {
	if len(f.frames) != 0 {
		frame := f.frames[0]
		f.frames = f.frames[1:]
		return frame, nil
	}
	if f.eof {
		return nil, io.EOF
	}
	<-f.ctx.Done()
	return nil, f.ctx.Err()
}

func robotCameraFrame(data []byte, timestamp int64) *pb.UnitreeStreamMessage {
	return &pb.UnitreeStreamMessage{Kind: &pb.UnitreeStreamMessage_Video{Video: &pb.UnitreeVideoFrame{Annexb: data, ReceivedUnixNanos: timestamp}}}
}

var robotCameraTestIDR = []byte{0, 0, 0, 1, 0x65, 0x88}

func TestRobotCameraRegistrationAndSelection(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv := server.NewMCPServer("test", "1")
	s.registerRobotCameraTools(srv)
	tool := srv.GetTool("robot_camera_snapshot")
	if tool == nil || boolVal(t, tool.Tool.Annotations.ReadOnlyHint, "ReadOnlyHint") || boolVal(t, tool.Tool.Annotations.DestructiveHint, "DestructiveHint") {
		t.Fatal("robot camera must require approval without a destructive annotation")
	}
	result, err := s.handleRobotCameraSnapshot(context.Background(), callToolReq("robot_camera_snapshot", nil))
	if err != nil || !result.IsError || structuredMap(t, result)["error_code"] != "NOT_CONNECTED" {
		t.Fatalf("disconnected result: %v %v", result, err)
	}
	a, b := &pb.UnitreeRobot{Serial: "a", Paired: true}, &pb.UnitreeRobot{Serial: "b", Paired: true}
	for _, tc := range []struct {
		name, requested, want string
		robots                []*pb.UnitreeRobot
		fail                  bool
	}{
		{name: "sole paired", robots: []*pb.UnitreeRobot{nil, a, {Serial: "unpaired"}}, want: "a"},
		{name: "explicit", requested: "b", robots: []*pb.UnitreeRobot{a, b}, want: "b"},
		{name: "ambiguous", robots: []*pb.UnitreeRobot{a, b}, fail: true},
		{name: "unknown", requested: "z", robots: []*pb.UnitreeRobot{a}, fail: true},
		{name: "none", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectRobotCameraSerial(tc.requested, tc.robots)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("got %q %v", got, err)
			}
		})
	}
}

func TestRobotCameraImageAndFreshnessMetadata(t *testing.T) {
	jpegBytes := snapshotTestJPEG(t, 32, 24)
	result, err := robotCameraSnapshotResult(context.Background(), callToolReq("robot_camera_snapshot", map[string]any{"serial": "test-go2"}), func(ctx context.Context, serial string) (string, cameraSnapshot, int64, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		return serial, cameraSnapshot{jpeg: jpegBytes, width: 32, height: 24, timestampNS: 102}, 100, nil
	})
	if err != nil || result.IsError {
		t.Fatalf("%v %v", result, err)
	}
	metadata := structuredMap(t, result)
	if metadata["serial"] != "test-go2" || metadata["source"] != "go2_onboard_camera" || metadata["freshness_basis"] != "gateway_receipt_after_request" || metadata["live"] != false {
		t.Fatalf("metadata: %v", metadata)
	}
	var imageFound bool
	for _, content := range result.Content {
		if image, ok := content.(mcpgo.ImageContent); ok {
			decoded, err := base64.StdEncoding.DecodeString(image.Data)
			if err != nil || image.MIMEType != "image/jpeg" || !bytes.Equal(decoded, jpegBytes) {
				t.Fatal("wrong image content")
			}
			imageFound = true
		}
	}
	if !imageFound {
		t.Fatal("missing actual image content")
	}
	for _, args := range []map[string]any{{"timeout_seconds": 31}, {"timeout_seconds": 1.5}, {"serial": 1}, {"serial": ""}} {
		result, _ := robotCameraSnapshotResult(context.Background(), callToolReq("robot_camera_snapshot", args), func(context.Context, string) (string, cameraSnapshot, int64, error) {
			t.Fatal("invalid request captured")
			return "", cameraSnapshot{}, 0, nil
		})
		if !result.IsError {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRobotCameraSkipsCachedAndDependentFrames(t *testing.T) {
	fake := &robotCameraFake{clock: 100, eof: true, frames: []*pb.UnitreeStreamMessage{
		robotCameraFrame(robotCameraTestIDR, 99), robotCameraFrame(robotCameraTestIDR, 100),
		robotCameraFrame([]byte{0, 0, 0, 1, 0x41, 0x88}, 101), robotCameraFrame(robotCameraTestIDR, 102),
	}}
	shot, cutoff, err := captureRobotCameraSnapshot(context.Background(), fake, fake, "test-go2", snapshotTestDecoder("jpeg", snapshotTestJPEG(t, 32, 24), nil))
	if err != nil || cutoff != 100 || shot.timestampNS != 102 {
		t.Fatalf("shot %v cutoff %d err %v", shot, cutoff, err)
	}
	if fake.ctx.Err() == nil {
		t.Fatal("video subscription not canceled")
	}
	if !fake.request.Video || len(fake.request.Topics) != 0 || fake.request.Serial != "test-go2" {
		t.Fatalf("request %v", fake.request)
	}
}

func TestRobotCameraCancellationAndClockFailure(t *testing.T) {
	for _, frames := range [][]*pb.UnitreeStreamMessage{nil, {robotCameraFrame(robotCameraTestIDR, 101)}} {
		fake := &robotCameraFake{clock: 100, frames: frames}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		var cmd *exec.Cmd
		_, _, err := captureRobotCameraSnapshot(ctx, fake, fake, "test-go2", snapshotTestDecoder("wait", nil, &cmd))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || fake.ctx.Err() == nil {
			t.Fatalf("stream cancellation err=%v ctx=%v", err, fake.ctx.Err())
		}
		if len(frames) > 0 && (cmd == nil || cmd.ProcessState == nil) {
			t.Fatal("decoder was not reaped")
		}
	}
	for _, fake := range []*robotCameraFake{{clock: 0}, {clock: 100, clockErr: errors.New("clock unavailable")}} {
		_, _, err := captureRobotCameraSnapshot(context.Background(), fake, fake, "test-go2", snapshotTestDecoder("wait", nil, nil))
		if err == nil || fake.request != nil {
			t.Fatal("camera opened without a usable gateway clock")
		}
	}
}

func TestRobotCameraBoundsSkippedBytesAndRejectsClockRegression(t *testing.T) {
	large := make([]byte, 1<<20)
	fake := &robotCameraFake{clock: 100, eof: true}
	for i := 0; i < 33; i++ {
		fake.frames = append(fake.frames, robotCameraFrame(large, 99))
	}
	_, _, err := captureRobotCameraSnapshot(context.Background(), fake, fake, "test-go2", snapshotTestDecoder("wait", nil, nil))
	if !errors.Is(err, errRobotCameraByteLimit) || fake.ctx.Err() == nil {
		t.Fatalf("limit err %v", err)
	}
	fake = &robotCameraFake{clock: 100, frames: []*pb.UnitreeStreamMessage{robotCameraFrame(robotCameraTestIDR, 102), robotCameraFrame(robotCameraTestIDR, 101)}}
	fake.clockRead = true
	stream, err := (&robotCameraAdapter{client: fake, serial: "test-go2", cutoff: 100}).StreamVideo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); !errors.Is(err, errRobotCameraClockRegression) {
		t.Fatalf("regression err %v", err)
	}
}

// This exercises the real local GStreamer H.264-to-JPEG process. Generated
// red cached video must be discarded and fresh green pixels must reach Chat.
func TestRobotCameraRealGStreamerFreshJPEG(t *testing.T) {
	path, err := resolveSnapshotGStreamer()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	frames, err := go2video.Generate(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &robotCameraFake{clock: 100, eof: true, frames: []*pb.UnitreeStreamMessage{robotCameraFrame(frames[0], 99), robotCameraFrame(frames[1], 101)}}
	shot, _, err := captureRobotCameraSnapshot(ctx, fake, fake, "test-go2", func(ctx context.Context, codec agentpb.VideoCodec) (*exec.Cmd, error) {
		args, err := cameraSnapshotPipeline(codec)
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, path, args...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.ctx.Err() == nil || shot.width != go2video.Width || shot.height != go2video.Height || shot.timestampNS != 101 {
		t.Fatalf("wrong snapshot dimensions/freshness %dx%d ts=%d", shot.width, shot.height, shot.timestampNS)
	}
	image, err := jpeg.Decode(bytes.NewReader(shot.jpeg))
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range [][2]int{{64, 36}, {320, 180}, {576, 324}} {
		r, g, b, _ := image.At(point[0], point[1]).RGBA()
		if g < 200*257 || r > 45*257 || b > 45*257 {
			t.Fatalf("stale/wrong JPEG pixels RGB=%d,%d,%d", r/257, g/257, b/257)
		}
	}
}

func TestRobotCameraRealGStreamerRequiresNativeCalibrationMode(t *testing.T) {
	path, err := resolveSnapshotGStreamer()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	frames, err := go2video.Generate(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	decode := func(ctx context.Context, codec agentpb.VideoCodec) (*exec.Cmd, error) {
		args, err := robotCameraSnapshotPipeline(codec, true)
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, path, args...), nil
	}
	fake := &robotCameraFake{clock: 100, eof: true, frames: []*pb.UnitreeStreamMessage{robotCameraFrame(frames[1], 101)}}
	if _, _, err := captureRobotCameraSnapshot(ctx, fake, fake, "test-go2", decode); err == nil {
		t.Fatal("640x360 encoded source accepted as calibrated 1280x720")
	}
	// Encode a native 1280x720 stream independently of the snapshot pipeline.
	encode := exec.CommandContext(ctx, path, "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!", "videoconvert", "!", "videoscale", "!", "video/x-raw,format=I420,width=1280,height=720", "!", "x264enc", "tune=zerolatency", "speed-preset=ultrafast", "key-int-max=1", "byte-stream=true", "!", "h264parse", "config-interval=-1", "!", "video/x-h264,stream-format=byte-stream,alignment=au", "!", "fdsink", "fd=1")
	encode.Stdin = bytes.NewReader(frames[1])
	var diagnostic bytes.Buffer
	encode.Stderr = &diagnostic
	encode.WaitDelay = time.Second
	native, err := encode.Output()
	if err != nil {
		t.Fatalf("native fixture: %v: %s", err, &diagnostic)
	}
	fake = &robotCameraFake{clock: 100, eof: true, frames: []*pb.UnitreeStreamMessage{robotCameraFrame(native, 101)}}
	shot, _, err := captureRobotCameraSnapshot(ctx, fake, fake, "test-go2", decode)
	if err != nil {
		t.Fatal(err)
	}
	if shot.width != 1280 || shot.height != 720 {
		t.Fatalf("native mode unexpectedly resized to %dx%d", shot.width, shot.height)
	}
	image, err := jpeg.Decode(bytes.NewReader(shot.jpeg))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := image.At(640, 360).RGBA()
	if g < 200*257 || r > 45*257 || b > 45*257 {
		t.Fatalf("native JPEG wrong pixels RGB=%d,%d,%d", r/257, g/257, b/257)
	}
}
