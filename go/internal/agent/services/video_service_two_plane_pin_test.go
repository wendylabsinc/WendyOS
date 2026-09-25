package services

import (
	"context"
	"testing"
)

func TestTwoPlanePinSurvivesContainerSync(t *testing.T) {
	video, loop, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	node, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0")
	if err != nil {
		t.Fatal(err)
	}
	if path, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok || path != node {
		t.Fatalf("node %q is not the published one (%q, %v)", node, path, ok)
	}
	// The minute sweep finds no camera-entitled app containers.
	video.SetTwoPlaneContainerConsumers(ctx, nil)
	if _, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok {
		t.Fatal("a container sync tore down the node a model host holds")
	}
	video.ReleaseTwoPlaneNode(ctx, "m-1")
	waitUntilTwoPlane(t, "the node to go once the pin is released", func() bool {
		_, ok := video.TwoPlaneNodePath("v4l2:/dev/video0")
		return !ok
	})
	if got := loop.auxCreatedCount(); got != 1 {
		t.Fatalf("created %d nodes, want 1", got)
	}
}

func TestTwoPlaneAcquireUnknownSourceLeavesNoPin(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	if _, err := video.AcquireTwoPlaneNode(context.Background(), "m-1", "v4l2:/dev/video7"); err == nil {
		t.Fatal("acquired a node for a camera that does not exist")
	}
	video.twoPlaneMu.Lock()
	demand, pins := video.twoPlaneDemand, len(video.twoPlanePinned)
	video.twoPlaneMu.Unlock()
	if demand || pins != 0 {
		t.Fatalf("demand=%v pins=%d after a failed acquire", demand, pins)
	}
}

func TestTwoPlaneContainerDemandSurvivesPinRelease(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	video.SetTwoPlaneContainerConsumers(ctx, []string{"app-a"})
	if _, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0"); err != nil {
		t.Fatal(err)
	}
	video.ReleaseTwoPlaneNode(ctx, "m-1")
	if _, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok {
		t.Fatal("releasing a pin stopped the node an app container still uses")
	}
}
