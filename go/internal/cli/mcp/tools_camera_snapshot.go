package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

const (
	snapshotMaxBytes      = 2 << 20
	snapshotMaxEdge       = 1280
	snapshotMaxVideoBytes = 32 << 20
)

func cameraSnapshotTool() mcpgo.Tool {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Capture one still image from a camera on the connected Wendy device and return it for visual inspection. Requires approval because it activates a physical camera. Use camera_list to select device_id. This is a finite snapshot, not a live view: the temporary stream closes after capture. Supports direct and cloud device connections. Local GStreamer with H.264/VP8 decoders and JPEG encoding is required."),
		mcpgo.WithInteger("device_id", mcpgo.Required(), mcpgo.Min(0), mcpgo.Max(math.MaxUint32), mcpgo.Description("Camera id returned by camera_list")),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(30), mcpgo.Description("Maximum time to obtain an image, default 15 seconds")),
	}
	opts = append(opts, mutating()...)
	opts = append(opts, localOnly()...)
	return mcpgo.NewTool("camera_snapshot", opts...)
}

func (s *mcpServer) registerCameraSnapshotTool(srv *server.MCPServer) {
	srv.AddTool(cameraSnapshotTool(), s.handleCameraSnapshot)
}

func (s *mcpServer) handleCameraSnapshot(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	return cameraSnapshotResult(ctx, req, func(ctx context.Context, request *agentpb.StreamVideoRequest) (cameraSnapshot, error) {
		path, err := resolveSnapshotGStreamer()
		if err != nil {
			return cameraSnapshot{}, err
		}
		return captureCameraSnapshot(ctx, conn.VideoService, request, func(ctx context.Context, codec agentpb.VideoCodec) (*exec.Cmd, error) {
			args, err := cameraSnapshotPipeline(codec)
			if err != nil {
				return nil, err
			}
			return exec.CommandContext(ctx, path, args...), nil
		})
	})
}

type cameraSnapshot struct {
	jpeg          []byte
	width, height int
	timestampNS   uint64
}

func cameraSnapshotResult(ctx context.Context, req mcpgo.CallToolRequest, capture func(context.Context, *agentpb.StreamVideoRequest) (cameraSnapshot, error)) (*mcpgo.CallToolResult, error) {
	id, err := snapshotInteger(req, "device_id", -1, 0, math.MaxUint32)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	seconds, err := snapshotInteger(req, "timeout_seconds", 15, 1, 30)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	shot, err := capture(ctx, &agentpb.StreamVideoRequest{DeviceId: uint32(id), Codec: agentpb.VideoCodec_VIDEO_CODEC_H264})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errResult(errCodeTimeout, "Camera snapshot timed out; the temporary stream was canceled. Check that the camera is online and try again."), nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return errResult(errCodeInternal, "Camera snapshot canceled."), nil
		}
		return errResult(codeFromGRPC(err), err.Error()), nil
	}
	metadata := map[string]any{
		"kind": "camera_snapshot", "live": false, "device_id": id,
		"width": shot.width, "height": shot.height, "mime_type": "image/jpeg",
		"received_at": time.Now().UTC().Format(time.RFC3339Nano),
		"note":        "A single still image. The temporary camera stream has closed.",
	}
	if shot.timestampNS != 0 {
		metadata["stream_timestamp_ns"] = shot.timestampNS
	}
	text, _ := json.Marshal(metadata)
	result := mcpgo.NewToolResultImage(string(text), base64.StdEncoding.EncodeToString(shot.jpeg), "image/jpeg")
	result.StructuredContent = metadata
	return result, nil
}

func snapshotInteger(req mcpgo.CallToolRequest, name string, fallback int64, minimum, maximum int64) (int64, error) {
	value, exists := req.GetArguments()[name]
	if !exists {
		if fallback >= minimum && fallback <= maximum {
			return fallback, nil
		}
		return 0, fmt.Errorf("%s is required (see camera_list)", name)
	}
	encoded, err := json.Marshal(value)
	var number json.Number
	if err == nil && len(encoded) > 0 && encoded[0] != '"' {
		err = json.Unmarshal(encoded, &number)
	}
	if err == nil {
		f, parseErr := number.Float64()
		if parseErr == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f && f >= float64(minimum) && f <= float64(maximum) {
			return int64(f), nil
		}
	}
	return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
}

