//go:build linux && regression_camera_fixture

package services

import (
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
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// The RTSP/RTP camera is synthetic. Loopback allocation, supervision, clocks,
// GStreamer decode/v4l2sink, and independent V4L2 consumers are production paths.
// Container demand enters through the real provider API; this does not claim
// an OCI containerd mount or a physical camera firmware acceptance.
func TestIPCameraKernelPumpRetainsConsumersAndStopsAfterFinalRelease(t *testing.T) {
	testIPCameraKernelLifecycle(t, "")
}

// These isolated observations run only after the mandatory lifecycle gate fails.
// They never turn that failure into success or change production pipeline args.
func TestIPCameraKernelDeliveryDiagnostic(t *testing.T) {
	mode := os.Getenv("WENDY_IPCAM_DELIVERY_DIAGNOSTIC")
	if mode != "consumer-unsynced" && mode != "producer-write" {
		t.Skip("requires an explicit diagnostic mode from the hosted fixture")
	}
	testIPCameraKernelLifecycle(t, mode)
}

func testIPCameraKernelLifecycle(t *testing.T, diagnosticMode string) {
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
	bound := 80 * time.Second
	if diagnosticMode != "" {
		bound = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
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
		if diagnosticMode == "producer-write" {
			args = append(args, "io-mode=rw")
		}
		err := runIPCameraGStreamerPipeline(pumpCtx, args, func([]byte) error { return errors.New("kernel pump unexpectedly emitted stdout bytes") })
		t.Logf("IP kernel producer exit: diagnostic=%q context=%v error=%v RTP_access_units=%d", diagnosticMode, pumpCtx.Err(), err, source.rtpAccessUnits.Load())
		return err
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
			// With exclusive_caps the format appears before the producer starts
			// streaming. GStreamer rejects an output-only node immediately, so
			// wait for the kernel's actual capture capability as well.
			var caps v4l2Capability
			fd, capabilityErr := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if capabilityErr == nil {
				_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQueryCap, uintptr(unsafe.Pointer(&caps)))
				unix.Close(fd)
				if errno != 0 {
					capabilityErr = errno
				}
			}
			if err == nil && strings.HasSuffix(strings.TrimSpace(string(format)), ":64x48@25") && capabilityErr == nil && caps.hasVideoCapture() {
				break
			}
			if time.Now().After(formatDeadline) {
				t.Fatalf("kernel pump did not become a 64x48 capture device at 25fps: format=%q error=%v capabilities=%#x device_caps=%#x query_error=%v", format, err, caps.Capabilities, caps.DeviceCaps, capabilityErr)
			}
			wait(20 * time.Millisecond)
		}
		captureCtx, stop := context.WithTimeout(ctx, 6*time.Second)
		defer stop()
		args := []string{"-q", "v4l2src", "device=" + path, "num-buffers=10", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1"}
		if diagnosticMode == "consumer-unsynced" {
			args = append(args, "sync=false")
		}
		cmd := exec.CommandContext(captureCtx, launch, args...)
		// Keep consumer diagnostics separate from the producer's GST_DEBUG_FILE.
		// GStreamer initializes independently in this child and otherwise opens
		// the same file with truncation. Drain stderr through a bounded writer.
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "GST_DEBUG_FILE=") {
				cmd.Env = append(cmd.Env, variable)
			}
		}
		diagnostic := &limitedBuffer{limit: 64 * 1024}
		cmd.Stderr = diagnostic
		rtpBefore := source.rtpAccessUnits.Load()
		pixels, err := cmd.Output()
		const frameBytes = 64 * 48 * 3
		if err != nil || len(pixels) != 10*frameBytes {
			t.Fatalf("IP kernel consumer did not decode ten frames: diagnostic=%q bytes=%d err=%v producer_starts=%d active=%d exits=%d RTP_access_units=%d->%d stderr=%s", diagnosticMode, len(pixels), err, starts.Load(), active.Load(), exits.Load(), rtpBefore, source.rtpAccessUnits.Load(), diagnostic.buf.String())
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
	if diagnosticMode != "" {
		capture(1)
		assertOne("diagnostic changing pixels")
		t.Logf("IP kernel delivery diagnostic %q decoded ten frames in each fresh phase; the mandatory normal gate remains failed", diagnosticMode)
		return
	}
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
