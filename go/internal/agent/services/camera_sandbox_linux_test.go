//go:build linux

package services

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The whole launch as the root agent does it: the OOM-score wrapper, the
// credential drop, the shared registry and the stdout pipe together. The
// stand-in pipeline reports what it runs as in place of video.
func TestCameraPipelineRunsWithoutRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping root needs root")
	}
	// t.TempDir is searchable by root alone; the sandbox user must reach the programs.
	dir, err := os.MkdirTemp("", "gst-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "gst-test")
	for path, script := range map[string]string{
		// GStreamer writes its registry readable by its owner alone.
		filepath.Join(dir, "gst-inspect-1.0"): `umask 077; echo plugins > "$GST_REGISTRY"`,
		program: `PATH=/usr/bin:/bin
echo uid=$(id -u) gid=$(id -g)
grep NoNewPrivs /proc/self/status
echo oom=$(cat /proc/self/oom_score_adj)
echo home=$HOME gsettings=$GSETTINGS_BACKEND update=$GST_REGISTRY_UPDATE registry=$(cat "$GST_REGISTRY")
test -w "$GST_REGISTRY" || test -w "$(dirname "$GST_REGISTRY")" || echo registry is read-only`,
	} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	origRegistry := gstRegistry
	t.Cleanup(func() { gstRegistry = origRegistry })
	gstRegistry = filepath.Join(dir, "registry", "registry.bin")

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
	for _, want := range []string{
		`(?m)^uid=(60\d{3}|61\d{3}) gid=(60\d{3}|61\d{3})$`,
		`(?m)^NoNewPrivs:\s+1$`,
		`(?m)^oom=\d+$`,
		`(?m)^home=/ gsettings=memory update=no registry=plugins$`,
		`(?m)^registry is read-only$`,
	} {
		if !regexp.MustCompile(want).Match(got.Bytes()) {
			t.Errorf("pipeline output %q does not match %s", got.String(), want)
		}
	}
	t.Logf("stand-in pipeline reported:\n%s", got.String())
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// The helper is handed a camera password on stdin. By the time it reads it, no
// other process may read the helper's memory, whatever uid that process has.
func TestIPCameraHelperHidesItsMemoryBeforeReadingThePipeline(t *testing.T) {
	t.Cleanup(func() { unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0) }) //nolint:errcheck
	dumpable := -1
	err := RunIPCameraGStreamerHelper(readerFunc(func([]byte) (int, error) {
		dumpable, _ = unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		return 0, io.EOF
	}), io.Discard)
	if err == nil {
		t.Fatal("helper accepted an empty pipeline")
	}
	if dumpable != 0 {
		t.Fatalf("dumpable = %d when the pipeline was read; want 0", dumpable)
	}
}
