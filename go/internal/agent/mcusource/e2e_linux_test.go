//go:build e2e_v4l2loopback

package mcusource_test

// Requires: a Linux host with v4l2loopback 0.15.x loaded and CAP_SYS_ADMIN.
// Run: go test -tags e2e_v4l2loopback ./internal/agent/mcusource/ -run TestEndToEndCameraMount -v
//
// It starts the simulator, runs a real ipcam.Loopback + supervisor, and reads
// back the created /dev/videoN as a V4L2 CAPTURE device asserting bytes arrive.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// TestEndToEndCameraMount wires the real ipcam.Loopback (real v4l2loopback
// ioctls) and the real mcusource.Supervisor together, dials a sensorlink
// simulator over TCP, and asserts a frame lands on the created MCU-band
// /dev/video256 node.
func TestEndToEndCameraMount(t *testing.T) {
	dir := t.TempDir()

	reg := ipcam.NewRegistry(filepath.Join(dir, "cameras.json"))
	creds := ipcam.NewCredentialStore(filepath.Join(dir, "credentials.json"))
	if err := creds.Load(); err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No IP-camera pumps are exercised in this test; the supervisor under
	// test drives the MCU band directly via EnsureNode/NodePath.
	noopPump := func(ctx context.Context, args []string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	lb := ipcam.NewLoopback(ctx, zap.NewNop(), reg, creds, noopPump)
	if err := lb.Available(); err != nil {
		t.Fatalf("v4l2loopback unavailable: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 42, Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 640, Height: 480, Fps: 30}},
		}}},
		Frames:        [][]byte{{0xFF, 0xD8, 0xFF, 0xD9}}, // minimal JPEG SOI/EOI
		FrameInterval: 10 * time.Millisecond,
	})

	sup := mcusource.NewSupervisor(zap.NewNop(), lb,
		func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
			return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
		},
		ros2camera.NewFrameWriter, noAudioLoop{},
	)

	pairing := mcusource.SensorPairing{SourceAssetID: 42, OrgID: 1, Name: "e2e"}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	done := make(chan error, 1)
	go func() { done <- sup.RunPairing(rctx, pairing, ln.Addr().String()) }()

	// Wait for the MCU-band node to actually appear on disk.
	const nodePath = "/dev/video256"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(nodePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", nodePath)
		}
		time.Sleep(50 * time.Millisecond)
	}

	f, err := os.OpenFile(nodePath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", nodePath, err)
	}
	defer f.Close()

	buf := make([]byte, 4096)
	if err := f.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("read %s: %v", nodePath, err)
	}
	if n == 0 {
		t.Fatal("expected bytes from the MCU-band loopback node, got none")
	}

	rcancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPairing did not exit after cancellation")
	}
}
