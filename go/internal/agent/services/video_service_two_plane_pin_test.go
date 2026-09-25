package services

import (
	"context"
	"fmt"
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

func TestTwoPlaneStaleStopKeepsAPinnedNode(t *testing.T) {
	video, loop, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	node, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0")
	if err != nil {
		t.Fatal(err)
	}
	waitUntilTwoPlane(t, "the node to be created", func() bool {
		_, ok := video.TwoPlaneNodePath("v4l2:/dev/video0")
		return ok
	})
	// A stale demand decision (released outside the lock) tries to stop, but
	// should find demand still held by the pin and leave the node alone.
	video.stopTwoPlaneIfUnneeded()
	if path, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok || path != node {
		t.Fatalf("stale stop tore down a pinned node: got %q (ok=%v), want %q", path, ok, node)
	}
	if got := loop.auxCreatedCount(); got != 1 {
		t.Fatalf("created %d nodes after stale stop, want 1", got)
	}
	// Now release and confirm the node goes.
	video.ReleaseTwoPlaneNode(ctx, "m-1")
	waitUntilTwoPlane(t, "the node to go once the pin is released", func() bool {
		_, ok := video.TwoPlaneNodePath("v4l2:/dev/video0")
		return !ok
	})
}

func TestTwoPlaneConcurrentPinHandoverKeepsTheNewOwnersNode(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	sourceID := "v4l2:/dev/video0"
	const handovers = 20

	for i := 0; i < handovers; i++ {
		// Owner A acquires a pin.
		_, err := video.AcquireTwoPlaneNode(ctx, "a", sourceID)
		if err != nil {
			t.Fatalf("iteration %d: acquire a failed: %v", i, err)
		}

		// B acquires concurrently with A's release (race condition would destroy
		// the node under B).
		var nodeB string
		var nodeErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			nodeB, nodeErr = video.AcquireTwoPlaneNode(ctx, "b", sourceID)
		}()
		video.ReleaseTwoPlaneNode(ctx, "a")
		<-done

		if nodeErr != nil {
			t.Fatalf("iteration %d: acquire b failed: %v", i, nodeErr)
		}
		if nodeB == "" {
			t.Fatalf("iteration %d: acquire b returned empty path", i)
		}

		// The node B got must still be published; a node torn down under it would
		// fail here.
		if path, ok := video.TwoPlaneNodePath(sourceID); !ok || path != nodeB {
			t.Fatalf("iteration %d: node torn down under B's pin: got %q (ok=%v), want %q", i, path, ok, nodeB)
		}

		// Clean up for the next iteration.
		video.ReleaseTwoPlaneNode(ctx, "b")
		waitUntilTwoPlane(t, fmt.Sprintf("iteration %d: node to be gone", i), func() bool {
			_, ok := video.TwoPlaneNodePath(sourceID)
			return !ok
		})
	}
}
