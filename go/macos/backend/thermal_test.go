package backend

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
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
	f, err := decodeThermalFrame(v, &pb.ThermalCamera{Adapter: thermal.PureThermal})
	if err != nil {
		t.Fatal(err)
	}
	if f.Coldest.X != 1 || f.Coldest.Y != 0 || f.Hottest.X != 79 || f.Hottest.Y != 59 || f.TimestampNS != 123 || len(f.RGBA) != 80*60*4 {
		t.Fatalf("bad thermal frame: %+v", f)
	}
	v.RawExtrema = &pb.RawFrameExtrema{ColdX: 1, ColdRaw: 29000, HotX: 79, HotY: 59, HotRaw: 31000}
	if _, err = decodeThermalFrame(v, &pb.ThermalCamera{Adapter: thermal.PureThermal}); err != nil {
		t.Fatal(err)
	}
	v.RawExtrema.HotX = 78
	if _, err = decodeThermalFrame(v, &pb.ThermalCamera{Adapter: thermal.PureThermal}); err == nil {
		t.Fatal("accepted mismatched device measurements")
	}
	v.Codec = pb.VideoCodec_VIDEO_CODEC_H264
	if _, err = decodeThermalFrame(v, &pb.ThermalCamera{Adapter: thermal.PureThermal}); err == nil {
		t.Fatal("accepted encoded image as measurements")
	}
}

type thermalAgent struct {
	pb.UnimplementedWendyVideoServiceServer
	requests chan *pb.StreamVideoRequest
	stopped  chan struct{}
	device   *pb.VideoDevice
	frame    *pb.VideoFrame
}

func (a *thermalAgent) ListVideoDevices(context.Context, *pb.ListVideoDevicesRequest) (*pb.ListVideoDevicesResponse, error) {
	if a.device != nil {
		return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{a.device}}, nil
	}
	return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{{Id: 2, Name: "PureThermal", StableId: "by-id:thermal-fixture", PixelFormats: []string{"UYVY", "Y16 "}}}}, nil
}
func (a *thermalAgent) StreamVideo(req *pb.StreamVideoRequest, stream grpc.ServerStreamingServer[pb.VideoFrame]) error {
	a.requests <- req
	defer close(a.stopped)
	f := a.frame
	if f == nil {
		f = thermalFixture()
	}
	if err := stream.Send(f); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestTOPDONStreamUsesAdvertisedCapability(t *testing.T) {
	for _, tc := range []struct {
		adapter     string
		height      uint32
		temperature bool
	}{
		{thermal.TOPDONTC001, 384, true}, {thermal.TOPDONTC001HIK, 392, false},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			l := &pb.RawFormat{Width: 256, Height: tc.height, Fourcc: "YUYV", BytesPerLine: 512}
			plane, err := thermal.Plane(tc.adapter, l)
			if err != nil {
				t.Fatal(err)
			}
			l.ThermalPlane = plane
			data := make([]byte, 512*tc.height)
			for i := uint32(0); i < 256*192; i++ {
				binary.LittleEndian.PutUint16(data[plane.OffsetBytes+2*i:], 19200)
			}
			profile := &pb.ThermalCamera{Adapter: tc.adapter, CaptureWidth: 256, CaptureHeight: tc.height}
			a := &thermalAgent{requests: make(chan *pb.StreamVideoRequest, 1), stopped: make(chan struct{}),
				device: &pb.VideoDevice{Id: 2, Name: "USB Camera: UVC Camera", StableId: "by-id:topdon", Thermal: profile},
				frame:  &pb.VideoFrame{Codec: pb.VideoCodec_VIDEO_CODEC_RAW, Data: data, RawFormat: l, TimestampNs: 123}}
			listener := bufconn.Listen(1 << 20)
			defer listener.Close()
			server := grpc.NewServer()
			pb.RegisterWendyVideoServiceServer(server, a)
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient("passthrough:///topdon", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
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
			if err = b.StartCamera(2, 42, 7, "by-id:topdon"); err != nil {
				t.Fatal(err)
			}
			select {
			case req := <-a.requests:
				if req.Codec != pb.VideoCodec_VIDEO_CODEC_RAW || req.Width != 0 || req.Height != 0 || req.StableId != "by-id:topdon" {
					t.Fatalf("wrong capture request: %v", req)
				}
			case <-time.After(time.Second):
				t.Fatal("no request")
			}
			select {
			case event := <-b.events:
				if event.Kind != EventThermal || event.Thermal.Width != 256 || event.Thermal.Height != 192 || (event.Thermal.Temperature != nil) != tc.temperature || event.Thermal.AllowScaleOverride {
					t.Fatalf("wrong thermal event: %+v", event)
				}
			case <-time.After(time.Second):
				t.Fatal("no frame")
			}
		})
	}
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

func TestSummaryDoesNotRefreshOnRepeatedMetadata(t *testing.T) {
	var tracker thermalSummaryTracker
	now := time.Unix(100, 0)
	makeFrame := func(code uint16) *ThermalFrame {
		return &ThermalFrame{Summary: &thermal.TemperatureSummary{Identity: [10]uint16{code}}}
	}
	first := makeFrame(1)
	tracker.apply(first, now)
	if first.Summary != nil {
		t.Fatal("cached startup summary displayed")
	}
	changed := makeFrame(2)
	tracker.apply(changed, now.Add(time.Second))
	if changed.Summary == nil || changed.SummaryUpdatedNS != uint64(now.Add(time.Second).UnixNano()) {
		t.Fatal("new summary missing")
	}
	held := makeFrame(2)
	tracker.apply(held, now.Add(2*time.Second))
	if held.SummaryUpdatedNS != changed.SummaryUpdatedNS {
		t.Fatal("repeated summary refreshed age")
	}
	stale := makeFrame(2)
	tracker.apply(stale, now.Add(4*time.Second))
	if stale.Summary != nil {
		t.Fatal("stale summary displayed")
	}
}
