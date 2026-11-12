//go:build linux

package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"go.uber.org/zap"
)

// Run only through test-sensorlink-audio-loopback.sh on the disposable hosted
// Linux media runner. This exercises real snd-aloop/aplay/arecord and a live
// PipeWire graph. Only the remote microphones' PCM and pairing identities are
// synthetic; capture, source resolution and the sealed WAVs are production.
func TestSensorLinkMicrophonesRecordKernelLoopbackWithLivePipeWire(t *testing.T) {
	if os.Getenv("WENDY_SENSORLINK_AUDIO_LOOPBACK") != "1" {
		t.Skip("requires the disposable Linux media fixture")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Geteuid() != 0 {
		t.Fatal("kernel audio fixture is restricted to the disposable hosted CI runner")
	}
	runtimeDir := os.Getenv("WENDY_TEST_PIPEWIRE_RUNTIME")
	if !filepath.IsAbs(runtimeDir) {
		t.Fatal("private PipeWire runtime was not supplied")
	}
	originalGlob := audio.SocketGlob
	audio.SocketGlob = filepath.Join(runtimeDir, "pipewire-0")
	t.Cleanup(func() { audio.SocketGlob = originalGlob })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !audio.Available() {
		t.Fatalf("real trusted PipeWire session is unavailable: %s", audio.UnavailableReason())
	}
	nodes, _, err := audio.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var liveGraphSource bool
	for _, node := range nodes {
		if node.Name == "wendy-regression-unused-side" && !node.IsSink {
			liveGraphSource = true
		}
	}
	if !liveGraphSource {
		t.Fatal("real PipeWire ALSA source did not appear in pw-dump")
	}
	manager := audioloop.NewManager(zap.NewNop())
	principals := []string{"urn:wendy:fixture:microphone:a", "urn:wendy:fixture:microphone:b"}
	markers := [][]byte{kernelAudioMarker(0x12345678), kernelAudioMarker(0x87654321)}
	for i, principal := range principals {
		sub, err := manager.AllocatePrincipal(principal, 1, "microphone")
		if err != nil || sub != i {
			t.Fatalf("allocate microphone %d: sub=%d err=%v", i, sub, err)
		}
		playCtx, stop := context.WithCancel(ctx)
		writer, err := manager.OpenWriter(playCtx, sub, audioloop.PCMFormat{SampleRate: 48000, Channels: 1})
		if err != nil {
			stop()
			t.Fatal(err)
		}
		done := make(chan struct{})
		payload := bytes.Repeat(markers[i], 16)
		go func() {
			defer close(done)
			for playCtx.Err() == nil {
				if err := writer.WritePCM(payload); err != nil {
					return
				}
			}
		}()
		t.Cleanup(func() {
			stop()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("kernel playback writer did not stop")
			}
			_ = writer.Close() // CommandContext cancellation kills only this aplay.
			manager.ReleasePrincipal(principal)
		})
	}
	service := NewAudioService(zap.NewNop())
	service.SetSensorLinkNaming(manager.Mounts, nil)
	service.SetSensorLinkPrincipalNaming(func(uri string) (string, bool) {
		for i, principal := range principals {
			if uri == principal {
				return fmt.Sprintf("ci-mic-%d", i), true
			}
		}
		return "", false
	})
	adapter := newAudioDataAdapter(service).(*audioDataAdapter)
	sources := adapter.Discover(ctx)
	selected := make([]data.Source, 2)
	for i := range selected {
		id := fmt.Sprintf("audio:sensor-link:ci-mic-%d:microphone", i)
		var matches int
		for _, source := range sources {
			if source.ID == id {
				selected[i] = source
				matches++
			}
		}
		if matches != 1 || !selected[i].Healthy || selected[i].ClockDomain != alsaCaptureClockDomain {
			t.Fatalf("named SensorLink source missing or wrong clock domain: id=%s matches=%d source=%+v", id, matches, selected[i])
		}
	}
	_, requestTime, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{ID: "kernel-loopback-fixture", Directory: t.TempDir(), RequestBootNanos: requestTime}
	running, err := adapter.Start(ctx, session, selected)
	if err != nil || running == nil {
		t.Fatalf("live SensorLink recording could not start: %v", err)
	}
	t.Cleanup(func() { _, _ = running.Stop(context.Background()) })
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		t.Fatal("live SensorLink recording exceeded its deadline")
	}
	results, err := running.Stop(ctx)
	if err != nil || len(results) != 2 {
		t.Fatalf("sealing live SensorLink recording: results=%+v err=%v", results, err)
	}
	for i, source := range selected {
		var resultFound bool
		for _, result := range results {
			if result.SourceID == source.ID {
				resultFound = result.Count > 0 && result.ClockDomain == alsaCaptureClockDomain
			}
		}
		if !resultFound {
			t.Fatalf("recorded source identity/clock/count lost for %s: %+v", source.ID, results)
		}
		dir := filepath.Join(session.Directory, "audio", safeCaptureName(source.ID))
		var found bool
		for _, path := range wavFragments(t, dir) {
			if wavDataBytes(t, path) < 48000 {
				t.Fatalf("kernel recording for %s is too short", source.ID)
			}
			wav, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if binary.LittleEndian.Uint16(wav[22:24]) != 1 || binary.LittleEndian.Uint32(wav[24:28]) != 48000 || binary.LittleEndian.Uint16(wav[34:36]) != 16 {
				t.Fatalf("recorded WAV format for %s is not mono 48kHz S16", source.ID)
			}
			pcm := wav[wavHeaderBytes:]
			found = found || bytes.Contains(pcm, bytes.Repeat(markers[i], 8))
			if bytes.Contains(pcm, bytes.Repeat(markers[1-i], 2)) {
				t.Fatalf("kernel recording crossed microphone identities for %s", source.ID)
			}
		}
		if !found {
			t.Fatalf("kernel recording did not preserve exact microphone PCM for %s", source.ID)
		}
		t.Logf("%s: recorded eight exact 997-sample marker periods through snd-aloop, with no other microphone marker", source.ID)
	}
}

// Distinct deterministic full-width S16 patterns identify each synthetic remote
// microphone. Exact long matches reject silence, resampling and wrong routing.
func kernelAudioMarker(seed uint32) []byte {
	marker := make([]byte, 997*2)
	for i := 0; i < len(marker); i += 2 {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		binary.LittleEndian.PutUint16(marker[i:], uint16(seed))
	}
	return marker
}