type cameraSnapshotClient interface {
	StreamVideo(context.Context, *agentpb.StreamVideoRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.VideoFrame], error)
}

// captureCameraSnapshot reuses StreamVideo's multiplexed capture and active
// connection. Canceling this subscriber releases its camera capture without
// disconnecting the device or disturbing other viewers.
func captureCameraSnapshot(ctx context.Context, client cameraSnapshotClient, request *agentpb.StreamVideoRequest, decoder func(context.Context, agentpb.VideoCodec) (*exec.Cmd, error)) (cameraSnapshot, error) {
	captureCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.StreamVideo(captureCtx, request, grpc.MaxCallRecvMsgSize(8<<20))
	if err != nil {
		return cameraSnapshot{}, err
	}
	first, err := stream.Recv()
	if err != nil {
		return cameraSnapshot{}, fmt.Errorf("receiving camera image: %w", err)
	}
	if first == nil {
		return cameraSnapshot{}, errors.New("camera returned an empty video frame")
	}
	cmd, err := decoder(captureCtx, first.GetCodec())
	if err != nil {
		return cameraSnapshot{}, err
	}
	stdout, stderr := &snapshotBuffer{limit: snapshotMaxBytes}, &snapshotBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return cameraSnapshot{}, fmt.Errorf("opening camera decoder input: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return cameraSnapshot{}, fmt.Errorf("starting camera decoder: %w", err)
	}
	feedDone, processDone := make(chan struct{}), make(chan struct{})
	var feedErr, processErr error
	var pipeClosed bool
	go func() {
		defer close(feedDone)
		frame, total := first, 0
		for {
			if frame == nil || frame.GetCodec() != first.GetCodec() {
				feedErr = errors.New("camera stream changed video format during snapshot")
				return
			}
			total += len(frame.GetData())
			if total > snapshotMaxVideoBytes {
				feedErr = errors.New("camera snapshot exceeded the 32 MiB encoded-video limit")
				return
			}
			if _, feedErr = stdin.Write(frame.GetData()); feedErr != nil {
				pipeClosed = true
				return
			}
			frame, feedErr = stream.Recv()
			if feedErr != nil {
				return
			}
		}
	}()
	go func() {
		defer close(processDone)
		processErr = cmd.Wait()
	}()
	defer func() {
		cancel()
		_ = stdin.Close()
		<-processDone
		<-feedDone
	}()
	select {
	case <-ctx.Done():
		return cameraSnapshot{}, ctx.Err()
	case <-feedDone:
		_ = stdin.Close()
		if feedErr != nil && !errors.Is(feedErr, io.EOF) && !pipeClosed {
			return cameraSnapshot{}, fmt.Errorf("receiving camera image: %w", feedErr)
		}
		select {
		case <-processDone:
		case <-ctx.Done():
			return cameraSnapshot{}, ctx.Err()
		}
	case <-processDone:
	}
	if ctx.Err() != nil {
		return cameraSnapshot{}, ctx.Err()
	}
	if processErr != nil {
		detail := strings.TrimSpace(string(stderr.bytes()))
		hint := snapshotInstallHint()
		if strings.Contains(detail, "not-negotiated") {
			hint = "Configure the camera to provide a 4K-or-smaller source (at most 4096×2160), then retry."
		}
		return cameraSnapshot{}, fmt.Errorf("decoding camera image: %w: %s. %s", processErr, detail, hint)
	}
	encoded := stdout.bytes()
	if stdout.exceeded() {
		return cameraSnapshot{}, errors.New("camera image exceeds the 2 MiB snapshot limit")
	}
	shot, err := validateSnapshotJPEG(encoded)
	shot.timestampNS = first.GetTimestampNs()
	return shot, err
}

