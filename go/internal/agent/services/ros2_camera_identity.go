package services

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Resolve the graph/domain/topic identity against currently active publishers.
// Never fall back to a remembered loopback number or to the topic alone.
func resolveROSCameraID(cameras []ros2camera.Camera, identity string) (uint32, error) {
	var id uint32
	found := false
	for _, camera := range cameras {
		if camera.StableID != identity {
			continue
		}
		if found {
			return 0, status.Error(codes.FailedPrecondition, "ROS 2 camera identity is ambiguous")
		}
		id, found = camera.ID, true
	}
	if !found {
		return 0, status.Error(codes.NotFound, "ROS 2 camera publisher is unavailable")
	}
	return id, nil
}
