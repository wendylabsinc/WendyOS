//go:build linux && regression_camera_fixture

package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/color"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"go.uber.org/zap"
)

// The RTSP/RTP camera is synthetic. Loopback allocation, supervision, clocks,
// GStreamer decode/v4l2sink, and independent V4L2 consumers are production paths.
// Container demand enters through the real provider API; this does not claim
// an OCI containerd mount or a physical camera firmware acceptance.
func TestIPCameraKernelPumpRetainsConsumersAndStopsAfterFinalRelease(t *testing.T) {
	if os.Getenv("WENDY_IPCAM_LIFECYCLE_LOOPBACK") != "1" {
		t.Skip("requires the disposable Linux media fixture")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Geteuid() != 0 {
		t.Fatal("kernel IP camera fixture requires the disposable hosted runner")
	}
	const path = "/dev/video200"
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("requires absent fixture node %s: %v", path, err)
	}
	version, err := os.ReadFile("/sys/module/v4l2loopback/version")
	if err != nil || strings.TrimSpace(string(version)) != "0.15.4" {
		t.Fatalf("expected owned v4l2loopback 0.15.4: %q %v", version, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		t.Fatal(err)
	}
	source := newCameraAdmissionServer(t, ctx, launch)
	alternate := encodeAdmissionAccessUnit(t, ctx, launch, color.RGBA{G: 240, A: 255})
	source.alternateAccessUnit.Store(&alternate)
	directory := t.TempDir()
	registry := ipcam.NewRegistry(filepath.Join(directory, "cameras.json"))
	credentials := ipcam.NewCredentialStore(filepath.Join(directory, "credentials.json"))
	camera, err := registry.Upsert(ipcam.Camera{MAC: "ec:71:db:2a:ae:7e", Address: "127.0.0.1", StreamMain: "/measurement", StreamSub: "/measurement"})
	if err != nil {
		t.Fatal(err)
	}
	if camera.ID != 200 {
		t.Fatalf("unexpected isolated registry ID %d", camera.ID)
	}
	if err := credentials.Set(camera.MAC, ipcam.Credential{Username: "camera-user", Password: "camera-password"}); err != nil {
		t.Fatal(err)
	}
	var starts, active, exits atomic.Int32
	pump := func(pumpCtx context.Context, args []string) error {
		starts.Add(1)
		active.Add(1)
		defer active.Add(-1)
		defer exits.Add(1)
		// The real URL builder fixes camera RTSP to port 554. Substitute only the
		// local listener's ephemeral port; keep credentials, path and every element.
		args = append([]string(nil), args...)
		for index, arg := range args {
			if strings.HasPrefix(arg, "location=") {
				location, err := url.Parse(strings.TrimPrefix(arg, "location="))
				if err != nil {
					return err
				}
				location.Host = source.rtsp.Addr().String()
				args[index] = "location=" + location.String()
			}
		}
		return runIPCameraGStreamerPipeline(pumpCtx, args, func([]byte) error { return errors.New("kernel pump unexpectedly emitted stdout bytes") })
	}
	loopback := ipcam.NewLoopback(ctx, zap.NewNop(), registry, credentials, pump)
	t.Cleanup(func() { loopback.Shutdown(); loopback.RemoveCamera(camera.ID) })
	if err := loopback.EnsureNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if actual, ok := loopback.NodePath(camera.ID); !ok || actual != path {
		t.Fatalf("paired camera kernel node unavailable: %q %v", actual, ok)
	}
	if starts.Load() != 0 {
		t.Fatal("camera started before any demand")
	}
	wait := func(duration time.Duration) {
		t.Helper()
		select {
		case <-time.After(duration):
		case <-ctx.Done():
			t.Fatal("kernel lifecycle fixture exceeded its deadline")
		}
	}
	waitStarts := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for starts.Load() < want || source.rtspPlays.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("authenticated kernel pump did not start: starts=%d plays=%d", starts.Load(), source.rtspPlays.Load())
			}
			wait(20 * time.Millisecond)
		}
	}
	capture := func(phase int32) {
		t.Helper()
		source.phase.Store(phase)
		formatDeadline := time.Now().Add(4 * time.Second)
		for {
			format, err := os.ReadFile("/sys/class/video4linux/video200/format")
			if err == nil && strings.HasSuffix(strings.TrimSpace(string(format)), ":64x48@25") {
				break
			}
			if time.Now().After(formatDeadline) {
				t.Fatalf("kernel pump did not negotiate 64x48 capture format at 25fps: %q %v", format, err)
			}
			wait(20 * time.Millisecond)
		}
		captureCtx, stop := context.WithTimeout(ctx, 6*time.Second)
		defer stop()
		cmd := exec.CommandContext(captureCtx, launch, "-q", "v4l2src", "device="+path, "num-buffers=10", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1")
		var diagnostic bytes.Buffer
		cmd.Stderr = &diagnostic
		pixels, err := cmd.Output()
		const frameBytes = 64 * 48 * 3
		if err != nil || len(pixels) != 10*frameBytes {
			t.Fatalf("IP kernel consumer did not decode ten frames: bytes=%d err=%v stderr=%s", len(pixels), err, diagnostic.String())
		}
		for offset := len(pixels) - 3*frameBytes; offset < len(pixels); offset += frameBytes {
			assertAdmissionRGB(t, pixels[offset:offset+frameBytes], phase)
		}
	}
	assertOne := func(stage string) {
		t.Helper()
		if starts.Load() != 1 || exits.Load() != 0 || active.Load() != 1 || source.rtspPlays.Load() != 1 || source.authenticated.Load() != 1 {
			t.Fatalf("%s lost or duplicated the shared pump: starts=%d exits=%d active=%d plays=%d auth=%d", stage, starts.Load(), exits.Load(), active.Load(), source.rtspPlays.Load(), source.authenticated.Load())
		}
	}
	first := loopback.AcquireView(camera.ID)
	defer first()
	second := loopback.AcquireView(camera.ID)
	defer second()
	waitStarts(1)
	capture(0)
	assertOne("two viewers")
	first()
	first() // Releasing one view twice must not consume the other's ref.
	wait(11 * time.Second)
	assertOne("remaining viewer after idle-grace duration")
	capture(1)
	loopback.SetContainerConsumers([]string{"fixture-camera-app"})
	second()
	wait(11 * time.Second)
	assertOne("container-only demand after idle-grace duration")
	capture(0)
	loopback.SetContainerConsumers(nil)
	wait(2 * time.Second)
	third := loopback.AcquireView(camera.ID)
	defer third()
	wait(9 * time.Second)
	assertOne("reacquire cancelled idle stop")
	capture(1)
	releasedAt := time.Now()
	third()
	deadline := time.NewTimer(13 * time.Second)
	defer deadline.Stop()
	for active.Load() != 0 {
		select {
		case <-deadline.C:
			t.Fatal("last release did not stop the actual kernel pump")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if time.Since(releasedAt) < 9*time.Second || starts.Load() != 1 || exits.Load() != 1 {
		t.Fatalf("last release did not honor bounded idle grace: elapsed=%v starts=%d exits=%d", time.Since(releasedAt), starts.Load(), exits.Load())
	}
	fourth := loopback.AcquireView(camera.ID)
	defer fourth()
	waitStarts(2)
	capture(0)
	if active.Load() != 1 || source.authenticated.Load() != 2 {
		t.Fatal("new demand did not reacquire a fresh authenticated pump")
	}
	if err := credentials.Delete(camera.MAC); err != nil {
		t.Fatal(err)
	}
	if !registry.Forget(camera.ID) {
		t.Fatal("fixture camera disappeared from registry")
	}
	loopback.RemoveCamera(camera.ID)
	fourth()
	if active.Load() != 0 || exits.Load() != 2 {
		t.Fatal("forget did not stop and join the kernel writer")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forgotten IP camera node survived removal: %v", err)
	}
	t.Log(fmt.Sprintf("two authenticated RTSP sessions; preserved both viewer and container demand; final release joined writer and removed %s", path))
}