func cameraSnapshotPipeline(codec agentpb.VideoCodec) ([]string, error) {
	args := []string{"-q", "fdsrc", "fd=0", "!"}
	switch codec {
	case agentpb.VideoCodec_VIDEO_CODEC_H264:
		args = append(args, "typefind", "!", "h264parse", "!",
			"video/x-h264,width=[1,4096],height=[1,2160]", "!", "avdec_h264", "max-threads=1")
	case agentpb.VideoCodec_VIDEO_CODEC_VP8:
		args = append(args, "matroskademux", "!",
			"video/x-vp8,width=[1,4096],height=[1,2160]", "!", "vp8dec")
	default:
		return nil, fmt.Errorf("camera snapshot does not support video codec %s", codec)
	}
	return append(args, "!", "videoconvert", "!", "videoscale", "!",
		"video/x-raw,width=[1,1280],height=[1,1280],pixel-aspect-ratio=1/1",
		"!", "jpegenc", "snapshot=true", "quality=80", "!", "fdsink", "fd=1", "sync=false"), nil
}

func validateSnapshotJPEG(encoded []byte) (cameraSnapshot, error) {
	if len(encoded) > snapshotMaxBytes {
		return cameraSnapshot{}, errors.New("camera image exceeds the 2 MiB snapshot limit")
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > snapshotMaxEdge || config.Height > snapshotMaxEdge {
		return cameraSnapshot{}, errors.New("camera decoder did not return a valid JPEG within the 1280-pixel size limit")
	}
	image, err := jpeg.Decode(bytes.NewReader(encoded))
	if err != nil {
		return cameraSnapshot{}, fmt.Errorf("camera decoder returned an incomplete JPEG: %w", err)
	}
	// Re-encode validated pixels: only the finite image, with no extra trailing
	// frames or decoder metadata, can enter the model's multimodal context.
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image, &jpeg.Options{Quality: 80}); err != nil {
		return cameraSnapshot{}, err
	}
	if out.Len() > snapshotMaxBytes {
		return cameraSnapshot{}, errors.New("camera image exceeds the 2 MiB snapshot limit")
	}
	return cameraSnapshot{jpeg: out.Bytes(), width: config.Width, height: config.Height}, nil
}

func resolveSnapshotGStreamer() (string, error) {
	if path, err := exec.LookPath("gst-launch-1.0"); err == nil {
		return path, nil
	}
	paths := []string{"/opt/homebrew/bin/gst-launch-1.0", "/usr/local/bin/gst-launch-1.0", "/usr/bin/gst-launch-1.0"}
	if runtime.GOOS == "windows" {
		for _, key := range []string{"GSTREAMER_1_0_ROOT_MSVC_X86_64", "GSTREAMER_1_0_ROOT_MINGW_X86_64", "GSTREAMER_1_0_ROOT_X86_64"} {
			if root := os.Getenv(key); root != "" {
				paths = append(paths, filepath.Join(root, "bin", "gst-launch-1.0.exe"))
			}
		}
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", fmt.Errorf("camera snapshots require GStreamer on this computer. %s", snapshotInstallHint())
}

func snapshotInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "Install it with `brew install gstreamer`, then retry."
	case "windows":
		return "Install the GStreamer runtime with good/bad/libav plugins and add its bin directory to PATH, then retry."
	default:
		return "Install GStreamer tools, good/bad plugins and libav (on Debian/Ubuntu: `sudo apt install gstreamer1.0-tools gstreamer1.0-plugins-good gstreamer1.0-plugins-bad gstreamer1.0-libav`), then retry."
	}
}

type snapshotBuffer struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	tooLarge bool
}

func (b *snapshotBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(p), max(0, b.limit-len(b.data)))
	b.data = append(b.data, p[:n]...)
	b.tooLarge = b.tooLarge || n < len(p)
	return len(p), nil
}
func (b *snapshotBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}
func (b *snapshotBuffer) exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tooLarge
}
