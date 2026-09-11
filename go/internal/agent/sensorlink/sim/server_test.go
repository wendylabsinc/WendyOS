package sim_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestServerSendsManifestThenFrames(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 99, Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4, Fps: 10}},
		}}},
		Frames:        [][]byte{[]byte("frame-a"), []byte("frame-b")},
		FrameInterval: time.Millisecond,
	}
	go sim.Serve(ctx, ln, opts)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	env, err := sensorlink.ReadMessage(conn)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if env.GetManifest().GetDeviceAssetId() != 99 {
		t.Fatalf("bad manifest: %+v", env.GetManifest())
	}
	if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Subscribe{Subscribe: &sensorlinkpb.Subscribe{ChannelId: []uint32{1}}}}); err != nil {
		t.Fatal(err)
	}
	f, err := sensorlink.ReadMessage(conn)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.GetFrame().ChannelId != 1 || len(f.GetFrame().Payload) == 0 {
		t.Fatalf("bad frame: %+v", f.GetFrame())
	}
}

func TestServerWithEmptyFramesNosPanic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 99, Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 4, Height: 4, Fps: 10}},
		}}},
		Frames:        [][]byte{}, // empty frames
		FrameInterval: time.Millisecond,
	}
	go sim.Serve(ctx, ln, opts)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	env, err := sensorlink.ReadMessage(conn)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if env.GetManifest().GetDeviceAssetId() != 99 {
		t.Fatalf("bad manifest: %+v", env.GetManifest())
	}
	if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Subscribe{Subscribe: &sensorlinkpb.Subscribe{ChannelId: []uint32{1}}}}); err != nil {
		t.Fatal(err)
	}
	// Connection should close cleanly without panic; no frames should arrive.
	_, err = sensorlink.ReadMessage(conn)
	if err == nil {
		t.Fatal("expected connection to close after empty Frames check")
	}
}
