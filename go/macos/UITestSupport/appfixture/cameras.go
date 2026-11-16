package main

import (
	"context"
	_ "embed"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"sync"
	"time"
)

// Synthetic testsrc2 640x480 frame, encoded with ffmpeg/libx264 (ultrafast, zerolatency).
// Repeated IDRs exercise the same H.264 bridge/decoder as a physical camera.
//
//go:embed camera.h264
var cameraFrame []byte

type cameraService struct {
	pb.UnimplementedWendyVideoServiceServer
	mu   sync.Mutex
	gain int32
}

func (*cameraService) ListVideoDevices(context.Context, *pb.ListVideoDevicesRequest) (*pb.ListVideoDevicesResponse, error) {
	return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{{Id: 1, Name: "QA Camera", StableId: "qa:camera", PixelFormats: []string{"MJPG"}}}}, nil
}
func (*cameraService) StreamVideo(_ *pb.StreamVideoRequest, stream grpc.ServerStreamingServer[pb.VideoFrame]) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := stream.Send(&pb.VideoFrame{Data: cameraFrame, Codec: pb.VideoCodec_VIDEO_CODEC_H264}); err != nil {
			return err
		}
		select {
		case <-ticker.C:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}
func (c *cameraService) GetCameraControls(context.Context, *pb.GetCameraControlsRequest) (*pb.GetCameraControlsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &pb.GetCameraControlsResponse{Controls: []*pb.CameraControl{{Name: "gain", Value: c.gain, Minimum: 0, Maximum: 20, Step: 2, Mutable: true}, {Name: "temperature", Value: 25, Minimum: 0, Maximum: 100}}}, nil
}
func (c *cameraService) SetCameraControls(_ context.Context, r *pb.SetCameraControlsRequest) (*pb.SetCameraControlsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gain = r.Controls[0].Value
	return &pb.SetCameraControlsResponse{}, nil
}
