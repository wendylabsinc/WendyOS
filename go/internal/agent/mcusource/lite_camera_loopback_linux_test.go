//go:build linux

package mcusource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"go.uber.org/zap"
)

func TestLiteCameraDeliversDecodedPixelsThroughKernelLoopback(t *testing.T) {
	if os.Getenv("WENDY_LITE_CAMERA_LOOPBACK") != "1" {
		t.Skip("requires the disposable Linux media fixture")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Geteuid() != 0 {
		t.Fatal("kernel camera fixture is restricted to the disposable hosted CI runner")
	}
	path := fmt.Sprintf("/dev/video%d", ipcam.MCUBandStart)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture requires absent %s: %v", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fixture := newLiteCameraFixture(t, ctx)
	directory := t.TempDir()
	loopback := ipcam.NewLoopback(ctx, zap.NewNop(), ipcam.NewRegistry(filepath.Join(directory, "cameras.json")),
		ipcam.NewCredentialStore(filepath.Join(directory, "credentials.json")),
		func(context.Context, []string) error { return errors.New("unexpected IP camera pump in Lite fixture") })
	defer loopback.Shutdown()
	if err := loopback.Available(); err != nil {
		t.Fatal(err)
	}
	supervisor := NewSupervisor(zap.NewNop(), loopback, fixture.transport, ros2camera.NewFrameWriter, nil)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := supervisor.streamOnce(runCtx, fixture.pairing, fixture.address)
		done <- err
	}()
	completed := false
	t.Cleanup(func() {
		stop()
		if !completed {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("Lite source did not release its kernel writer")
			}
		}
		supervisor.releasePairing(fixture.pairing)
	})
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for !supervisor.IsPairingConnected(fixture.pairing) {
		select {
		case err := <-done:
			completed = true
			t.Fatalf("Lite stream ended before kernel camera became live: %v", err)
		case <-deadline.C:
			t.Fatal("Lite source did not deliver to the kernel camera")
		case <-time.After(20 * time.Millisecond):
		}
	}
	wantIdentity := "sensorlink:" + fixture.pairing.Key() + ":1"
	if supervisor.CameraIdentities()[ipcam.MCUBandStart] != wantIdentity {
		t.Fatalf("kernel camera identity = %v, want %s", supervisor.CameraIdentities(), wantIdentity)
	}
	for _, phase := range []int32{0, 1} {
		fixture.phase.Store(phase)
		decodeCtx, stopDecode := context.WithTimeout(ctx, 6*time.Second)
		command := exec.CommandContext(decodeCtx, "gst-launch-1.0", "-q", "v4l2src", "device="+path, "num-buffers=10", "!",
			"image/jpeg,width=64,height=48", "!", "jpegdec", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1")
		var diagnostics bytes.Buffer
		command.Stderr = &diagnostics
		pixels, err := command.Output()
		stopDecode()
		const frameBytes = 64 * 48 * 3
		if err != nil || len(pixels) != 10*frameBytes {
			t.Fatalf("V4L2 consumer did not decode ten frames: bytes=%d err=%v stderr=%s", len(pixels), err, diagnostics.String())
		}
		// Early buffers may predate the phase change; the final three must
		// contain the new pixels, keeping the right blue region unchanged.
		for offset := len(pixels) - 3*frameBytes; offset < len(pixels); offset += frameBytes {
			for _, point := range []struct{ x, channel int }{{16, int(phase)}, {48, 2}} {
				index := offset + (24*64+point.x)*3
				for channel, value := range pixels[index : index+3] {
					if (channel == point.channel && value < 200) || (channel != point.channel && value > 45) {
						t.Fatalf("kernel camera delivered stale/incorrect pixels for phase %d at x=%d: %v", phase, point.x, pixels[index:index+3])
					}
				}
			}
		}
		t.Logf("decoded ten 64x48 kernel camera frames, verified final three in phase %d", phase)
	}
	stop()
	select {
	case <-done:
		completed = true
	case <-time.After(3 * time.Second):
		t.Fatal("Lite camera mount did not stop after cancellation")
	}
	if supervisor.IsPairingConnected(fixture.pairing) || !fixture.verifiedConsumer.Load() || fixture.accepted.Load() != 1 {
		t.Fatal("mount liveness or single authenticated source connection was incorrect")
	}
	supervisor.releasePairing(fixture.pairing)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) || len(supervisor.CameraIdentities()) != 0 {
		t.Fatalf("Lite camera node/identity survived release: path=%v identities=%v", err, supervisor.CameraIdentities())
	}
}
