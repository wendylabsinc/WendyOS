package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestStreamGStreamerReportsCaptureFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   []string
		code   codes.Code
		busy   bool
		cancel bool
	}{
		{
			name: "encoder failure",
			script: "echo 'ERROR: from element x264enc0: Can not initialize encoder.' >&2\n" +
				"echo 'Additional debug info: unsupported input format NV12' >&2\nexit 1\n",
			want: []string{"/dev/video0", "x264enc", "exit status 1", "Can not initialize encoder", "unsupported input format NV12"},
			code: codes.Internal,
		},
		{
			name:   "missing element",
			script: "echo 'WARNING: erroneous pipeline: no element \"videoconvert\"' >&2\nexit 1\n",
			want:   []string{"no element \"videoconvert\""},
			code:   codes.Internal,
		},
		{
			name: "no stderr", script: "exit 2\n", code: codes.Internal,
			want: []string{"/dev/video0", "x264enc", "exit status 2", "no error output", "wendy device logs --tail 50"},
		},
		{
			name: "busy camera", script: "echo 'ERROR: v4l2src: Device /dev/video0 is busy' >&2\nexit 1\n",
			code: codes.FailedPrecondition, busy: true,
		},
		{
			name: "recovered warning", script: "echo 'WARNING: v4l2src: Device or resource busy' >&2\nprintf frame\nexit 0\n",
			code: codes.OK,
		},
		{
			name: "cancelled viewer", script: "echo 'WARNING: camera startup' >&2\nprintf frame\nexec /bin/sleep 30\n",
			cancel: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"gst-inspect-1.0": "echo 'x264: x264enc: H.264 encoder'\necho 'videoparsersbad: h264parse: H.264 parser'\n",
				"gst-launch-1.0":  tc.script,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc := NewVideoService(ctx, zap.NewNop(), nil)
			broadcast := func([]byte, frameTimestamp, agentpb.VideoCodec) bool {
				if tc.cancel {
					cancel()
					return false
				}
				return true
			}
			err := svc.streamGStreamer(ctx, broadcast, "/dev/video0",
				&agentpb.StreamVideoRequest{Width: 640, Height: 480}, camera.TransportUSB, "", pipeWireSource{}, nil)
			if tc.cancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled viewer returned %v", err)
				}
				return
			}
			if status.Code(err) != tc.code {
				t.Fatalf("status = %v, want %s", err, tc.code)
			}
			if isCameraInUse(err) != tc.busy {
				t.Fatalf("camera contention = %v, want %v", err, tc.busy)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestCameraPipelineErrorPreservesTimeoutDiagnostic(t *testing.T) {
	err := gstCameraPipelineError("/dev/video0", "nvv4l2h264enc",
		"ERROR: nvarguscamerasrc: Failed to create CaptureSession", nil,
		gstStreamEnd(nil, true, false, nil))
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("timeout status changed: %v", err)
	}
	for _, want := range []string{"camera produced no video", "/dev/video0", "nvv4l2h264enc", "Failed to create CaptureSession"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestCameraPipelineErrorBoundsAndSanitizesDiagnostic(t *testing.T) {
	waitErr := exec.Command("false").Run()
	stderr := "ERROR: rtsp://admin:secret@camera/live\n\t\x1b[31m\x00\u202e" + strings.Repeat("€", maxCameraErrorDetail)
	err := gstCameraPipelineError("/dev/video0", "x264enc", stderr, waitErr, nil)
	msg := status.Convert(err).Message()
	if strings.Contains(msg, "admin") || strings.Contains(msg, "secret") {
		t.Fatalf("credentials in diagnostic: %s", msg)
	}
	if strings.IndexFunc(msg, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		t.Fatalf("control characters in diagnostic: %q", msg)
	}
	if !utf8.ValidString(msg) || len(msg) > maxCameraErrorDetail+256 || !strings.Contains(msg, "[truncated]") {
		t.Fatalf("invalid or unbounded diagnostic: %q", msg)
	}
}

func TestLimitedBufferDrainsBeyondLimit(t *testing.T) {
	b := &limitedBuffer{limit: 8}
	input := bytes.Repeat([]byte("diagnostic"), 1024)
	for range 2 {
		n, err := io.Copy(b, bytes.NewBuffer(input))
		if err != nil || n != int64(len(input)) {
			t.Fatalf("stderr copier stopped after %d bytes: %v", n, err)
		}
	}
	if b.buf.Len() != b.limit {
		t.Fatalf("buffer retained %d bytes, want %d", b.buf.Len(), b.limit)
	}
}
