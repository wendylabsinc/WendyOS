package services

import (
	"context"
	"sync"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

// newTwoPlaneTestService builds a VideoService that enumerates exactly one
// local camera and whose loopback is a recording double, with a hub already
// running for that camera so a pump joins it instead of opening a device.
func newTwoPlaneTestService(t *testing.T) (*VideoService, *fakeLoopback, *deviceHub) {
	t.Helper()
	video := NewVideoService(context.Background(), zap.NewNop())
	loop := newFakeLoopback()
	video.loopback = loop
	video.globDevices = func() ([]string, error) { return []string{"/dev/video0"}, nil }
	video.hasVideoCapture = func(string) bool { return true }
	video.readDeviceName = func(string) (string, error) { return "test camera", nil }
	video.readStableNames = func() map[string]stableNames { return nil }
	hub, _ := newDefaultHub(t, video, "/dev/video0")

	original := openLoopbackFrameWriter
	t.Cleanup(func() { openLoopbackFrameWriter = original })
	openLoopbackFrameWriter = func(string) (loopbackFrameWriter, error) {
		return newFakeLoopbackWriter(9), nil
	}
	t.Cleanup(video.stopAllTwoPlane)
	return video, loop, hub
}

func (f *fakeLoopback) auxCreatedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.auxCreated)
}

// waitUntilTwoPlane polls until cond holds, failing the test if it never does. The
// two-plane lifecycle runs on its own goroutine, so every assertion about it
// is an assertion about a state the sweep eventually reaches.
func waitUntilTwoPlane(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A source whose pump refuses its frames must not have its node rebuilt on the
// next sweep. Before the refusal was remembered, ensureTwoPlaneForLocalCameras
// recreated the node every minute and on every container lifecycle nudge, so
// the camera pipeline was started and torn down forever.
func TestTwoPlaneRefusedSourceIsNotRecreated(t *testing.T) {
	video, loop, hub := newTwoPlaneTestService(t)
	ctx := context.Background()

	video.SetTwoPlaneContainerConsumers(ctx, []string{"container-a"})
	waitUntilTwoPlane(t, "the first node to be created", func() bool { return loop.auxCreatedCount() == 1 })
	// The pump joins the hub on its own goroutine; a frame produced before it
	// subscribes would simply never reach it.
	waitUntilTwoPlane(t, "the pump to join the hub", func() bool { return hubSubscriberCount(hub) == 2 })

	// A byte-chunked frame: whole access units are what a binding needs, so
	// frameBindableToLoopback refuses this source permanently.
	hub.produce(&videoFrame{
		data:      []byte{0, 0, 0, 1, 0x65, 0x11},
		codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
		auAligned: false,
	})
	waitUntilTwoPlane(t, "the refusal to be recorded", func() bool {
		return video.twoPlaneSourceRefused("v4l2:/dev/video0")
	})

	// The minute sweep and every container lifecycle nudge land here.
	video.ensureTwoPlaneForLocalCameras(ctx)
	video.ensureTwoPlaneForLocalCameras(ctx)
	if got := loop.auxCreatedCount(); got != 1 {
		t.Fatalf("aux nodes created = %d, want 1: a refused source was rebuilt by a later sweep", got)
	}

	// A different entitled container set is a different question, so the
	// refusal stops standing and the source gets one honest attempt again.
	video.SetTwoPlaneContainerConsumers(ctx, []string{"container-b"})
	waitUntilTwoPlane(t, "the node to be rebuilt for a new consumer set", func() bool {
		return loop.auxCreatedCount() == 2
	})
}

// Two concurrent sweeps must not both allocate the same aux node number: the
// loser used to remove the node the winner was pumping into.
func TestTwoPlaneConcurrentSweepsAllocateOneNode(t *testing.T) {
	video, loop, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	video.twoPlaneMu.Lock()
	video.twoPlaneDemand = true
	video.twoPlaneMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			video.ensureTwoPlaneForLocalCameras(ctx)
		}()
	}
	wg.Wait()

	if got := loop.auxCreatedCount(); got != 1 {
		t.Fatalf("aux nodes created = %d, want exactly 1 across concurrent sweeps", got)
	}
	loop.mu.Lock()
	removed := len(loop.auxRemoved)
	loop.mu.Unlock()
	if removed != 0 {
		t.Fatalf("aux nodes removed = %d, want 0: a losing sweep tore down the winner's node", removed)
	}
	if _, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok {
		t.Fatal("no node is registered for the source after the sweeps")
	}
}
