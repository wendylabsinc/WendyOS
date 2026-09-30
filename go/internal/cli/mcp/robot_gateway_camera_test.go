package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestCameraPreviewDecoderProcess(t *testing.T) {
	if os.Getenv("WENDY_TEST_PREVIEW") == "" {
		return
	}
	_, _ = io.ReadFull(os.Stdin, make([]byte, 1))
	data, _ := base64.StdEncoding.DecodeString(os.Getenv("WENDY_TEST_PREVIEW"))
	_, _ = os.Stdout.Write(append(data, data...))
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestCameraPreviewKeepsStreamUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &snapshotFakeClient{frames: []*agentpb.VideoFrame{{Data: []byte("video"), Codec: agentpb.VideoCodec_VIDEO_CODEC_H264}}}
	var frames atomic.Int32
	var process *exec.Cmd
	err := streamCameraPreview(ctx, client, &agentpb.StreamVideoRequest{}, func(ctx context.Context, _ agentpb.VideoCodec) (*exec.Cmd, error) {
		process = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCameraPreviewDecoderProcess$")
		process.Env = append(os.Environ(), "WENDY_TEST_PREVIEW="+base64.StdEncoding.EncodeToString(snapshotTestJPEG(t, 16, 12)))
		return process, nil
	}, func(frame cameraSnapshot) {
		if frame.width != 16 || frame.height != 12 {
			t.Error("invalid preview size")
		}
		if frames.Add(1) == 2 {
			cancel()
		}
	})
	if frames.Load() != 2 {
		t.Fatalf("expected two frames, got %d: %v", frames.Load(), err)
	}
	if client.ctx.Err() == nil || process.ProcessState == nil {
		t.Fatal("stream or decoder leaked after cancellation")
	}
}

func TestGatewayCameraPreviewOwnershipAndUIOnlyFrames(t *testing.T) {
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("unauthorized camera must not connect")
		return nil, fmt.Errorf("unexpected")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	other := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"bob", robotGatewayScopes})
	closed, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &gatewayCameraPreview{subject: "alice", robot: "alpha", cancel: cancel, changed: make(chan struct{})}
	p.publish(cameraSnapshot{jpeg: []byte("private image bytes"), width: 16, height: 12})
	g.previews["test"] = p
	req := callToolReq("read_camera_preview", map[string]any{"robot_id": "alpha", "preview_id": "test"})
	denied, _ := g.readCameraPreview(other, req)
	if !denied.IsError {
		t.Fatal("cross-account frame leaked")
	}
	result, _ := g.readCameraPreview(ctx, req)
	if result.IsError || result.Meta == nil {
		t.Fatal("missing frame")
	}
	visible, _ := json.Marshal(result.Content)
	if bytes.Contains(visible, []byte("private")) || len(result.Content) != 1 {
		t.Fatal("frame entered model content")
	}
	if p.lastRead.IsZero() {
		t.Fatal("lease not renewed")
	}
	stopped, _ := g.stopCameraPreview(ctx, req)
	if stopped.IsError || closed.Err() == nil || len(g.previews) != 0 {
		t.Fatal("stop did not release preview")
	}
	denied, _ = g.startCameraPreview(other, callToolReq("start_camera_preview", map[string]any{"robot_id": "alpha", "camera_id": 0}))
	if !denied.IsError {
		t.Fatal("cross-account camera started")
	}
}

func TestCameraPreviewIdleLeaseExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &gatewayCameraPreview{cancel: cancel, lastRead: time.Now().Add(-cameraPreviewIdle)}
	start := time.Now()
	p.expireWhenIdle(ctx)
	if time.Since(start) > 2*time.Second {
		t.Fatal("idle camera was not stopped")
	}
}
