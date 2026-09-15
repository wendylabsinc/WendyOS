package mcusource_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestTCPTransportManifestAndStream(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest:      &sensorlinkpb.SensorManifest{DeviceAssetId: 5, Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0"}}},
		Frames:        [][]byte{[]byte("jpg")},
		FrameInterval: time.Millisecond,
	})
	tr := mcusource.NewTCPTransport(tcpDialer{}, ln.Addr().String())
	m, err := tr.FetchManifest(ctx)
	if err != nil || m.GetDeviceAssetId() != 5 {
		t.Fatalf("manifest: %v %+v", err, m)
	}
	frames, closeFn, err := tr.Stream(ctx, []uint32{1})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer closeFn()
	select {
	case f := <-frames:
		if f.ChannelId != 1 {
			t.Fatalf("bad frame: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame")
	}
}
