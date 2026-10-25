package backend

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func thermalFixture() *pb.VideoFrame {
	payload := make([]byte, 80*60*2)
	for i := 0; i < len(payload); i += 2 {
		binary.LittleEndian.PutUint16(payload[i:], 30000)
	}
	binary.LittleEndian.PutUint16(payload[2:], 29000)
	binary.LittleEndian.PutUint16(payload[len(payload)-2:], 31000)
	return &pb.VideoFrame{Codec: pb.VideoCodec_VIDEO_CODEC_RAW, Data: payload, TimestampNs: 123, RawFormat: &pb.RawFormat{Width: 80, Height: 60, BytesPerLine: 160, Fourcc: "Y16 "}}
}

func TestThermalSummaryMatchesImage(t *testing.T) {
	v := thermalFixture()
	f, err := decodeThermalFrame(v)
	if err != nil {
		t.Fatal(err)
	}
	if f.Coldest.X != 1 || f.Coldest.Y != 0 || f.Hottest.X != 79 || f.Hottest.Y != 59 || f.TimestampNS != 123 || len(f.RGBA) != 80*60*4 {
		t.Fatalf("bad thermal frame: %+v", f)
	}
	v.RawExtrema = &pb.RawFrameExtrema{ColdX: 1, ColdRaw: 29000, HotX: 79, HotY: 59, HotRaw: 31000}
	if _, err = decodeThermalFrame(v); err != nil {
		t.Fatal(err)
	}
	v.RawExtrema.HotX = 78
	if _, err = decodeThermalFrame(v); err == nil {
		t.Fatal("accepted mismatched device measurements")
	}
	v.Codec = pb.VideoCodec_VIDEO_CODEC_H264
	if _, err = decodeThermalFrame(v); err == nil {
		t.Fatal("accepted encoded image as measurements")
	}
}

type thermalAgent struct {
	pb.UnimplementedWendyVideoServiceServer
	requests chan *pb.StreamVideoRequest
	stopped  chan struct{}
}

func (a *thermalAgent) ListVideoDevices(context.Context, *pb.ListVideoDevicesRequest) (*pb.ListVideoDevicesResponse, error) {
	return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{{Id: 2, Name: "PureThermal", StableId: "by-id:thermal-fixture", PixelFormats: []string{"UYVY", "Y16 "}}}}, nil
}
func (a *thermalAgent) StreamVideo(req *pb.StreamVideoRequest, stream grpc.ServerStreamingServer[pb.VideoFrame]) error {
	a.requests <- req
	defer close(a.stopped)
	if err := stream.Send(thermalFixture()); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestThermalStreamUsesRawStableIdentityAndCancels(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agent := &thermalAgent{requests: make(chan *pb.StreamVideoRequest, 1), stopped: make(chan struct{})}
	pb.RegisterWendyVideoServiceServer(server, agent)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///thermal", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.life, b.stop = context.WithCancel(context.Background())
	b.session = 7
	defer b.Shutdown()
	if _, err = b.Cameras(); err != nil {
		t.Fatal(err)
	}
	if err = b.StartCamera(2, 41, 7, "by-id:another-camera"); err == nil {
		t.Fatal("accepted a reused camera number with a different identity")
	}
	if err = b.StartCamera(2, 42, 7, "by-id:thermal-fixture"); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-agent.requests:
		if req.Codec != pb.VideoCodec_VIDEO_CODEC_RAW || req.StableId != "by-id:thermal-fixture" {
			t.Fatalf("wrong request: %v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("no stream request")
	}
	select {
	case event := <-b.events:
		if event.Kind != EventThermal || event.Stream != 42 || event.Session != 7 || event.Thermal.Hottest.Raw != 31000 {
			t.Fatalf("wrong event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("no thermal frame")
	}
	// An old stop must not cancel the active generation.
	if err = b.StopCamera(2, 41, 7); err != nil {
		t.Fatal(err)
	}
	select {
	case <-agent.stopped:
		t.Fatal("stale stop cancelled stream")
	default:
	}
	if err = b.StopCamera(2, 42, 7); err != nil {
		t.Fatal(err)
	}
	select {
	case <-agent.stopped:
	case <-time.After(time.Second):
		t.Fatal("stream did not cancel")
	}
	b.disconnect()
	if b.cameraDevices != nil {
		t.Fatal("camera identity survived disconnect")
	}
}
