package services

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Exercise the encoder's actual output, not only the planned caps. The producer
// has its own plugin directory and registry with no h264parse. The consumer has
// the normal decoder plugins, as a viewer on a separate machine would.
func TestGStreamerX264WithoutParserDecodesForInitialAndLateViewers(t *testing.T) {
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("GStreamer is not installed")
	}
	inspect, err := exec.LookPath("gst-inspect-1.0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	viewerEnv := os.Environ()
	plugins := t.TempDir()
	// Include a viable VP8 alternative: reverting encoder selection must choose
	// that alternative and fail, rather than reach x264 as the final fallback.
	for _, element := range []string{"fdsrc", "fdsink", "queue", "capsfilter", "typefind", "jpegparse", "jpegdec", "videoconvert", "videoscale", "x264enc", "vp8enc", "webmmux"} {
		cmd := exec.CommandContext(ctx, inspect, element)
		cmd.Env = append(viewerEnv, "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		if err != nil {
			if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
				t.Fatalf("required GStreamer element %s: %v: %s", element, err, out)
			}
			t.Skipf("GStreamer element %s unavailable: %v", element, err)
		}
		var filename string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Filename ") {
				filename = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Filename"))
				break
			}
		}
		if !filepath.IsAbs(filename) {
			t.Fatalf("no plugin filename for %s", element)
		}
		dest := filepath.Join(plugins, filepath.Base(filename))
		if err := os.Symlink(filename, dest); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}
	for _, element := range []string{"h264parse", "avdec_h264"} {
		cmd := exec.CommandContext(ctx, inspect, element)
		if out, err := cmd.CombinedOutput(); err != nil {
			if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
				t.Fatalf("required viewer decoder %s: %v: %s", element, err, out)
			}
			t.Skipf("viewer decoder %s unavailable", element)
		}
	}
	t.Setenv("GST_PLUGIN_PATH", plugins)
	t.Setenv("GST_PLUGIN_PATH_1_0", plugins)
	t.Setenv("GST_PLUGIN_SYSTEM_PATH", "")
	t.Setenv("GST_PLUGIN_SYSTEM_PATH_1_0", "")
	t.Setenv("GST_REGISTRY", filepath.Join(t.TempDir(), "producer-registry.bin"))
	t.Setenv("GST_REGISTRY_1_0", os.Getenv("GST_REGISTRY"))
	available, err := listGSTElements(inspect)
	if err != nil {
		t.Fatal(err)
	}
	if available["h264parse"] || !available["x264enc"] || !available["vp8enc"] || !available["webmmux"] {
		t.Fatalf("producer registry does not reproduce the missing-parser host: %v", available)
	}
	enc, err := findGStreamerEncoder(inspect)
	if err != nil || enc.element != "x264enc" || enc.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || enc.hasH264Parse {
		t.Fatalf("missing-parser host did not select H.264: encoder=%+v err=%v", enc, err)
	}
	plan, err := planSharedJPEGPipeline(launch, "", &agentpb.StreamVideoRequest{}, enc, sharedCameraJPEG{width: 64, height: 48})
	if err != nil {
		t.Fatal(err)
	}
	jpegFrame := redGreenJPEG(t, 64, 48)
	input, writer := io.Pipe()
	defer input.Close()
	written := make(chan error, 1)
	go func() {
		defer writer.Close()
		for range 40 {
			if _, err := writer.Write(jpegFrame); err != nil {
				written <- err
				return
			}
			// The production queue deliberately sheds old frames. Pace this
			// synthetic source so more than one keyframe reaches the viewer.
			select {
			case <-ctx.Done():
				written <- ctx.Err()
				return
			case <-time.After(30 * time.Millisecond):
			}
		}
		written <- nil
	}()
	producer := exec.CommandContext(ctx, plan.args[0], plan.args[1:]...)
	producer.Stdin = input
	var stderr bytes.Buffer
	producer.Stderr = &stderr
	encoded, err := producer.Output()
	_ = input.Close()
	writeErr := <-written
	if err != nil || writeErr != nil {
		t.Fatalf("missing-parser pipeline failed: process=%v writer=%v stderr=%s", err, writeErr, stderr.String())
	}
	// Locate access units using the byte stream itself. A late viewer starts
	// at a later IDR's AUD, with no initial SPS/PPS supplied out of band.
	starts := annexBKeyframeStarts(encoded)
	if len(starts) < 3 {
		t.Fatalf("missing-parser output is not independently decodable Annex B: got %d keyframes with SPS/PPS", len(starts))
	}
	for name, stream := range map[string][]byte{"initial": encoded, "late": encoded[starts[2]:]} {
		t.Run(name, func(t *testing.T) {
			pixels, stderr, err := decodeH264ToRGB(ctx, launch, viewerEnv, stream)
			const frameBytes = 64 * 48 * 3
			if err != nil || len(pixels) < frameBytes || len(pixels)%frameBytes != 0 {
				t.Fatalf("%s viewer could not decode complete frames: bytes=%d err=%v stderr=%s", name, len(pixels), err, stderr)
			}
			if err := redGreenMismatch(pixels, 64, 48); err != nil {
				t.Fatalf("%s viewer %v", name, err)
			}
			t.Logf("decoded %d complete 64x48 RGB frames with expected pixels", len(pixels)/frameBytes)
		})
	}
}

