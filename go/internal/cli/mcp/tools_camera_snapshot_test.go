package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

func snapshotTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 80, A: 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type snapshotFakeClient struct {
	frames  []*agentpb.VideoFrame
	eof     bool
	ctx     context.Context
	request *agentpb.StreamVideoRequest
}

func (c *snapshotFakeClient) StreamVideo(ctx context.Context, req *agentpb.StreamVideoRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.VideoFrame], error) {
	c.ctx, c.request = ctx, req
	return &snapshotFakeStream{ctx: ctx, frames: c.frames, eof: c.eof}, nil
}

type snapshotFakeStream struct {
	grpc.ClientStream
	ctx    context.Context
	frames []*agentpb.VideoFrame
	eof    bool
}

func (s *snapshotFakeStream) Recv() (*agentpb.VideoFrame, error) {
	if len(s.frames) > 0 {
		frame := s.frames[0]
		s.frames = s.frames[1:]
		return frame, nil
	}
	if s.eof {
		return nil, io.EOF
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func snapshotTestDecoder(mode string, jpegBytes []byte, observed **exec.Cmd) func(context.Context, agentpb.VideoCodec) (*exec.Cmd, error) {
	return func(ctx context.Context, _ agentpb.VideoCodec) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCameraSnapshotDecoderProcess$")
		cmd.Env = append(os.Environ(), "WENDY_TEST_SNAPSHOT_DECODER="+mode, "WENDY_TEST_SNAPSHOT_JPEG="+base64.StdEncoding.EncodeToString(jpegBytes))
		if observed != nil {
			*observed = cmd
		}
		return cmd, nil
	}
}

// Runs only as a subprocess: no real camera or decoder dependency is used by
// the capture lifecycle tests.
func TestCameraSnapshotDecoderProcess(t *testing.T) {
	mode := os.Getenv("WENDY_TEST_SNAPSHOT_DECODER")
	if mode == "" {
		return
	}
	switch mode {
	case "wait":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "large":
		_, _ = os.Stdout.Write(make([]byte, snapshotMaxBytes+1))
	default:
		_, _ = io.ReadFull(os.Stdin, make([]byte, 1))
		data, _ := base64.StdEncoding.DecodeString(os.Getenv("WENDY_TEST_SNAPSHOT_JPEG"))
		_, _ = os.Stdout.Write(data)
	}
	os.Exit(0)
}

func TestCameraSnapshotRegisteredAndRequiresApproval(t *testing.T) {
	s := New(&config.Config{}, nil)
	mcp := server.NewMCPServer("test", "1")
	s.registerCameraTools(mcp)
	tool := mcp.GetTool("camera_snapshot")
	if tool == nil || boolVal(t, tool.Tool.Annotations.ReadOnlyHint, "ReadOnlyHint") || boolVal(t, tool.Tool.Annotations.DestructiveHint, "DestructiveHint") {
		t.Fatal("snapshot must be registered as an approval-gated, non-destructive tool")
	}
	result, err := s.handleCameraSnapshot(context.Background(), callToolReq("camera_snapshot", map[string]any{"device_id": 0}))
	if err != nil || !result.IsError || structuredMap(t, result)["error_code"] != "NOT_CONNECTED" {
		t.Fatalf("snapshot without a connection: %v, %v", result, err)
	}
}

func TestCameraSnapshotReturnsImageAndFiniteMetadata(t *testing.T) {
	encoded := snapshotTestJPEG(t, 32, 24)
	result, err := cameraSnapshotResult(context.Background(), callToolReq("camera_snapshot", map[string]any{"device_id": 3}), func(ctx context.Context, req *agentpb.StreamVideoRequest) (cameraSnapshot, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second || req.DeviceId != 3 || req.Codec != agentpb.VideoCodec_VIDEO_CODEC_H264 {
			t.Fatal("capture must receive the selected camera and finite default deadline")
		}
		shot, err := validateSnapshotJPEG(encoded)
		shot.timestampNS = 123456
		return shot, err
	})
	if err != nil || result.IsError || len(result.Content) != 2 {
		t.Fatalf("snapshot result: %+v, %v", result, err)
	}
	img, ok := result.Content[1].(mcpgo.ImageContent)
	if !ok || img.MIMEType != "image/jpeg" {
		t.Fatalf("expected a JPEG ImageContent, got %#v", result.Content[1])
	}
	data, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("image payload is not a complete JPEG: %v", err)
	}
	meta := structuredMap(t, result)
	if meta["live"] != false || meta["width"] != 32 || meta["height"] != 24 || meta["stream_timestamp_ns"] != uint64(123456) {
		t.Fatalf("snapshot metadata is incomplete: %#v", meta)
	}
	if _, err := time.Parse(time.RFC3339Nano, meta["received_at"].(string)); err != nil {
		t.Fatal("snapshot must include a timestamp")
	}
	if strings.Contains(toolResultText(t, result), img.Data) {
		t.Fatal("image base64 must not appear in text content")
	}
}

