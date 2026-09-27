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

type tcpDialer struct{}

func (tcpDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func TestConnectReceivesManifestAndFrames(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sim.Serve(ctx, ln, sim.Options{
		Manifest:      &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Name: "cam0"}}},
		Frames:        [][]byte{[]byte("jpg")},
		FrameInterval: time.Millisecond,
	})

	stream, err := mcusource.Connect(ctx, tcpDialer{}, ln.Addr().String(), []uint32{1})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer stream.Close()
	if len(stream.Manifest.GetSensors()) != 1 {
		t.Fatalf("bad manifest: %+v", stream.Manifest)
	}
	select {
	case f := <-stream.Frames:
		if f.ChannelID != 1 {
			t.Fatalf("bad frame channel: %d", f.ChannelID)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame received")
	}
}