// redGreenJPEG encodes a width x height picture whose left half is red and right
// half green. It is generated in memory, so no image assets are committed, and
// the two halves let a decoder's output prove pixel content as well as size.
func redGreenJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			value := color.RGBA{R: 240, G: 10, B: 10, A: 255}
			if x >= width/2 {
				value = color.RGBA{R: 10, G: 240, B: 10, A: 255}
			}
			frame.SetRGBA(x, y, value)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// decodeH264ToRGB decodes an Annex B stream as a viewer would and returns its
// pictures as packed RGB, with what the decoder wrote on stderr. A nil env runs
// the decoder in this process's environment.
func decodeH264ToRGB(ctx context.Context, launch string, env []string, stream []byte) ([]byte, string, error) {
	decode := exec.CommandContext(ctx, launch, "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1")
	decode.Env = env
	decode.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	decode.Stderr = &stderr
	pixels, err := decode.Output()
	return pixels, stderr.String(), err
}

// redGreenMismatch checks every decoded RGB frame of a redGreenJPEG picture in
// the middle of each half, and names the first point that lost its colour.
func redGreenMismatch(pixels []byte, width, height int) error {
	frameBytes := width * height * 3
	for offset := 0; offset+frameBytes <= len(pixels); offset += frameBytes {
		for _, point := range []struct{ x, y, channel int }{{width / 4, height / 2, 0}, {3 * width / 4, height / 2, 1}} {
			index := offset + (point.y*width+point.x)*3
			for channel, value := range pixels[index : index+3] {
				if (channel == point.channel && value < 200) || (channel != point.channel && value > 45) {
					return fmt.Errorf("frame %d lost fixture colors at %d,%d: %v", offset/frameBytes, point.x, point.y, pixels[index:index+3])
				}
			}
		}
	}
	return nil
}

// Return only independently decodable IDR access units, each with its own AUD,
// SPS and PPS. A parserless AVC stream has length prefixes and returns no starts.
func annexBKeyframeStarts(data []byte) []int {
	var starts []int
	aud, sps, pps := -1, false, false
	for i := 0; i+4 < len(data); {
		prefix := 0
		if bytes.HasPrefix(data[i:], []byte{0, 0, 0, 1}) {
			prefix = 4
		} else if bytes.HasPrefix(data[i:], []byte{0, 0, 1}) {
			prefix = 3
		}
		if prefix == 0 {
			i++
			continue
		}
		switch data[i+prefix] & 0x1f {
		case 9:
			aud, sps, pps = i, false, false
		case 7:
			sps = true
		case 8:
			pps = true
		case 5:
			if aud >= 0 && sps && pps {
				starts = append(starts, aud)
				// Multiple IDR slices belong to the same access unit.
				aud = -1
			}
		}
		i += prefix + 1
	}
	return starts
}