func TestCameraSnapshotRejectsInvalidArgumentsBeforeCapture(t *testing.T) {
	for _, args := range []map[string]any{
		nil, {"device_id": -1}, {"device_id": 0.5}, {"device_id": "2"}, {"device_id": uint64(1) << 32},
		{"device_id": 0, "timeout_seconds": 0}, {"device_id": 0, "timeout_seconds": 31}, {"device_id": 0, "timeout_seconds": 1.5},
	} {
		result, err := cameraSnapshotResult(context.Background(), callToolReq("camera_snapshot", args), func(context.Context, *agentpb.StreamVideoRequest) (cameraSnapshot, error) {
			t.Fatalf("capture ran for invalid arguments: %#v", args)
			return cameraSnapshot{}, nil
		})
		if err != nil || !result.IsError || structuredMap(t, result)["error_code"] != "INVALID_ARGUMENT" {
			t.Fatalf("invalid args %#v: %v, %v", args, result, err)
		}
	}
}

func TestCameraSnapshotCaptureClosesStreamAndReapsDecoder(t *testing.T) {
	client := &snapshotFakeClient{frames: []*agentpb.VideoFrame{{Data: []byte("encoded video"), TimestampNs: 987}}}
	var decoder *exec.Cmd
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	shot, err := captureCameraSnapshot(ctx, client, &agentpb.StreamVideoRequest{DeviceId: 5}, snapshotTestDecoder("image", snapshotTestJPEG(t, 16, 12), &decoder))
	if err != nil || shot.width != 16 || shot.height != 12 || shot.timestampNS != 987 {
		t.Fatalf("capture: %+v, %v", shot, err)
	}
	if client.ctx.Err() == nil || decoder.ProcessState == nil {
		t.Fatal("snapshot returned while its stream or decoder was still running")
	}
}

