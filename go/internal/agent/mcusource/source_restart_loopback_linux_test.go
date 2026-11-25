//go:build linux

package mcusource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"go.uber.org/zap"
)

// Run only inside the existing media job's owned v4l2loopback/snd-aloop gate.
// This proves actual Supervisor reconnect, authenticated streams, both kernel
// writers and decoded consumer samples. The source is synthetic; physical Mac
// capture-driver restart and mDNS advertisement remain separate acceptance.
func TestSourceRestartMovesBothKernelStreamsWithoutChangingIdentity(t *testing.T) {
	if os.Getenv("WENDY_SOURCE_RESTART_LOOPBACK") != "1" {
		t.Skip("requires the disposable Linux dual-media fixture")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Geteuid() != 0 {
		t.Fatal("dual-media fixture is restricted to the disposable hosted CI runner")
	}
	label := os.Getenv("WENDY_TEST_SOURCE_RESTART_LABEL")
	if len(label) < 8 || len(label) > 24 {
		t.Fatal("outer fixture must provide its unique camera ownership label (8..24 bytes)")
	}
	for _, module := range []string{"snd_aloop", "v4l2loopback"} {
		if _, err := os.Stat("/sys/module/" + module); err != nil {
			t.Fatalf("outer fixture did not load %s: %v", module, err)
		}
	}
	path := fmt.Sprintf("/dev/video%d", ipcam.MCUBandStart)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("requires absent %s: %v", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	f := newRestartSourceFixture(t)
	f.pairing.Name = label
	first, source0, stopFirst := f.start(t, 0)
	var endpoint atomic.Value
	endpoint.Store(first)
	var resolutions atomic.Int32
	directory := t.TempDir()
	loopback := ipcam.NewLoopback(ctx, zap.NewNop(), ipcam.NewRegistry(filepath.Join(directory, "cameras.json")), ipcam.NewCredentialStore(filepath.Join(directory, "credentials.json")), func(context.Context, []string) error { return errors.New("unexpected IP camera pump") })
	t.Cleanup(loopback.Shutdown)
	if err := loopback.Available(); err != nil {
		t.Fatal(err)
	}
	manager := audioloop.NewManager(zap.NewNop())
	supervisor := NewSupervisor(zap.NewNop(), loopback, f.transport, ros2camera.NewFrameWriter, manager)
	supervisor.SetSourceResolver(func(_ context.Context, p SensorPairing) ([]string, bool) {
		if p.Key() != f.pairing.Key() {
			return nil, false
		}
		resolutions.Add(1)
		return []string{endpoint.Load().(string)}, true
	})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- supervisor.RunPairing(runCtx, f.pairing, "") }()
	completed := false
	t.Cleanup(func() {
		stop()
		if !completed {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("restart supervisor did not release kernel writers")
			}
		}
		supervisor.releasePairing(f.pairing)
	})
	waitLive := func(source *restartSensorServer) {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for source.subscribed.Load() == 0 || !supervisor.IsPairingConnected(f.pairing) {
			select {
			case err := <-done:
				completed = true
				t.Fatalf("restart supervisor exited before both streams: %v", err)
			case <-deadline.C:
				t.Fatal("source restart did not reconnect both channels")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	waitLive(source0)
	beforeCameras := supervisor.CameraIdentities()
	beforeAudio := manager.Mounts()
	if beforeCameras[ipcam.MCUBandStart] != "sensorlink:"+f.pairing.Key()+":1" || len(beforeCameras) != 1 {
		t.Fatalf("wrong camera identity: %v", beforeCameras)
	}
	if len(beforeAudio) != 1 || beforeAudio[0].SourcePrincipalURI != f.pairing.SourcePrincipalURI || beforeAudio[0].ChannelID != 2 || beforeAudio[0].SensorName != "microphone" {
		t.Fatalf("wrong microphone identity: %+v", beforeAudio)
	}
	capture := func(phase int32) {
		t.Helper()
		decodeCtx, stopDecode := context.WithTimeout(ctx, 6*time.Second)
		command := exec.CommandContext(decodeCtx, "gst-launch-1.0", "-q", "v4l2src", "device="+path, "num-buffers=10", "!", "image/jpeg,width=64,height=48", "!", "jpegdec", "!", "videoconvert", "!", "video/x-raw,format=RGB", "!", "fdsink", "fd=1")
		var diagnostic bytes.Buffer
		command.Stderr = &diagnostic
		pixels, err := command.Output()
		stopDecode()
		const frameBytes = 64 * 48 * 3
		if err != nil || len(pixels) != 10*frameBytes {
			t.Fatalf("restart camera consumer did not decode ten frames: phase=%d bytes=%d err=%v stderr=%s", phase, len(pixels), err, diagnostic.String())
		}
		for offset := len(pixels) - 3*frameBytes; offset < len(pixels); offset += frameBytes {
			for _, point := range []struct{ x, channel int }{{16, int(phase)}, {48, 2}} {
				index := offset + (24*64+point.x)*3
				for channel, value := range pixels[index : index+3] {
					if (channel == point.channel && value < 200) || (channel != point.channel && value > 45) {
						t.Fatalf("restart camera delivered stale pixels in phase %d: %v", phase, pixels[index:index+3])
					}
				}
			}
		}
		recordCtx, stopRecord := context.WithTimeout(ctx, 6*time.Second)
		command = exec.CommandContext(recordCtx, "arecord", "-q", "-D", fmt.Sprintf("hw:Loopback,1,%d", beforeAudio[0].Sub), "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "1", "-d", "2", "-")
		diagnostic.Reset()
		command.Stderr = &diagnostic
		pcm, err := command.Output()
		stopRecord()
		if err != nil || len(pcm) != 2*48000*2 {
			t.Fatalf("restart microphone consumer failed: phase=%d bytes=%d err=%v stderr=%s", phase, len(pcm), err, diagnostic.String())
		}
		tail := pcm[len(pcm)-48000*2:]
		if !bytes.Contains(tail, bytes.Repeat(restartPCMMarker(phase), 8)) || bytes.Contains(tail, bytes.Repeat(restartPCMMarker(1-phase), 2)) {
			t.Fatalf("restart microphone delivered stale/changed PCM in phase %d", phase)
		}
		t.Logf("phase %d: decoded fresh camera pixels and exact microphone PCM through both kernel devices", phase)
	}
	capture(0)
	second, source1, _ := f.start(t, 1)
	firstHost, _, _ := net.SplitHostPort(first)
	secondHost, _, _ := net.SplitHostPort(second)
	if firstHost == secondHost {
		t.Fatal("Linux restart must change the source IP address")
	}
	t.Logf("restarting source at changed IP/endpoint: %s -> %s", first, second)
	endpoint.Store(second)
	stopFirst() // Tear down the actual old TLS stream; RunPairing must re-resolve.
	waitLive(source1)
	capture(1)
	if !reflect.DeepEqual(beforeCameras, supervisor.CameraIdentities()) || !reflect.DeepEqual(beforeAudio, manager.Mounts()) {
		t.Fatalf("source identity/allocation changed after address move: cameras=%v audio=%+v", supervisor.CameraIdentities(), manager.Mounts())
	}
	if resolutions.Load() != 2 || f.verified.Load() != 2 || source0.subscribed.Load() != 1 || source1.subscribed.Load() != 1 {
		t.Fatalf("unexpected reconnect path: resolutions=%d TLS=%d subscriptions=%d/%d", resolutions.Load(), f.verified.Load(), source0.subscribed.Load(), source1.subscribed.Load())
	}
	stop()
	select {
	case <-done:
		completed = true
	case <-time.After(3 * time.Second):
		t.Fatal("restart supervisor did not stop")
	}
	supervisor.releasePairing(f.pairing)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) || len(supervisor.CameraIdentities()) != 0 || len(manager.Mounts()) != 0 || supervisor.IsPairingConnected(f.pairing) {
		t.Fatalf("restart pairing left device allocations or liveness: %v", err)
	}
	probeCtx, stopProbe := context.WithTimeout(ctx, 3*time.Second)
	defer stopProbe()
	probe := exec.CommandContext(probeCtx, "aplay", "-q", "-D", fmt.Sprintf("hw:Loopback,0,%d", beforeAudio[0].Sub), "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "1", "-")
	probe.Stdin = bytes.NewReader(bytes.Repeat(restartPCMMarker(0), 50))
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("restart microphone retained exclusive playback owner: %v: %s", err, output)
	}
}
