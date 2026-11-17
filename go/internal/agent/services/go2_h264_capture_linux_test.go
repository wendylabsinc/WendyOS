//go:build linux && regression_go2_h264

package services

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

// Executed both on the hosted parent and inside its camera-entitled OCI task.
// The decoder/writer fixture is separate; this exercises the production native
// V4L2 service capture with its real mmap buffers and UAPI layout.
func TestGo2NativeH264CaptureChild(t *testing.T) {
	if os.Getenv("WENDY_GO2_CAPTURE_CHILD") != "1" {
		t.Skip("requires the owned Go2 kernel fixture")
	}
	frames, rate, bound := 6, 10, 8*time.Second
	if os.Getenv("WENDY_GO2_CAPTURE_RECORDED") == "1" {
		frames, rate, bound = 120, 30, 20*time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	svc := &VideoService{logger: zap.NewNop()}
	var encoded []byte
	count := 0
	err := svc.streamV4L2Native(ctx, func(data []byte, _ frameTimestamp, codec agentpb.VideoCodec) bool {
		if codec != agentpb.VideoCodec_VIDEO_CODEC_H264 {
			t.Error("native capture lost H264 codec")
			return false
		}
		if len(data) == 0 || len(data) > 1<<20 {
			t.Errorf("invalid native access unit size %d", len(data))
			return false
		}
		if len(encoded)+len(data) > 16<<20 {
			t.Error("capture exceeds 16 MiB")
			return false
		}
		encoded = append(encoded, data...)
		count++
		return count < frames
	}, "/dev/video128", &agentpb.StreamVideoRequest{Width: 640, Height: 360, Framerate: uint32(rate)})
	if err != nil || count != frames {
		t.Fatalf("Go2 native service capture failed: frames=%d err=%v", count, err)
	}
	fmt.Printf("GO2_CAPTURE_BYTES %s\n", base64.StdEncoding.EncodeToString(encoded))
}