func TestCameraSnapshotDecoderEarlyExitWinsOverBrokenInputPipe(t *testing.T) {
	// The helper reads only one byte, emits a complete JPEG, then exits. The
	// much larger input remains blocked in Write and receives EPIPE when the
	// decoder closes; that expected closure must not discard a valid snapshot.
	client := &snapshotFakeClient{frames: []*agentpb.VideoFrame{{Data: make([]byte, 512<<10)}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var decoder *exec.Cmd
	shot, err := captureCameraSnapshot(ctx, client, &agentpb.StreamVideoRequest{}, snapshotTestDecoder("image", snapshotTestJPEG(t, 16, 12), &decoder))
	if err != nil || shot.width != 16 || shot.height != 12 {
		t.Fatalf("valid JPEG lost when decoder closed its input: %+v, %v", shot, err)
	}
	if decoder.ProcessState == nil || !decoder.ProcessState.Success() || client.ctx.Err() == nil {
		t.Fatal("early decoder exit did not leave all capture resources closed")
	}
}

func TestCameraSnapshotTimeoutCancelsReceiveAndDecoder(t *testing.T) {
	for _, firstFrame := range []bool{false, true} {
		client := &snapshotFakeClient{}
		if firstFrame {
			client.frames = []*agentpb.VideoFrame{{Data: []byte("encoded")}}
		}
		var decoder *exec.Cmd
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := captureCameraSnapshot(ctx, client, &agentpb.StreamVideoRequest{}, snapshotTestDecoder("wait", nil, &decoder))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || client.ctx.Err() == nil {
			t.Fatalf("timeout did not stop camera read (first=%v): %v", firstFrame, err)
		}
		if firstFrame && (decoder == nil || decoder.ProcessState == nil) {
			t.Fatal("decoder process was not reaped after timeout")
		}
	}
}

func TestCameraSnapshotRejectsOversizedOrInvalidImage(t *testing.T) {
	for _, encoded := range [][]byte{
		[]byte("not an image"), snapshotTestJPEG(t, snapshotMaxEdge+1, 2), make([]byte, snapshotMaxBytes+1),
	} {
		if _, err := validateSnapshotJPEG(encoded); err == nil {
			t.Fatal("accepted oversized or invalid camera JPEG")
		}
	}
	client := &snapshotFakeClient{frames: []*agentpb.VideoFrame{{Data: []byte("video")}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := captureCameraSnapshot(ctx, client, &agentpb.StreamVideoRequest{}, snapshotTestDecoder("large", nil, nil))
	if err == nil || !strings.Contains(err.Error(), "2 MiB") || client.ctx.Err() == nil {
		t.Fatalf("decoder byte cap not enforced: %v", err)
	}
}

func TestCameraSnapshotPipelineIsFiniteForSupportedCodecs(t *testing.T) {
	for _, codec := range []agentpb.VideoCodec{agentpb.VideoCodec_VIDEO_CODEC_H264, agentpb.VideoCodec_VIDEO_CODEC_VP8} {
		args, err := cameraSnapshotPipeline(codec)
		if err != nil || !strings.Contains(strings.Join(args, " "), "jpegenc snapshot=true") || !strings.Contains(strings.Join(args, " "), "height=[1,1280]") {
			t.Fatalf("unbounded or invalid snapshot pipeline: %v %v", args, err)
		}
		pipeline := strings.Join(args, " ")
		decoder := "avdec_h264"
		if codec == agentpb.VideoCodec_VIDEO_CODEC_VP8 {
			decoder = "vp8dec"
		}
		bound := strings.Index(pipeline, "width=[1,4096],height=[1,2160]")
		if bound < 0 || bound > strings.Index(pipeline, decoder) {
			t.Fatalf("source dimensions must be bounded before decoder allocation: %s", pipeline)
		}
	}
	if _, err := cameraSnapshotPipeline(agentpb.VideoCodec_VIDEO_CODEC_RAW); err == nil {
		t.Fatal("raw frames cannot enter an encoded-video decoder")
	}
}

func TestCameraSnapshotGStreamerSyntheticVideo(t *testing.T) {
	gst, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		t.Skip("GStreamer not installed; lifecycle tests use a fake decoder")
	}
	inspect, err := exec.LookPath("gst-inspect-1.0")
	if err != nil {
		t.Skip("GStreamer plugin inspection unavailable")
	}
	for _, codec := range []agentpb.VideoCodec{agentpb.VideoCodec_VIDEO_CODEC_H264, agentpb.VideoCodec_VIDEO_CODEC_VP8} {
		t.Run(codec.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			plugins := []string{"videotestsrc", "fdsrc", "fdsink", "videoconvert", "videoscale", "jpegenc"}
			if codec == agentpb.VideoCodec_VIDEO_CODEC_H264 {
				plugins = append(plugins, "x264enc", "typefind", "h264parse", "avdec_h264")
			} else {
				plugins = append(plugins, "vp8enc", "webmmux", "matroskademux", "vp8dec")
			}
			for _, plugin := range plugins {
				if err := exec.CommandContext(ctx, inspect, "--exists", plugin).Run(); err != nil {
					if ctx.Err() != nil {
						t.Fatalf("GStreamer prerequisite checks timed out: %v", ctx.Err())
					}
					t.Skipf("optional GStreamer plugin %s unavailable", plugin)
				}
			}
			args := []string{"-q", "videotestsrc", "num-buffers=3", "!", "video/x-raw,width=1920,height=1080,framerate=10/1", "!"}
			if codec == agentpb.VideoCodec_VIDEO_CODEC_H264 {
				args = append(args, "x264enc", "tune=zerolatency", "!", "h264parse", "!", "video/x-h264,stream-format=byte-stream")
			} else {
				args = append(args, "vp8enc", "deadline=1", "!", "webmmux", "streamable=true")
			}
			args = append(args, "!", "fdsink", "fd=1")
			var diagnostics bytes.Buffer
			encode := exec.CommandContext(ctx, gst, args...)
			encode.Stderr = &diagnostics
			video, err := encode.Output()
			if err != nil {
				t.Fatalf("encoding synthetic video: %v: %s", err, diagnostics.String())
			}
			client := &snapshotFakeClient{frames: []*agentpb.VideoFrame{{Data: video, Codec: codec}}, eof: true}
			shot, err := captureCameraSnapshot(ctx, client, &agentpb.StreamVideoRequest{}, func(ctx context.Context, codec agentpb.VideoCodec) (*exec.Cmd, error) {
				args, err := cameraSnapshotPipeline(codec)
				return exec.CommandContext(ctx, gst, args...), err
			})
			if err != nil {
				t.Fatalf("decoding synthetic camera video: %v", err)
			}
			if shot.width != 1280 || shot.height != 720 || len(shot.jpeg) > snapshotMaxBytes || client.ctx.Err() == nil {
				t.Fatalf("snapshot not bounded or camera stream left active: %+v", shot)
			}
		})
	}
}
