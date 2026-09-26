package services

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// feedTwoPlaneFrames stands in for a camera whose stream carries frame
// identity: it produces a whole H.264 access unit on hub every millisecond
// until the test ends, so a pump that joins writes its first frame at once.
func feedTwoPlaneFrames(t *testing.T, hub *deviceHub) {
	t.Helper()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				hub.produce(wholeAccessUnit())
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

// wholeAccessUnit is a frame the pump can bind to a loopback sequence.
func wholeAccessUnit() *videoFrame {
	return &videoFrame{data: []byte{0, 0, 0, 1, 0x65, 0x11}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}
}

// twoPlanePins reports the path's demand and how many owners pin it.
func twoPlanePins(video *VideoService) (bool, int) {
	video.twoPlaneMu.Lock()
	defer video.twoPlaneMu.Unlock()
	return video.twoPlaneDemand, len(video.twoPlanePinned)
}

func TestTwoPlanePinSurvivesContainerSync(t *testing.T) {
	video, loop, hub := newTwoPlaneTestService(t)
	feedTwoPlaneFrames(t, hub)
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
	if demand, pins := twoPlanePins(video); demand || pins != 0 {
		t.Fatalf("demand=%v pins=%d after a failed acquire", demand, pins)
	}
}

// Acquire returns once the pump has written a frame to the node: that is the
// pump's verdict that the source's stream carries frame identity.
func TestTwoPlaneAcquireWaitsForTheFirstFrame(t *testing.T) {
	video, _, hub := newTwoPlaneTestService(t)
	var node string
	acquired := make(chan error, 1)
	go func() {
		var err error
		node, err = video.AcquireTwoPlaneNode(context.Background(), "m-1", "v4l2:/dev/video0")
		acquired <- err
	}()
	waitUntilTwoPlane(t, "the pump to join the hub", func() bool { return hubSubscriberCount(hub) == 2 })
	select {
	case err := <-acquired:
		t.Fatalf("Acquire returned (%v) before any frame reached the node", err)
	case <-time.After(20 * time.Millisecond):
	}
	hub.produce(wholeAccessUnit())
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return once a frame reached the node")
	}
	if path, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok || path != node {
		t.Fatalf("Acquire returned %q; the published node is %q (%v)", node, path, ok)
	}
}

// A source whose pump refuses its first frame, because its stream carries no
// frame identity, fails Acquire with the refusal and leaves no pin behind.
func TestTwoPlaneAcquireOfARefusedSourceLeavesNoPin(t *testing.T) {
	video, _, hub := newTwoPlaneTestService(t)
	acquired := make(chan error, 1)
	go func() {
		_, err := video.AcquireTwoPlaneNode(context.Background(), "m-1", "v4l2:/dev/video0")
		acquired <- err
	}()
	waitUntilTwoPlane(t, "the pump to join the hub", func() bool { return hubSubscriberCount(hub) == 2 })
	// A byte-chunked frame, as GStreamer delivers from most USB webcams.
	hub.produce(&videoFrame{data: []byte{0, 0, 0, 1, 0x65, 0x11}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: false})
	select {
	case err := <-acquired:
		if !errors.Is(err, errSourceNotBindable) {
			t.Fatalf("Acquire = %v, want the pump's refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return after the pump refused the source")
	}
	if demand, pins := twoPlanePins(video); demand || pins != 0 {
		t.Fatalf("demand=%v pins=%d after a refused acquire", demand, pins)
	}
	if !video.twoPlaneSourceRefused("v4l2:/dev/video0") {
		t.Fatal("the refusal was not remembered")
	}
}

// Acquire gives up when its context ends before any frame arrives, and
// releases its pin, so the unused data path stops.
func TestTwoPlaneAcquireGivesUpWhenItsContextEnds(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire of a camera sending nothing = %v, want its context's deadline", err)
	}
	if demand, pins := twoPlanePins(video); demand || pins != 0 {
		t.Fatalf("demand=%v pins=%d after an acquire that gave up", demand, pins)
	}
	waitUntilTwoPlane(t, "the unused node to go", func() bool {
		_, ok := video.TwoPlaneNodePath("v4l2:/dev/video0")
		return !ok
	})
}

func TestTwoPlaneContainerDemandSurvivesPinRelease(t *testing.T) {
	video, _, hub := newTwoPlaneTestService(t)
	feedTwoPlaneFrames(t, hub)
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
	video, loop, hub := newTwoPlaneTestService(t)
	feedTwoPlaneFrames(t, hub)
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
	video, _, hub := newTwoPlaneTestService(t)
	feedTwoPlaneFrames(t, hub)
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
