package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

const cameraPreviewIdle = 15 * time.Second

// One continuous device subscription and decoder per explicitly started viewer.
// Only the latest frame is retained. Frame bytes stay in UI-only result metadata.
type gatewayCameraPreview struct {
	mu             sync.Mutex
	subject, robot string
	camera         int64
	cancel         context.CancelFunc
	lastRead       time.Time
	frame          cameraSnapshot
	sequence       uint64
	observed       time.Time
	changed        chan struct{}
}

func (g *RobotGateway) registerCameraPreviewTools() {
	g.previews = make(map[string]*gatewayCameraPreview)
	for _, name := range []string{"start_camera_preview", "read_camera_preview", "stop_camera_preview"} {
		opts := []mcpgo.ToolOption{robotArgument()}
		description := "Start a live camera preview for this app. Activates the camera until Stop, tab close, a 15-second idle timeout, or the 10-minute session limit. Frames are not sent to model context."
		behavior := mutating()
		if name == "start_camera_preview" {
			opts = append(opts, mcpgo.WithInteger("camera_id", mcpgo.Required(), mcpgo.Min(0), mcpgo.Max(4294967295)))
		} else {
			opts = append(opts, mcpgo.WithString("preview_id", mcpgo.Required(), mcpgo.MaxLength(64)))
			description = "Stop your camera preview and release its capture subscription."
			if name == "read_camera_preview" {
				description = "Read the latest frame of your active camera preview. Renews the viewer lease. Image bytes are UI-only metadata."
				behavior = readOnly()
				opts = append(opts, mcpgo.WithInteger("after_sequence", mcpgo.Description("Last frame sequence received. Omits image bytes when no newer frame is available."), mcpgo.Min(0), mcpgo.Max(9007199254740991)))
			}
		}
		t := gatewayTool(name, description, behavior, opts...)
		t.Meta = mcpgo.NewMetaFromMap(map[string]any{"ui": map[string]any{"visibility": []string{"app"}}, "openai/widgetAccessible": true})
		switch name {
		case "start_camera_preview":
			g.protocol.AddTool(t, g.startCameraPreview)
		case "read_camera_preview":
			g.protocol.AddTool(t, g.readCameraPreview)
		case "stop_camera_preview":
			g.protocol.AddTool(t, g.stopCameraPreview)
		}
	}
}

