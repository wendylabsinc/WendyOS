//go:build linux

package services

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The whole launch as the root agent does it: the OOM-score wrapper, the
// credential drop and the stdout pipe together. The stand-in pipeline reports
// who it runs as in place of video.
func TestCameraPipelineRunsWithoutRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping root needs root")
	}
	// t.TempDir is searchable by root alone; the sandbox user must reach the program.
	dir, err := os.MkdirTemp("", "gst-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	program := filepath.Join(dir, "gst-test")
	script := "#!/bin/sh\necho uid=$(id -u)\ngrep NoNewPrivs /proc/self/status\necho oom=$(cat /proc/self/oom_score_adj)\n"
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	svc := &VideoService{logger: zap.NewNop(), camSandbox: newCameraSandbox(zap.NewNop(), "require")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var got bytes.Buffer
	err = svc.runCameraPipeline(ctx, func(b []byte, _ frameTimestamp, _ agentpb.VideoCodec) bool {
		got.Write(b)
		return true
	}, "/dev/video4", gstEncoderResult{element: "x264enc"}, gstPipelinePlan{args: []string{program}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("sandboxed pipeline failed: %v; output %q", err, got.String())
	}
	for _, want := range []string{`(?m)^uid=[1-9]\d*$`, `(?m)^NoNewPrivs:\s+1$`, `(?m)^oom=\d+$`} {
		if !regexp.MustCompile(want).Match(got.Bytes()) {
			t.Errorf("pipeline output %q does not match %s", got.String(), want)
		}
	}
}
