package backend

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type previewAgent struct {
	pb.UnimplementedWendyVideoServiceServer
	pb.UnimplementedWendyContainerServiceServer
}

func (*previewAgent) ListContainers(_ *pb.ListContainersRequest, stream grpc.ServerStreamingServer[pb.ListContainersResponse]) error {
	return stream.Send(&pb.ListContainersResponse{Container: &pb.AppContainer{AppName: "dev.wendy.thermal-viewer", RunningState: pb.AppRunningState_RUNNING, HttpPort: 8080}})
}

func (*previewAgent) ListVideoDevices(context.Context, *pb.ListVideoDevicesRequest) (*pb.ListVideoDevicesResponse, error) {
	return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{
		{Id: 0, StableId: "camera", Name: "Camera"},
		{Id: 1, StableId: "duplicate"}, {Id: 2, StableId: "duplicate"},
	}}, nil
}
func (*previewAgent) StreamVideo(req *pb.StreamVideoRequest, stream grpc.ServerStreamingServer[pb.VideoFrame]) error {
	if req.GetStableId() != "camera" || req.GetDeviceId() != 0 {
		return errors.New("wrong sensor")
	}
	if err := stream.Send(&pb.VideoFrame{Codec: pb.VideoCodec_VIDEO_CODEC_H264, Data: []byte{0, 0, 1, 0x67, 1}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}
func previewFixture(t *testing.T) *Backend {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterWendyVideoServiceServer(server, &previewAgent{})
	pb.RegisterWendyContainerServiceServer(server, &previewAgent{})
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	b := New()
	b.connectTarget = func(ctx context.Context, selector string) (*grpcclient.AgentConnection, liteConnection, error) {
		conn, err := grpc.NewClient("passthrough:///"+selector, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
		if err != nil {
			return nil, nil, err
		}
		agent := grpcclient.NewFromConn(conn)
		agent.Host = "preview-device"
		agent.AgentService = vpnProbeClient{call: func(context.Context) error { return nil }}
		return agent, nil, nil
	}
	t.Cleanup(func() { b.previewsReset(true) })
	return b
}
func TestPreviewsKeepDevicesAndInteractiveSessionSeparate(t *testing.T) {
	b := previewFixture(t)
	b.session = 99
	first, second := b.PreviewCreate(), b.PreviewCreate()
	for index, id := range []uint64{first, second} {
		selector := []string{"first-device", "second-device"}[index]
		cameras, err := b.PreviewCameras(id, selector, false)
		if err != nil || len(cameras) != 3 {
			t.Fatalf("inventory: %v %v", cameras, err)
		}
		if err := b.PreviewPlay(id, "camera"); err != nil {
			t.Fatal(err)
		}
	}
	if b.session != 99 || b.conn != nil {
		t.Fatal("preview replaced interactive session")
	}
	seen := map[uint64]bool{}
	deadline := time.Now().Add(time.Second)
	for len(seen) < 2 && time.Now().Before(deadline) {
		for _, id := range []uint64{first, second} {
			for _, event := range b.PreviewPoll(id) {
				if event.Stream != id || event.Kind != EventVideo {
					t.Fatalf("crossed preview events: %+v", event)
				}
				seen[id] = true
			}
		}
		time.Sleep(time.Millisecond)
	}
	if len(seen) != 2 {
		t.Fatal("both devices did not stream")
	}
	b.PreviewClose(first)
	b.PreviewClose(first)
	if b.preview(second) == nil || b.preview(second).ctx.Err() != nil {
		t.Fatal("closing one preview stopped another")
	}
	if err := b.PreviewPlay(second, "duplicate"); err == nil {
		t.Fatal("ambiguous camera accepted")
	}
	if err := b.PreviewPlay(second, "missing"); err == nil {
		t.Fatal("missing camera accepted")
	}
	if err := b.PreviewPlay(second, ""); err == nil {
		t.Fatal("empty identity accepted")
	}
	b.previewsReset(true)
	if b.PreviewCreate() != 0 || len(b.PreviewPoll(second)) != 0 {
		t.Fatal("inactive scope retained previews")
	}
}
func TestPreviewCancellationRetiresPendingDial(t *testing.T) {
	b := New()
	started := make(chan struct{})
	b.connectTarget = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, liteConnection, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	id := b.PreviewCreate()
	result := make(chan error, 1)
	go func() { _, err := b.PreviewCameras(id, "slow-device", false); result <- err }()
	<-started
	b.PreviewCancel(id)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled dial succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt dial")
	}
	b.PreviewClose(id)
	if b.preview(id) != nil {
		t.Fatal("closed handle retained")
	}
}
func TestPreviewResetCancelsInventory(t *testing.T) {
	b := previewFixture(t)
	id := b.PreviewCreate()
	b.previewsReset(true)
	if _, err := b.PreviewCameras(id, "device", false); err == nil {
		t.Fatal("retired handle allowed inventory")
	}
	b.previewsReset(false)
	next := b.PreviewCreate()
	if next == 0 || next == id {
		t.Fatal("preview handle reused across scopes")
	}
}

func TestPreviewThermalAppUsesExistingConnection(t *testing.T) {
	b := previewFixture(t)
	dials := 0
	dial := b.connectTarget
	b.connectTarget = func(ctx context.Context, selector string) (*grpcclient.AgentConnection, liteConnection, error) {
		dials++
		return dial(ctx, selector)
	}
	id := b.PreviewCreate()
	if _, err := b.PreviewCameras(id, "device", false); err != nil {
		t.Fatal(err)
	}
	rows, err := b.PreviewApps(id)
	if err != nil || len(rows) != 1 || rows[0].GetAppName() != "dev.wendy.thermal-viewer" {
		t.Fatalf("thermal app missing: %v %v", rows, err)
	}
	for i := 0; i < 2; i++ {
		endpoint, err := b.PreviewOpenApp(id, "dev.wendy.thermal-viewer")
		if err != nil || endpoint != "http://preview-device:8080/" {
			t.Fatalf("endpoint: %q %v", endpoint, err)
		}
	}
	if dials != 1 {
		t.Fatalf("opened %d device connections", dials)
	}
	b.PreviewClose(id)
	if _, err := b.PreviewOpenApp(id, "dev.wendy.thermal-viewer"); err == nil {
		t.Fatal("closed preview still opens apps")
	}
}
