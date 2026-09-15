package mcusource_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// noAudioLoop is a stub AudioLoop for camera-only tests, where it is never
// called.
type noAudioLoop struct{}

func (noAudioLoop) ReleaseSource(int32) {}

func (noAudioLoop) Allocate(int32, uint32, string) (int, error) { return 0, nil }
func (noAudioLoop) OpenWriter(context.Context, int, audioloop.PCMFormat) (audioloop.AudioWriter, error) {
	return nil, nil
}

// recordingAudioWriter is an AudioWriter that records every WritePCM
// payload, for asserting mic PCM reaches the writer.
type recordingAudioWriter struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (w *recordingAudioWriter) WritePCM(pcm []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.payloads = append(w.payloads, append([]byte(nil), pcm...))
	return nil
}
func (w *recordingAudioWriter) Close() error { return nil }
func (w *recordingAudioWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.payloads)
}

// fakeAudioLoop is a stub AudioLoop that always returns the given writer.
type fakeAudioLoop struct{ w *recordingAudioWriter }

func (*fakeAudioLoop) ReleaseSource(int32) {}

func (f *fakeAudioLoop) Allocate(int32, uint32, string) (int, error) { return 0, nil }
func (f *fakeAudioLoop) OpenWriter(context.Context, int, audioloop.PCMFormat) (audioloop.AudioWriter, error) {
	return f.w, nil
}

type fakeLoopback struct {
	mu      sync.Mutex
	ensured []uint32
	labels  []string
}

func (f *fakeLoopback) EnsureNode(_ context.Context, id uint32, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, id)
	f.labels = append(f.labels, label)
	return nil
}
func (f *fakeLoopback) RemoveCamera(uint32)               {}
func (f *fakeLoopback) NodePath(id uint32) (string, bool) { return "/dev/video-fake", true }

// idsForLabel returns every node id EnsureNode was called with for label, in
// call order.
func (f *fakeLoopback) idsForLabel(label string) []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uint32
	for i, l := range f.labels {
		if l == label {
			out = append(out, f.ensured[i])
		}
	}
	return out
}

type fakeWriter struct {
	mu     sync.Mutex
	frames int
}

func (w *fakeWriter) WriteFrame(ros2camera.Frame) error {
	w.mu.Lock()
	w.frames++
	w.mu.Unlock()
	return nil
}
func (w *fakeWriter) Close() error { return nil }
func (w *fakeWriter) count() int   { w.mu.Lock(); defer w.mu.Unlock(); return w.frames }

func TestSupervisorMountsCameraAndWritesFrames(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest:      &sensorlinkpb.SensorManifest{DeviceAssetId: 8, Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0", Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4}}}}},
		Frames:        [][]byte{[]byte("jpg")},
		FrameInterval: time.Millisecond,
	})

	lb := &fakeLoopback{}
	w := &fakeWriter{}
	sup := mcusource.NewSupervisor(zap.NewNop(), lb, func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
		return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
	}, func(string) ros2camera.CameraWriter { return w }, noAudioLoop{})

	rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer rcancel()
	_ = sup.RunPairing(rctx, mcusource.SensorPairing{SourceAssetID: 8, OrgID: 1}, ln.Addr().String())

	if len(lb.ensured) == 0 || lb.ensured[0] < 256 {
		t.Fatalf("expected an MCU-band node to be ensured, got %v", lb.ensured)
	}
	if w.count() == 0 {
		t.Fatal("expected frames written to the camera writer")
	}
}

func camManifest(deviceAssetID int32, channelID uint32, name string) *sensorlinkpb.SensorManifest {
	return &sensorlinkpb.SensorManifest{
		DeviceAssetId: deviceAssetID,
		Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: channelID,
			Kind:      sensorlinkpb.SensorDescriptor_CAMERA,
			Name:      name,
			Format:    &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4}},
		}},
	}
}

// TestSupervisorRunPairingReturnsPromptlyOnIdleStream guards against
// streamOnce blocking forever on `range stream.Frames` when the source has
// gone idle (no frames, no read error) and the caller cancels ctx.
func TestSupervisorRunPairingReturnsPromptlyOnIdleStream(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest: camManifest(11, 1, "cam0"),
		Frames:   [][]byte{[]byte("jpg")},
		// Long enough that no frame is sent during this test's lifetime, so
		// the source has read Subscribe and then gone silent.
		FrameInterval: 10 * time.Second,
	})

	lb := &fakeLoopback{}
	sup := mcusource.NewSupervisor(zap.NewNop(), lb, func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
		return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
	}, func(string) ros2camera.CameraWriter { return &fakeWriter{} }, noAudioLoop{})

	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- sup.RunPairing(rctx, mcusource.SensorPairing{SourceAssetID: 11, OrgID: 1}, ln.Addr().String())
	}()

	// Give the supervisor time to mount the node and open the frame stream.
	time.Sleep(100 * time.Millisecond)
	rcancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPairing did not return within 2s of ctx cancellation while the stream was idle")
	}
}

