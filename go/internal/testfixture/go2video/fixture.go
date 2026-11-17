// Package go2video supports generated and explicitly admitted recorded regression
// fixtures. Generated inputs never establish physical Go2 firmware acceptance.
package go2video

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const Width, Height = 640, 360

// Payload uses the little-endian timestamp/resolution/sequence CDR layout
// documented by the original physical observation. Its video is synthetic.
func Payload(accessUnit []byte, sequence uint64) []byte {
	b := []byte{0, 1, 0, 1}
	b = binary.LittleEndian.AppendUint64(b, sequence)
	b = binary.LittleEndian.AppendUint32(b, Height)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(accessUnit)))
	b = append(b, accessUnit...)
	for (len(b)-4)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func Generate(ctx context.Context, directory string) ([2][]byte, error) {
	var result [2][]byte
	for phase, argb := range []string{"4294901760", "4278255360"} {
		path := filepath.Join(directory, fmt.Sprintf("generated-%d.h264", phase))
		cmd := exec.CommandContext(ctx, "gst-launch-1.0", "-q", "videotestsrc", "num-buffers=1", "pattern=solid-color", "foreground-color="+argb, "!", "video/x-raw,format=I420,width=640,height=360,framerate=10/1", "!", "x264enc", "tune=zerolatency", "speed-preset=ultrafast", "key-int-max=1", "byte-stream=true", "!", "h264parse", "config-interval=-1", "!", "video/x-h264,stream-format=byte-stream,alignment=au", "!", "filesink", "location="+path)
		cmd.WaitDelay = time.Second
		output, err := cmd.CombinedOutput()
		if err != nil {
			return result, fmt.Errorf("generate H264: %w: %s", err, output)
		}
		result[phase], err = os.ReadFile(path)
		if err != nil {
			return result, err
		}
		if len(result[phase]) == 0 || len(result[phase]) > 1<<20 {
			return result, fmt.Errorf("generated H264 size %d", len(result[phase]))
		}
	}
	return result, nil
}

// Decode checks independently decoded pixels, not merely SPS/codec metadata.
func Decode(ctx context.Context, stream []byte, phase, count int) error {
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1")
	cmd.Stdin = bytes.NewReader(stream)
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	cmd.WaitDelay = time.Second
	pixels, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("decode H264: %w: %s", err, &diagnostic)
	}
	const size = Width * Height * 3
	if len(pixels) != count*size {
		return fmt.Errorf("decoded H264 length %d, expected %d frames of 640x360 RGB", len(pixels), count)
	}
	for frame := 0; frame < count; frame++ {
		for _, point := range [][2]int{{64, 36}, {320, 180}, {576, 324}} {
			i := frame*size + (point[1]*Width+point[0])*3
			for channel, value := range pixels[i : i+3] {
				if (channel == phase && value < 200) || (channel != phase && value > 45) {
					return fmt.Errorf("stale/wrong H264 pixels phase=%d frame=%d RGB=%v", phase, frame, pixels[i:i+3])
				}
			}
		}
	}
	return nil
}
