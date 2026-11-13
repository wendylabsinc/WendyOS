package ros2camera

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/testfixture/go2video"
)

// Generated access units exercise the observed CDR format and a real independent
// decoder. This is intentionally not labelled as a physical firmware recording.
func TestGo2ResolutionH264DecodesGeneratedPixels(t *testing.T) {
	if os.Getenv("WENDY_GO2_DECODE_FIXTURE") != "1" {
		t.Skip("requires GStreamer H264 fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	units, err := go2video.Generate(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for phase, unit := range units {
		frame, err := DecodeFrame(TypeGo2FrontVideo, go2video.Payload(unit, uint64(phase+1)))
		if err != nil {
			t.Fatalf("Go2 resolution H264 decode failed: %v", err)
		}
		if frame.Codec != CodecH264 || frame.Width != 640 || frame.Height != 360 || !bytes.Equal(frame.Data, unit) {
			t.Fatalf("Go2 CDR changed codec, dimensions or access unit: %+v", frame)
		}
		if err := go2video.Decode(ctx, frame.Data, phase, 1); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("GO2_GENERATED_H264_DECODE_VERIFIED: actual red/green pixels; no physical recording claim")
}
