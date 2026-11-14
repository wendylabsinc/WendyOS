//go:build regression_camera_fixture

package services

import (
	"context"
	"image/color"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
)

// Validate the synthetic freshness source and production RTSP decoder on any
// GStreamer host. Kernel pump lifecycle is tested separately on hosted Linux.
func TestIPCameraAuthenticatedStreamChangesDecodedPixels(t *testing.T) {
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("GStreamer is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 27*time.Second)
	defer cancel()
	source := newCameraAdmissionServer(t, ctx, launch)
	alternate := encodeAdmissionAccessUnit(t, ctx, launch, color.RGBA{G: 240, A: 255})
	source.alternateAccessUnit.Store(&alternate)
	location := (&url.URL{Scheme: "rtsp", Host: source.rtsp.Addr().String(), Path: "/measurement", User: url.UserPassword("camera-user", "camera-password")}).String()
	args := ipcam.LoopbackPipelineArgs(location, "unused")
	sink := slices.Index(args, "v4l2sink")
	if sink < 0 {
		t.Fatal("production pump no longer declares its kernel sink")
	}
	// Keep production network/depay/decoder elements. Only the final kernel sink
	// is replaced for this portable protocol fixture's raw pixel observation.
	args = append(args[:sink], "video/x-raw,format=RGB,framerate=25/1", "!", "fdsink", "fd=1")
	frames := make(chan []byte, 2)
	done := make(chan error, 1)
	go func() {
		var pending []byte
		done <- runIPCameraGStreamerPipeline(ctx, args, func(chunk []byte) error {
			pending = append(pending, chunk...)
			for len(pending) >= 64*48*3 {
				frame := append([]byte(nil), pending[:64*48*3]...)
				pending = pending[64*48*3:]
				select {
				case frames <- frame:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}()
	completed := false
	t.Cleanup(func() {
		cancel()
		if !completed {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("portable RTSP decoder did not stop")
			}
		}
	})
	started := time.Now()
	for _, phase := range []int32{0, 1} {
		source.phase.Store(phase)
		for index := 0; index < 10; index++ {
			select {
			case frame := <-frames:
				if index >= 7 {
					assertAdmissionRGB(t, frame, phase)
				}
			case err := <-done:
				completed = true
				t.Fatalf("production RTSP decoder ended: %v", err)
			case <-ctx.Done():
				t.Fatal("production RTSP decoder did not deliver changing pixels")
			}
		}
	}
	// The kernel lifecycle keeps one authenticated stream alive across multiple
	// ten-second idle grace periods. A handshake deadline must not expire PLAY.
	for time.Since(started) < 22*time.Second {
		select {
		case frame := <-frames:
			assertAdmissionRGB(t, frame, 1)
		case err := <-done:
			completed = true
			t.Fatalf("authenticated RTSP fixture expired during active PLAY: %v", err)
		case <-ctx.Done():
			t.Fatal("authenticated RTSP fixture stopped producing live frames")
		}
	}
	cancel()
	select {
	case <-done:
		completed = true
	case <-time.After(3 * time.Second):
		t.Fatal("portable RTSP decoder did not stop")
	}
	if source.rtspPlays.Load() != 1 || source.authenticated.Load() != 1 {
		t.Fatal("fresh pixels did not share one authenticated RTSP session")
	}
}

func assertAdmissionRGB(t *testing.T, pixels []byte, phase int32) {
	t.Helper()
	if len(pixels) != 64*48*3 {
		t.Fatalf("incorrect RGB dimensions: %d bytes", len(pixels))
	}
	for _, point := range []struct{ x, channel int }{{16, int(phase)}, {48, 2}} {
		index := (24*64 + point.x) * 3
		for channel, value := range pixels[index : index+3] {
			if (channel == point.channel && value < 180) || (channel != point.channel && value > 65) {
				t.Fatalf("IP camera consumer received stale/wrong phase %d pixels: %v", phase, pixels[index:index+3])
			}
		}
	}
}
