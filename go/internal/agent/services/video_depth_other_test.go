//go:build !linux

package services

import (
	"context"
	"strings"
	"testing"
)

// TestNativeDepthCaptureIsLinuxOnly keeps the refusal on platforms without V4L2
// a clear sentence rather than a failed ioctl.
func TestNativeDepthCaptureIsLinuxOnly(t *testing.T) {
	err := captureDepthFramesNative(context.Background(), nil, testDepthNode, 640, 480,
		func(uint32) { t.Fatal("offered called on a platform without V4L2") },
		func([]byte) bool { t.Fatal("deliver called on a platform without V4L2"); return false })
	if err == nil || !strings.Contains(err.Error(), "native depth capture is Linux only") {
		t.Fatalf("captureDepthFramesNative = %v, want the Linux-only refusal", err)
	}
}
