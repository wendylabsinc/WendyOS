package backend

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"net"
	"strings"
	"testing"
)

type audioAgent struct {
	pb.UnimplementedWendyAudioServiceServer
	calls chan uint32
}

func (s *audioAgent) ListAudioDevices(context.Context, *pb.ListAudioDevicesRequest) (*pb.ListAudioDevicesResponse, error) {
	return &pb.ListAudioDevicesResponse{Devices: []*pb.AudioDevice{{DeviceId: 1, Type: pb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT}, {DeviceId: 2, Type: pb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT, IsDefault: true}}}, nil
}
func (s *audioAgent) SetDefaultAudioDevice(_ context.Context, req *pb.SetDefaultAudioDeviceRequest) (*pb.SetDefaultAudioDeviceResponse, error) {
	s.calls <- req.DeviceId
	if req.DeviceId == 2 {
		return &pb.SetDefaultAudioDeviceResponse{ErrorMessage: proto.String("audio server rejected change")}, nil
	}
	return &pb.SetDefaultAudioDeviceResponse{Success: true}, nil
}
func TestAudioDefaultsValidateSelectionAndResponse(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &audioAgent{calls: make(chan uint32, 4)}
	pb.RegisterWendyAudioServiceServer(server, fake)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///audio", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.session = 7
	b.life, b.stop = context.WithCancel(context.Background())
	t.Cleanup(func() { b.Shutdown(); server.Stop(); listener.Close() })
	for _, args := range []struct {
		session   uint64
		id        uint32
		direction int32
	}{{0, 1, 1}, {6, 1, 1}, {7, 1, 2}, {7, 99, 1}, {7, 1, 0}} {
		if err := b.SetDefaultAudio(args.session, args.id, args.direction); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	select {
	case <-fake.calls:
		t.Fatal("invalid selection mutated device")
	default:
	}
	if err := b.SetDefaultAudio(7, 1, 1); err != nil {
		t.Fatal(err)
	}
	if id := <-fake.calls; id != 1 {
		t.Fatal("wrong audio endpoint selected")
	}
	if err := b.SetDefaultAudio(7, 2, 2); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("negative acknowledgement lost: %v", err)
	}
	devices, err := b.AudioDevices(7)
	if err != nil || len(devices) != 2 || !devices[1].IsDefault {
		t.Fatal("input/output defaults lost")
	}
}
