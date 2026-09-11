package mcusource_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/grpc"
)

type stubSensorServer struct {
	agentpbv2.UnimplementedWendySensorServiceServer
	assetID int32
}

func (s *stubSensorServer) GetSensorManifest(_ context.Context, _ *agentpbv2.GetSensorManifestRequest) (*sensorlinkpb.SensorManifest, error) {
	return &sensorlinkpb.SensorManifest{DeviceAssetId: s.assetID, Sensors: []*sensorlinkpb.SensorDescriptor{{
		ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
		Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_H264, Width: 640, Height: 480, Fps: 30}},
	}}}, nil
}

func (s *stubSensorServer) StreamSensors(req *agentpbv2.StreamSensorsRequest, stream agentpbv2.WendySensorService_StreamSensorsServer) error {
	for i := 0; i < 3; i++ {
		if err := stream.Send(&sensorlinkpb.SensorFrame{ChannelId: 1, Seq: uint32(i), Flags: 1, Payload: []byte("h264")}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

// newInsecureGRPCTransport mirrors grpcTransport but with an insecure loopback dial for the test.
func TestGRPCTransportStreamsFromStubServer(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := grpc.NewServer()
	agentpbv2.RegisterWendySensorServiceServer(srv, &stubSensorServer{assetID: 7})
	go srv.Serve(ln)
	defer srv.Stop()

	tr, err := mcusource.NewInsecureGRPCTransportForTest(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ctx := context.Background()
	m, err := tr.FetchManifest(ctx)
	if err != nil || m.GetDeviceAssetId() != 7 {
		t.Fatalf("manifest: %v %+v", err, m)
	}
	frames, closeFn, err := tr.Stream(ctx, []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	select {
	case f := <-frames:
		if f.ChannelId != 1 || string(f.Payload) != "h264" {
			t.Fatalf("bad frame: %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame from grpc stream")
	}
}