func (g *RobotGateway) startCameraPreview(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	r, err := g.authorize(ctx, req.GetString("robot_id", ""), RobotCameraScope)
	if err != nil || !r.AllowCamera {
		return mcpgo.NewToolResultError("Camera access is not authorized."), nil
	}
	camera, err := snapshotInteger(req, "camera_id", -1, 0, 4294967295)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	path, err := resolveSnapshotGStreamer()
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	_, principal := g.grant(ctx)
	// The explicit start action owns this bounded background stream. Request
	// cancellation must not kill it when the start RPC completes.
	liveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	p := &gatewayCameraPreview{subject: principal.Subject, robot: r.ID, camera: camera, cancel: cancel, lastRead: time.Now(), changed: make(chan struct{})}
	id := uuid.NewString()
	g.previewMu.Lock()
	if len(g.previews) >= 8 {
		g.previewMu.Unlock()
		cancel()
		return mcpgo.NewToolResultError("All camera preview slots are in use. Stop another preview and retry."), nil
	}
	g.previews[id] = p
	g.previewMu.Unlock()
	go func() {
		defer cancel()
		defer func() { g.previewMu.Lock(); delete(g.previews, id); g.previewMu.Unlock() }()
		go p.expireWhenIdle(liveCtx)
		conn, err := g.connect(liveCtx, r.Device)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = streamCameraPreview(liveCtx, conn.VideoService, &agentpb.StreamVideoRequest{DeviceId: uint32(camera), Codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, func(ctx context.Context, codec agentpb.VideoCodec) (*exec.Cmd, error) {
			args, err := cameraPreviewPipeline(codec)
			if err != nil {
				return nil, err
			}
			return exec.CommandContext(ctx, path, args...), nil
		}, p.publish)
	}()
	// Only report success after the camera supplies a frame. A failed startup
	// releases the stream rather than leaving an invisible camera running.
	select {
	case <-p.changed:
		return okResult(map[string]any{"preview_id": id, "robot_id": r.ID, "camera_id": camera, "live": true, "expires_in_seconds": 600}), nil
	case <-liveCtx.Done():
		return mcpgo.NewToolResultError("Could not start this camera. Check its connection and camera availability."), nil
	case <-ctx.Done():
		cancel()
		return mcpgo.NewToolResultError("Camera preview canceled."), nil
	case <-time.After(15 * time.Second):
		cancel()
		return mcpgo.NewToolResultError("Camera preview timed out."), nil
	}
}

func (p *gatewayCameraPreview) expireWhenIdle(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.Lock()
			idle := time.Since(p.lastRead) >= cameraPreviewIdle
			p.mu.Unlock()
			if idle {
				p.cancel()
				return
			}
		}
	}
}
func (p *gatewayCameraPreview) publish(frame cameraSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frame = frame
	p.sequence++
	p.observed = time.Now().UTC()
	if p.sequence == 1 {
		close(p.changed)
	}
}
func (g *RobotGateway) cameraPreview(ctx context.Context, req mcpgo.CallToolRequest) (*gatewayCameraPreview, error) {
	if !g.hasScope(ctx, RobotCameraScope) {
		return nil, fmt.Errorf("Camera access is not authorized.")
	}
	_, principal := g.grant(ctx)
	g.previewMu.Lock()
	p := g.previews[req.GetString("preview_id", "")]
	g.previewMu.Unlock()
	if p == nil || p.subject != principal.Subject || p.robot != req.GetString("robot_id", "") {
		return nil, fmt.Errorf("Camera preview ended. Start it again to reconnect.")
	}
	return p, nil
}
func (g *RobotGateway) readCameraPreview(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	p, err := g.cameraPreview(ctx, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	previous := int64(-1)
	if _, supplied := req.GetArguments()["after_sequence"]; supplied {
		previous, err = snapshotInteger(req, "after_sequence", -1, 0, 9007199254740991)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastRead = time.Now()
	r := okResult(map[string]any{"sequence": p.sequence, "received_at": p.observed.Format(time.RFC3339Nano), "width": p.frame.width, "height": p.frame.height, "live": true})
	if previous < 0 || p.sequence > uint64(previous) {
		r.Meta = mcpgo.NewMetaFromMap(map[string]any{"frame": map[string]any{"data": base64.StdEncoding.EncodeToString(p.frame.jpeg), "mimeType": "image/jpeg"}})
	}
	return r, nil
}
func (g *RobotGateway) stopCameraPreview(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	p, err := g.cameraPreview(ctx, req)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	p.cancel()
	g.previewMu.Lock()
	delete(g.previews, req.GetString("preview_id", ""))
	g.previewMu.Unlock()
	return okResult(map[string]any{"stopped": true}), nil
}

func cameraPreviewPipeline(codec agentpb.VideoCodec) ([]string, error) {
	args, err := cameraSnapshotPipeline(codec)
	if err != nil {
		return nil, err
	}
	for i, v := range args {
		switch v {
		case "snapshot=true":
			args[i] = "snapshot=false"
		case "quality=80":
			args[i] = "quality=70"
		case "video/x-raw,width=[1,1280],height=[1,1280],pixel-aspect-ratio=1/1":
			args[i] = "video/x-raw,width=[1,640],height=[1,640],pixel-aspect-ratio=1/1"
		}
		if v == "jpegenc" {
			args = append(append(append([]string{}, args[:i]...), "videorate", "drop-only=true", "max-rate=5", "!"), args[i:]...)
			break
		}
	}
	// jpegenc parameters come after its element, so update them after insertion.
	for i, v := range args {
		if v == "snapshot=true" {
			args[i] = "snapshot=false"
		}
		if v == "quality=80" {
			args[i] = "quality=70"
		}
	}
	return args, nil
}

func streamCameraPreview(ctx context.Context, client cameraSnapshotClient, request *agentpb.StreamVideoRequest, decoder func(context.Context, agentpb.VideoCodec) (*exec.Cmd, error), publish func(cameraSnapshot)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.StreamVideo(ctx, request, grpc.MaxCallRecvMsgSize(8<<20))
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first == nil {
		return fmt.Errorf("empty video frame")
	}
	cmd, err := decoder(ctx, first.GetCodec())
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	cmd.Stderr = &snapshotBuffer{limit: 4096}
	cmd.WaitDelay = 2 * time.Second
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return err
	}
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer stdin.Close()
		frame := first
		for {
			if frame == nil || frame.GetCodec() != first.GetCodec() || len(frame.GetData()) > 8<<20 {
				cancel()
				return
			}
			if _, err := stdin.Write(frame.GetData()); err != nil {
				return
			}
			var err error
			frame, err = stream.Recv()
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); _ = stdin.Close(); _ = cmd.Wait(); <-feedDone }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), snapshotMaxBytes+1)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if end := bytes.Index(data, []byte{0xff, 0xd9}); end >= 0 {
			return end + 2, data[:end+2], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, fmt.Errorf("incomplete preview frame")
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		frame, err := validateSnapshotJPEG(scanner.Bytes())
		if err != nil {
			return err
		}
		publish(frame)
	}
	return scanner.Err()
}
