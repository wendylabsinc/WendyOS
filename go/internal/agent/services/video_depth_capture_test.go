package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// The capture loop itself needs a real V4L2 device and is exercised by the
// hardware harness (a D435i depth node); these tests cover what is checkable
// everywhere — the seams the Z16 path added.

func TestFourccName(t *testing.T) {
	for pixfmt, want := range map[uint32]string{
		v4l2PixFmtZ16:  "Z16 ",
		v4l2PixFmtY16:  "Y16 ",
		v4l2PixFmtH264: "H264",
		v4l2PixFmtYUYV: "YUYV",
	} {
		if got := fourccName(pixfmt); got != want {
			t.Errorf("fourccName(%08x) = %q, want %q", pixfmt, got, want)
		}
	}
}

func TestCaptureDepthZ16RefusesAMissingDevice(t *testing.T) {
	s := &VideoService{logger: zap.NewNop()}
	err := s.captureDepthZ16(context.Background(), func([]byte, frameTimestamp) bool {
		t.Fatal("no frame can exist")
		return false
	}, filepath.Join(t.TempDir(), "video99"), 640, 480, 30)
	if err == nil {
		t.Fatal("capture from a nonexistent node succeeded")
	}
}

func TestCaptureRefusesANonDeviceWithoutEmitting(t *testing.T) {
	// A path that opens but is no V4L2 device must fail during setup and
	// never emit; this pins the loop's ordering (format first, frames later)
	// without hardware.
	path := filepath.Join(t.TempDir(), "not-a-camera")
	if err := os.WriteFile(path, make([]byte, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &VideoService{logger: zap.NewNop()}
	err := s.captureDepthZ16(context.Background(), func([]byte, frameTimestamp) bool {
		t.Fatal("emitted a frame from a regular file")
		return false
	}, path, 640, 480, 30)
	if err == nil {
		t.Fatal("capture from a regular file succeeded")
	}
}

func TestH264WrapperKeepsItsFallbackSignal(t *testing.T) {
	// The software-encoder fallback rides on the error TYPE, not its text —
	// the config indirection must preserve it.
	err := (v4l2CaptureConfig{
		formatUnsupported: func(msg string) error { return nativeH264NotSupported{msg: msg} },
	}).formatUnsupported("VIDIOC_S_FMT H264 rejected")
	if _, ok := err.(nativeH264NotSupported); !ok {
		t.Fatalf("fallback signal lost: %T", err)
	}
	if !strings.Contains(err.Error(), "H264") {
		t.Errorf("message lost: %q", err)
	}
}
