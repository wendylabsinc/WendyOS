package backend

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"testing"
)

type controlsClient struct {
	pb.WendyVideoServiceClient
	value  int32
	writes int
}

func (c *controlsClient) GetCameraControls(context.Context, *pb.GetCameraControlsRequest, ...grpc.CallOption) (*pb.GetCameraControlsResponse, error) {
	return &pb.GetCameraControlsResponse{Controls: []*pb.CameraControl{{Name: "gain", Value: c.value, Minimum: 0, Maximum: 20, Step: 2, Mutable: true}, {Name: "read_only", Value: 1, Maximum: 2}}}, nil
}
func (c *controlsClient) SetCameraControls(_ context.Context, req *pb.SetCameraControlsRequest, _ ...grpc.CallOption) (*pb.SetCameraControlsResponse, error) {
	c.writes++
	if req.Persist {
		panic("settings must not persist implicitly")
	}
	c.value = req.Controls[0].Value
	return &pb.SetCameraControlsResponse{}, nil
}
func TestCameraControlsRejectStaleAndInvalidWrites(t *testing.T) {
	c := &controlsClient{}
	b := New()
	b.conn = &grpcclient.AgentConnection{VideoService: c}
	b.life, b.stop = context.WithCancel(context.Background())
	defer b.stop()
	b.session = 7
	b.cameraDevices = map[uint32]*pb.VideoDevice{2: {Id: 2, StableId: "usb:qa"}}
	for _, tc := range []struct {
		session      uint64
		id           uint32
		stable, name string
		value        int32
	}{
		{0, 2, "usb:qa", "gain", 2}, {8, 2, "usb:qa", "gain", 2}, {7, 3, "usb:qa", "gain", 2}, {7, 2, "other", "gain", 2},
		{7, 2, "usb:qa", "gain", 3}, {7, 2, "usb:qa", "gain", 22}, {7, 2, "usb:qa", "read_only", 2}, {7, 2, "usb:qa", "missing", 2},
	} {
		if _, err := b.CameraControls(tc.session, tc.id, tc.stable, tc.name, tc.value); err == nil {
			t.Fatalf("accepted invalid request: %+v", tc)
		}
	}
	if c.writes != 0 {
		t.Fatal("invalid input reached camera")
	}
	rows, err := b.CameraControls(7, 2, "usb:qa", "gain", 8)
	if err != nil || c.writes != 1 || rows[0].Value != 8 {
		t.Fatalf("write/readback: rows=%v err=%v", rows, err)
	}
}
