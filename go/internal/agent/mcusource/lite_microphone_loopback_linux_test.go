//go:build linux

package mcusource

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"go.uber.org/zap"
)

// The source is synthetic; TLS/WendyCom parsing, microphone selection, the
// Supervisor audio writer, aplay, snd-aloop and capture with arecord are real.
// Run through the existing owned-module audio gate, never on a developer host.
func TestLiteMicrophoneDeliversPCMThroughAuthenticatedKernelLoopback(t *testing.T) {
	if os.Getenv("WENDY_SENSORLINK_AUDIO_LOOPBACK") != "1" {
		t.Skip("requires the disposable Linux audio fixture")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Geteuid() != 0 {
		t.Fatal("kernel microphone fixture is restricted to the disposable hosted CI runner")
	}
	if _, err := os.Stat("/sys/module/snd_aloop"); err != nil {
		t.Fatal("outer fixture did not load its owned audio module")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fixture := newLiteMicrophoneFixture(t, ctx)
	manager := audioloop.NewManager(zap.NewNop())
	// No camera descriptor exists: any camera allocation would be a test failure
	// via the nil camera dependencies, not an emulated kernel camera.
	supervisor := NewSupervisor(zap.NewNop(), nil, fixture.transport, nil, manager)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := supervisor.streamOnce(runCtx, fixture.pairing, fixture.address); done <- err }()
	completed := false
	t.Cleanup(func() {
		stop()
		if !completed {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("Lite microphone source did not release its playback writer")
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
			t.Fatalf("Lite microphone ended before kernel capture became live: %v", err)
		case <-deadline.C:
			t.Fatal("Lite microphone did not deliver to its kernel playback writer")
		case <-time.After(20 * time.Millisecond):
		}
	}
	mounts := manager.Mounts()
	if len(mounts) != 1 || mounts[0].SourcePrincipalURI != fixture.pairing.SourcePrincipalURI || mounts[0].ChannelID != 1 || mounts[0].SensorName != "microphone" {
		t.Fatalf("authenticated microphone allocation identity was lost: %+v", mounts)
	}
	capture := fmt.Sprintf("hw:Loopback,1,%d", mounts[0].Sub)
	for _, phase := range []int32{0, 1} {
		fixture.phase.Store(phase)
		recordCtx, stopRecord := context.WithTimeout(ctx, 6*time.Second)
		command := exec.CommandContext(recordCtx, "arecord", "-q", "-D", capture, "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "1", "-d", "2", "-")
		var diagnostics bytes.Buffer
		command.Stderr = &diagnostics
		pcm, err := command.Output()
		stopRecord()
		if err != nil || len(pcm) != 2*48000*2 {
			t.Fatalf("kernel microphone consumer did not record two seconds: bytes=%d err=%v stderr=%s", len(pcm), err, diagnostics.String())
		}
		// Ignore startup/ring contents: the final second must contain multiple
		// exact new marker periods and no complete old-phase marker.
		tail := pcm[len(pcm)-48000*2:]
		if !bytes.Contains(tail, bytes.Repeat(liteMicrophoneMarker(phase), 8)) || bytes.Contains(tail, bytes.Repeat(liteMicrophoneMarker(1-phase), 2)) {
			t.Fatalf("kernel microphone did not preserve fresh authenticated PCM for phase %d", phase)
		}
		t.Logf("recorded exact 48kHz mono PCM through authenticated Lite source and kernel loopback, phase %d", phase)
	}
	stop()
	select {
	case err := <-done:
		completed = true
		_ = err // Transport closure is expected on cancellation.
	case <-time.After(3 * time.Second):
		t.Fatal("Lite microphone writer did not stop on cancellation")
	}
	if supervisor.IsPairingConnected(fixture.pairing) || !fixture.verifiedConsumer.Load() || fixture.accepted.Load() != 1 {
		t.Fatal("microphone liveness or single authenticated source connection was incorrect")
	}
	supervisor.releasePairing(fixture.pairing)
	if len(manager.Mounts()) != 0 {
		t.Fatal("microphone allocation survived pairing release")
	}
	// Prove normal Go teardown released the hardware owner before the outer
	// guardian runs. An abandoned aplay would make this exclusive hw open fail.
	probeCtx, stopProbe := context.WithTimeout(ctx, 3*time.Second)
	defer stopProbe()
	probe := exec.CommandContext(probeCtx, "aplay", "-q", "-D", fmt.Sprintf("hw:Loopback,0,%d", mounts[0].Sub),
		"-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "1", "-")
	probe.Stdin = bytes.NewReader(bytes.Repeat(liteMicrophoneMarker(0), 50))
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("released Lite microphone retained its kernel playback owner: %v: %s", err, output)
	}
}
