package ros2camera

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/testfixture/go2video"
)

// A generated moving GOP validates the replay machinery, not physical firmware.
func TestRecordedGo2ReplayDecodesInterFrames(t *testing.T) {
	if os.Getenv("WENDY_GO2_RECORDED_DECODE_FIXTURE") != "1" {
		t.Skip("requires explicit GStreamer replay fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	corpus := filepath.Join(root, "corpus")
	if err := os.Mkdir(corpus, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", "-q", "videotestsrc", "num-buffers=90", "pattern=ball", "!", "video/x-raw,format=I420,width=640,height=360,framerate=30/1", "!", "x264enc", "tune=zerolatency", "speed-preset=ultrafast", "key-int-max=15", "bframes=0", "byte-stream=true", "!", "h264parse", "config-interval=-1", "!", "video/x-h264,stream-format=byte-stream,alignment=au", "!", "multifilesink", "next-file=buffer", "location="+filepath.Join(root, "unit-%04d.h264"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate moving GOP: %v %s", err, out)
	}
	var samples []map[string]any
	var parsed bytes.Buffer
	for index := 0; index < 90; index++ {
		unit, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("unit-%04d.h264", index)))
		if err != nil {
			t.Fatal(err)
		}
		payload := go2video.Payload(unit, uint64(index))
		name := fmt.Sprintf("%04d.cdr", index)
		if err := os.WriteFile(filepath.Join(corpus, name), payload, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		samples = append(samples, map[string]any{"file": name, "bytes": len(payload), "sha256": hex.EncodeToString(digest[:]), "received_monotonic_ns": 1 + int64(index)*int64(time.Second/30)})
		frame, err := DecodeFrame(TypeGo2FrontVideo, payload)
		if err != nil || frame.Codec != CodecH264 || frame.Width != 640 || frame.Height != 360 || !bytes.Equal(frame.Data, unit) {
			t.Fatalf("recorded CDR replay changed access unit: %v", err)
		}
		parsed.Write(frame.Data)
	}
	manifest, _ := json.Marshal(map[string]any{"schema_version": 1, "samples": samples})
	if err := os.WriteFile(filepath.Join(root, "replay.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	recording, err := go2video.LoadRecording(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := go2video.DecodeFrameHashes(ctx, parsed.Bytes(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 90 || len(recording.Reference) != 90 {
		t.Fatalf("missing inter-frames: got %d reference %d", len(hashes), len(recording.Reference))
	}
	for i := range hashes {
		if hashes[i] != recording.Reference[i] {
			t.Fatalf("independent decoded source differs at frame%d", i)
		}
	}
	if err := go2video.MatchRecording(recording.Reference, hashes[20:50]); err != nil {
		t.Fatal(err)
	}
	hashes[24] = sha256.Sum256([]byte("unrelated decoded frame"))
	if err := go2video.MatchRecording(recording.Reference, hashes[20:50]); err == nil {
		t.Fatal("pixel corruption was accepted")
	}
	t.Log("GO2_GENERATED_GOP_REPLAY_VERIFIED:90 decoded frames including inter-frames; no physical recording claim")
}
