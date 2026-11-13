//go:build linux && regression_go2_h264

package containerd

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtime "github.com/containerd/containerd/v2/client"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/testfixture/go2video"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

type go2CameraDemand struct {
	loop  *ipcam.Loopback
	label string
	mu    sync.Mutex
	names []string
}

func (p *go2CameraDemand) EnsureCameraNodes(ctx context.Context) error {
	return p.loop.EnsureNode(ctx, 128, p.label)
}
func (p *go2CameraDemand) SetCameraContainerConsumers(_ context.Context, names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names = slices.Clone(names)
}
func (p *go2CameraDemand) SetTwoPlaneContainerConsumers(context.Context, []string) {}
func (p *go2CameraDemand) active(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.names, name)
}

// Real kernel and OCI delivery, with generated video in the observed firmware
// CDR layout. The publisher and provider recorder are test seams; this does not
// claim physical Go2 firmware, DDS discovery or Manager scheduling coverage.
func TestGo2H264KernelServiceAndContainerDelivery(t *testing.T) {
	if os.Getenv("WENDY_GO2_H264_KERNEL") != "1" {
		t.Skip("requires the disposable hosted media fixture")
	}
	if err := go2video.KernelAdmission(); err != nil {
		t.Fatal(err)
	}
	label := os.Getenv("WENDY_GO2_CAMERA_LABEL")
	if !strings.HasPrefix(label, "ci-go2-") || len(label) != 19 {
		t.Fatal("missing unique owned camera label")
	}
	const path = "/dev/video128"
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("requires absent owned node: %v", err)
	}
	version, err := os.ReadFile("/sys/module/v4l2loopback/version")
	if err != nil || strings.TrimSpace(string(version)) != "0.15.4" {
		t.Fatal("requires owned loopback0.15.4")
	}
	child := os.Getenv("WENDY_GO2_CAPTURE_BINARY")
	if !filepath.IsAbs(child) {
		t.Fatal("requires absolute capture binary")
	}
	c, base := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(base, 120*time.Second)
	defer cancel()
	createReplaceTestImage(t, c, ctx)
	directory := t.TempDir()
	var units [2][]byte
	var recording *go2video.Recording
	if archive := os.Getenv("WENDY_GO2_RECORDING"); archive != "" {
		recording, err = go2video.LoadRecording(ctx, archive)
	} else {
		units, err = go2video.Generate(ctx, directory)
	}
	if err != nil {
		t.Fatal(err)
	}
	loop := ipcam.NewLoopback(ctx, zap.NewNop(), ipcam.NewRegistry(filepath.Join(directory, "cameras.json")), ipcam.NewCredentialStore(filepath.Join(directory, "credentials.json")), func(context.Context, []string) error { return errors.New("unexpected IP pump") })
	defer loop.Shutdown()
	provider := &go2CameraDemand{loop: loop, label: label}
	c.SetCameraLoopbackProvider(provider)
	if err := provider.EnsureCameraNodes(ctx); err != nil {
		t.Fatal(err)
	}
	ownedNode, err := os.Lstat(path)
	if err != nil || ownedNode.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("owned camera node missing: %v", err)
	}
	writer := ros2camera.NewFrameWriter(path)
	var phase atomic.Int32
	written := make(chan int32, 8)
	done := make(chan error, 1)
	pumpCtx, stopPump := context.WithCancel(ctx)
	go func() {
		defer writer.Close()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for sequence := uint64(1); ; sequence++ {
			select {
			case <-pumpCtx.Done():
				done <- nil
				return
			case <-ticker.C:
			}
			current := phase.Load()
			payload := go2video.Payload(units[current], sequence)
			if recording != nil {
				index := int(sequence-1) % len(recording.Payloads)
				payload = recording.Payloads[index]
				ticker.Reset(recording.Intervals[index])
			}
			frame, err := ros2camera.DecodeFrame(ros2camera.TypeGo2FrontVideo, payload)
			if err != nil {
				done <- fmt.Errorf("Go2 resolution H264 decode failed: %w", err)
				return
			}
			if frame.Codec != ros2camera.CodecH264 || frame.Width != 640 || frame.Height != 360 {
				done <- errors.New("Go2 CDR lost H264 format")
				return
			}
			if err := writer.WriteFrame(frame); err != nil {
				done <- err
				return
			}
			select {
			case written <- current:
			default:
			}
		}
	}()
	drained := false
	defer func() {
		stopPump()
		if !drained {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
				drained = true
			case <-time.After(3 * time.Second):
				t.Error("Go2 writer did not drain; outer PID namespace must clean up")
			}
		}
		if drained {
			current, statErr := os.Lstat(path)
			name, nameErr := os.ReadFile("/sys/class/video4linux/video128/name")
			if statErr != nil || nameErr != nil || !os.SameFile(ownedNode, current) || strings.TrimSpace(string(name)) != label {
				t.Error("owned camera identity changed before cleanup; retaining node")
			} else {
				loop.RemoveCamera(128)
			}
		}
	}()
	waitPhase := func(want int32) {
		t.Helper()
		phase.Store(want)
		count := 0
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for count < 3 {
			select {
			case got := <-written:
				if got == want {
					count++
				}
			case err := <-done:
				drained = true
				t.Fatalf("Go2 production writer failed: %v", err)
			case <-timer.C:
				t.Fatal("Go2 production writer did not publish current phase")
			}
		}
	}
	decode := func(output []byte, want int) {
		t.Helper()
		var encoded []byte
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "GO2_CAPTURE_BYTES ") {
				if encoded != nil {
					t.Fatal("duplicate capture result")
				}
				var err error
				encoded, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "GO2_CAPTURE_BYTES "))
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(encoded) == 0 {
			t.Fatalf("native service returned no H264: %s", output)
		}
		decodeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if recording != nil {
			hashes, err := go2video.DecodeFrameHashes(decodeCtx, encoded, 180)
			if err != nil {
				t.Fatal(err)
			}
			if err := go2video.MatchRecording(recording.Reference, hashes); err != nil {
				t.Fatal(err)
			}
			t.Logf("GO2_RECORDED_CAPTURE_VERIFIED phase=%d frames=%d manifest=%s", want, len(hashes), recording.ManifestHash)
			evidence := make([]string, len(hashes))
			for i, hash := range hashes {
				evidence[i] = fmt.Sprintf("%x", hash)
			}
			t.Logf("GO2_RECORDED_CAPTURE_HASHES phase=%d sha256=%s", want, strings.Join(evidence, ","))
		} else if err := go2video.Decode(decodeCtx, encoded, want, 6); err != nil {
			t.Fatal(err)
		}
	}
	// Same production service capture runs first outside the container, then as
	// a test-only executable inside the actual camera-entitled task.
	waitPhase(0)
	format, err := os.ReadFile("/sys/class/video4linux/video128/format")
	if err != nil || !strings.Contains(string(format), "H264:640x360@") {
		t.Fatalf("Go2 writer kernel format is not H264 640x360: %q %v", format, err)
	}
	hostCtx, stopHost := context.WithTimeout(ctx, 24*time.Second)
	cmd := exec.CommandContext(hostCtx, child, "-test.run=^TestGo2NativeH264CaptureChild$", "-test.v", "-test.timeout=23s")
	cmd.Env = append(os.Environ(), "WENDY_GO2_CAPTURE_CHILD=1")
	captureMode := "WENDY_GO2_CAPTURE_RECORDED=0"
	if recording != nil {
		captureMode = "WENDY_GO2_CAPTURE_RECORDED=1"
	}
	cmd.Env = append(cmd.Env, captureMode)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	stopHost()
	if err != nil {
		t.Fatalf("Go2 host service capture: %v: %s", err, output)
	}
	decode(output, 0)
	const app = "go2-camera-fixture"
	cfg := &appconfig.AppConfig{AppID: app, Version: "1", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}}
	if err := c.CreateContainerWithProgress(ctx, &agentpb.CreateContainerRequest{AppName: app, ImageName: replaceTestImage}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	ctr, err := c.client.LoadContainer(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if task, err := ctr.Task(cleanupCtx, nil); err == nil {
			if _, err := task.Delete(cleanupCtx, runtime.WithProcessKill); err != nil {
				t.Error(err)
			}
		}
		if err := ctr.Delete(cleanupCtx, runtime.WithSnapshotCleanup); err != nil {
			t.Error(err)
		}
	}()
	spec, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Inject the executable and its host-matched ELF loader/libraries. CGO=0
	// does not imply static linking when imported purego packages use dlopen.
	// Camera rules remain those produced by the real camera entitlement.
	spec.Mounts = append(spec.Mounts, go2CaptureRuntimeMounts(t, ctx, child)...)
	spec.Mounts = append(spec.Mounts, specs.Mount{Source: child, Destination: "/capture.test", Type: "bind", Options: []string{"bind", "ro"}})
	if err := ctr.Update(ctx, runtime.UpdateContainerOpts(runtime.WithSpec(spec))); err != nil {
		t.Fatal(err)
	}
	stream, err := c.StartContainer(ctx, app, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamDone := make(chan struct{})
	go func() {
		for range stream {
		}
		close(streamDone)
	}()
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if err := c.StopContainer(cleanupCtx, app); err != nil {
			t.Error(err)
		}
		select {
		case <-streamDone:
		case <-cleanupCtx.Done():
			t.Error("Go2 container output did not drain")
		}
	}()
	c.SyncCameraLoopbacks(ctx)
	if !provider.active(app) {
		t.Fatal("Go2 camera entitlement did not retain active consumer")
	}
	info, err := ctr.Info(ctx)
	if err != nil || info.Labels["sh.wendy/entitlement.camera"] == "" {
		t.Fatal("Go2 parameterless camera label missing from actual runtime")
	}
	for _, current := range []int32{0, 1} {
		waitPhase(current)
		var stdout, stderr bytes.Buffer
		execCtx, stop := context.WithTimeout(ctx, 24*time.Second)
		code, err := c.ExecInContainer(execCtx, app, []string{"/bin/busybox", "env", "WENDY_GO2_CAPTURE_CHILD=1", captureMode, "/capture.test", "-test.run=^TestGo2NativeH264CaptureChild$", "-test.v", "-test.timeout=23s"}, false, nil, &stdout, &stderr, nil)
		stop()
		if err != nil || code != 0 {
			t.Fatalf("Go2 entitled container capture failed: exit=%d err=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
		}
		decode(stdout.Bytes(), int(current))
	}
	if recording != nil {
		t.Logf("GO2_RECORDED_KERNEL_CONTAINER_VERIFIED manifest=%s; archived source, physical identity remains operator reviewed", recording.ManifestHash)
		return
	}
	t.Log("GO2_KERNEL_CONTAINER_DECODE_VERIFIED: real native service, camera-entitled OCI delivery, fresh red/green decoded pixels; generated source")
}

// The fixture executable is built from this checkout, never an external input.
// Resolve only its loader and concrete ldd paths, without mounting host directories.
func go2CaptureRuntimeMounts(t *testing.T, ctx context.Context, binary string) []specs.Mount {
	t.Helper()
	file, err := elf.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	interpreter := ""
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			value, err := io.ReadAll(io.LimitReader(program.Open(), 4096))
			if err != nil {
				t.Fatal(err)
			}
			interpreter = strings.TrimRight(string(value), "\x00")
		}
	}
	if interpreter == "" {
		return nil
	}
	if !filepath.IsAbs(interpreter) {
		t.Fatalf("capture ELF loader is not absolute: %q", interpreter)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "ldd", binary)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(output), "not found") {
		t.Fatalf("resolve capture ELF runtime: %v: %s", err, output)
	}
	paths := []string{interpreter}
	for _, field := range strings.Fields(string(output)) {
		if filepath.IsAbs(field) && !slices.Contains(paths, field) {
			paths = append(paths, field)
		}
	}
	var mounts []specs.Mount
	for _, path := range paths {
		source, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(source)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("capture runtime dependency is not a regular file: %s: %v", path, err)
		}
		mounts = append(mounts, specs.Mount{Source: source, Destination: path, Type: "bind", Options: []string{"bind", "ro"}})
	}
	t.Logf("Go2 capture ELF runtime: %s", output)
	return mounts
}
