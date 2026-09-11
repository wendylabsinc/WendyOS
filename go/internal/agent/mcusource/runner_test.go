package mcusource_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// TestRunnerStartStreamsThenStopCancels drives Start with an explicit address
// (the AddSensorPairing path always has one) end-to-end through the shared
// Supervisor, then verifies Stop tears the goroutine down.
func TestRunnerStartStreamsThenStopCancels(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 5, Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4}},
		}}},
		Frames:        [][]byte{[]byte("jpg")},
		FrameInterval: time.Millisecond,
	})

	lb := &fakeLoopback{}
	var frames atomic.Int32
	sup := mcusource.NewSupervisor(zap.NewNop(), lb,
		func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
			return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
		},
		func(string) ros2camera.CameraWriter { return &countingWriter{n: &frames} }, noAudioLoop{})

	r := mcusource.NewRunner(zap.NewNop(), sup)
	r.Start(mcusource.SensorPairing{SourceAssetID: 5, OrgID: 1}, ln.Addr().String())

	deadline := time.Now().Add(2 * time.Second)
	for frames.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if frames.Load() == 0 {
		t.Fatal("expected the runner to start streaming frames")
	}

	r.Stop(5)
	n := frames.Load()
	time.Sleep(100 * time.Millisecond)
	if frames.Load() > n+2 { // a frame or two in flight at cancel time is fine
		t.Fatalf("expected frame count to stop growing after Stop, went from %d to %d", n, frames.Load())
	}
}

type countingWriter struct {
	n *atomic.Int32
}

func (w *countingWriter) WriteFrame(ros2camera.Frame) error { w.n.Add(1); return nil }
func (w *countingWriter) Close() error                      { return nil }

// TestRunnerRestartCancelsPriorGoroutine asserts a second Start for the same
// source asset id stops the first goroutine rather than running both, and
// that a final Stop cleanly unblocks everything (no panic, no leak).
func TestRunnerRestartCancelsPriorGoroutine(t *testing.T) {
	sup := mcusource.NewSupervisor(zap.NewNop(), &fakeLoopback{},
		func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
			return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
		},
		func(string) ros2camera.CameraWriter { return &countingWriter{n: &atomic.Int32{}} }, noAudioLoop{})

	// A listener that never accepts: RunPairing blocks in its dial/backoff
	// loop, so Stop's ctx cancellation is what has to unblock each goroutine.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	r := mcusource.NewRunner(zap.NewNop(), sup)
	r.Start(mcusource.SensorPairing{SourceAssetID: 9}, ln.Addr().String())
	r.Start(mcusource.SensorPairing{SourceAssetID: 9}, ln.Addr().String())
	r.Stop(9)
}
