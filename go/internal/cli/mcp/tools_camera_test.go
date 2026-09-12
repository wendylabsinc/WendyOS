package mcp

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type cameraListClient struct {
	agentpb.WendyVideoServiceClient
	devices []*agentpb.VideoDevice
}

func (c *cameraListClient) ListVideoDevices(context.Context, *agentpb.ListVideoDevicesRequest, ...grpc.CallOption) (*agentpb.ListVideoDevicesResponse, error) {
	return &agentpb.ListVideoDevicesResponse{Devices: c.devices}, nil
}

func TestCameraList_Online(t *testing.T) {
	for _, tt := range []struct {
		name      string
		transport agentpb.VideoTransport
		online    bool
		want      bool
	}{
		{"legacy USB", agentpb.VideoTransport_VIDEO_TRANSPORT_USB, false, true},
		{"legacy CSI", agentpb.VideoTransport_VIDEO_TRANSPORT_CSI, false, true},
		{"legacy unclassified local", agentpb.VideoTransport_VIDEO_TRANSPORT_UNKNOWN, false, true},
		{"current USB", agentpb.VideoTransport_VIDEO_TRANSPORT_USB, true, true},
		{"offline IP with loopback node", agentpb.VideoTransport_VIDEO_TRANSPORT_IP, false, false},
		{"online IP", agentpb.VideoTransport_VIDEO_TRANSPORT_IP, true, true},
		{"offline ROS2", agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2, false, false},
		{"online ROS2", agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2, true, true},
		{"future transport", agentpb.VideoTransport(99), false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(&config.Config{}, nil)
			srv.SetConn(&grpcclient.AgentConnection{VideoService: &cameraListClient{
				devices: []*agentpb.VideoDevice{{
					Name: "CC HD webcam", Path: "/dev/video0", Transport: tt.transport, Online: tt.online,
				}},
			}})

			result, err := srv.handleCameraList(context.Background(), callToolReq("camera_list", nil))
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("camera_list failed: %v", result.Content)
			}
			for _, rows := range [][]map[string]any{
				listPayload(t, result, "cameras"),
				structuredMap(t, result)["cameras"].([]map[string]any),
			} {
				if len(rows) != 1 {
					t.Fatalf("got %d cameras, want 1", len(rows))
				}
				if got := rows[0]["online"]; got != tt.want {
					t.Errorf("online = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestCameraList_AbsentCamera(t *testing.T) {
	srv := New(&config.Config{}, nil)
	srv.SetConn(&grpcclient.AgentConnection{VideoService: &cameraListClient{}})
	result, err := srv.handleCameraList(context.Background(), callToolReq("camera_list", nil))
	if err != nil {
		t.Fatal(err)
	}
	if cameras := listPayload(t, result, "cameras"); len(cameras) != 0 {
		t.Fatalf("absent camera should not be listed: %v", cameras)
	}
}
