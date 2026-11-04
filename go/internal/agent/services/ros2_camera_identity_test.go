package services

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestROSCameraIdentityNeverFallsBackToTopicOrNode(t *testing.T) {
	cameras := []ros2camera.Camera{
		{ID: 130, StableID: "ros2:graph-a", Topic: "/camera/color/image_raw"},
		{ID: 129, StableID: "ros2:graph-b", Topic: "/camera/color/image_raw"},
	}
	if id, err := resolveROSCameraID(cameras, "ros2:graph-a"); err != nil || id != 130 {
		t.Fatalf("wrong camera: %d %v", id, err)
	}
	if _, err := resolveROSCameraID(cameras[1:], "ros2:graph-a"); status.Code(err) != codes.NotFound {
		t.Fatalf("missing source must not fall back to same topic: %v", err)
	}
	cameras[1].StableID = cameras[0].StableID
	if _, err := resolveROSCameraID(cameras, cameras[0].StableID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ambiguous source must fail: %v", err)
	}
}