// TestSupervisorNodeIDsUniqueAcrossSources asserts the node-id allocator
// never hands the same MCU-band id to two different sources, even when both
// happen to expose the same channel id.
func TestSupervisorNodeIDsUniqueAcrossSources(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln1, sim.Options{Manifest: camManifest(21, 1, "cam0"), Frames: [][]byte{[]byte("jpg")}, FrameInterval: time.Millisecond})
	go sim.Serve(ctx, ln2, sim.Options{Manifest: camManifest(22, 1, "cam0"), Frames: [][]byte{[]byte("jpg")}, FrameInterval: time.Millisecond})

	lb := &fakeLoopback{}
	sup := mcusource.NewSupervisor(zap.NewNop(), lb, func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
		return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
	}, func(string) ros2camera.CameraWriter { return &fakeWriter{} }, noAudioLoop{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rctx, rcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer rcancel()
		_ = sup.RunPairing(rctx, mcusource.SensorPairing{SourceAssetID: 21, OrgID: 1, Name: "src21"}, ln1.Addr().String())
	}()
	go func() {
		defer wg.Done()
		rctx, rcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer rcancel()
		_ = sup.RunPairing(rctx, mcusource.SensorPairing{SourceAssetID: 22, OrgID: 1, Name: "src22"}, ln2.Addr().String())
	}()
	wg.Wait()

	ids1 := lb.idsForLabel("src21:cam0")
	ids2 := lb.idsForLabel("src22:cam0")
	if len(ids1) == 0 || len(ids2) == 0 {
		t.Fatalf("expected both sources to mount a node, got %v and %v", ids1, ids2)
	}
	if ids1[0] == ids2[0] {
		t.Fatalf("expected different node ids for different sources, both got %d", ids1[0])
	}
}

// TestSupervisorNodeIDStableAcrossReconnect asserts that asking for the same
// (source, channel) twice -- as happens on every reconnect -- returns the
// same MCU-band node id both times.
func TestSupervisorNodeIDStableAcrossReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := &fakeLoopback{}
	sup := mcusource.NewSupervisor(zap.NewNop(), lb, func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
		return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
	}, func(string) ros2camera.CameraWriter { return &fakeWriter{} }, noAudioLoop{})
	pairing := mcusource.SensorPairing{SourceAssetID: 31, OrgID: 1, Name: "src31"}

	connectOnce := func() {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer ln.Close()
		go sim.Serve(ctx, ln, sim.Options{Manifest: camManifest(31, 1, "cam0"), Frames: [][]byte{[]byte("jpg")}, FrameInterval: time.Millisecond})
		rctx, rcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer rcancel()
		_ = sup.RunPairing(rctx, pairing, ln.Addr().String())
	}
	connectOnce() // first connect: allocates the node id
	connectOnce() // "reconnect": must reuse the same node id

	ids := lb.idsForLabel("src31:cam0")
	if len(ids) < 2 {
		t.Fatalf("expected at least 2 EnsureNode calls across the two connects, got %v", ids)
	}
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("expected a stable node id across reconnects, got %v", ids)
		}
	}
}

// TestSupervisorMountsCameraAndMicrophone asserts that a manifest with both
// a camera and a microphone channel fans frames out to the right writer by
// kind: camera frames to the CameraWriter, mic PCM to the AudioWriter, and
// both channels are subscribed.
func TestSupervisorMountsCameraAndMicrophone(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 42, Sensors: []*sensorlinkpb.SensorDescriptor{
			{
				ChannelId: 2, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
				Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4}},
			},
			{
				ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_MICROPHONE, Name: "mic0",
				Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_PCM_S16LE, SampleRate: 48000, Channels: 1}},
			},
		}},
		Frames:        [][]byte{[]byte("pcm-or-jpg")},
		FrameInterval: time.Millisecond,
	})

	lb := &fakeLoopback{}
	camW := &fakeWriter{}
	audioW := &recordingAudioWriter{}
	sup := mcusource.NewSupervisor(zap.NewNop(), lb,
		func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
			return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
		},
		func(string) ros2camera.CameraWriter { return camW },
		&fakeAudioLoop{w: audioW},
	)

	rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer rcancel()
	_ = sup.RunPairing(rctx, mcusource.SensorPairing{SourceAssetID: 42, OrgID: 1}, ln.Addr().String())

	if camW.count() == 0 {
		t.Fatal("expected frames written to the camera writer")
	}
	if audioW.count() == 0 {
		t.Fatal("expected PCM written to the audio writer")
	}
	if len(lb.idsForLabel("cam0")) == 0 {
		t.Fatal("expected the camera channel to be subscribed")
	}
}
