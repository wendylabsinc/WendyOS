package services

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/agent/framesource/framesourcetest"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// fakeBridgeWriter records what the pump writes to the bridge node.
type fakeBridgeWriter struct {
	mu     sync.Mutex
	frames []ros2camera.Frame
	closed bool
}

func (w *fakeBridgeWriter) WriteFrame(f ros2camera.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = append(w.frames, f)
	return nil
}

func (w *fakeBridgeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *fakeBridgeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.frames)
}

// newBridgedTestService wires a calibrated service whose RealSense capture is
// bridged through fakes: the loopback allocates node auxNext, the writer
// records frames, and "/dev/video4" is the colour node.
func newBridgedTestService(t *testing.T, fake *framesourcetest.Fake) (*CalibratedFrameService, *fakeLoopback, *fakeBridgeWriter) {
	t.Helper()
	prevNodes := framesource.RealSenseNodePaths
	framesource.RealSenseNodePaths = func() []string { return []string{"/dev/video0", "/dev/video4"} }
	t.Cleanup(func() { framesource.RealSenseNodePaths = prevNodes })
	t.Cleanup(func() { cameraOwners.release(fake.Desc().GetSource()) })

	fake.Desc().Kind = framesource.KindRealSense
	loop := newFakeLoopback()
	loop.auxNext = 250
	writer := &fakeBridgeWriter{}
	bridge := newRealSenseBridge(zap.NewNop(), loop)
	bridge.writerFor = func(string) ros2camera.CameraWriter { return writer }
	bridge.colourNodeFor = func([]string) string { return "/dev/video4" }

	svc := newCalibratedService(fake)
	svc.bridge = bridge
	return svc, loop, writer
}

// While a RealSense capture runs, its colour plane reaches the bridge node and
// StreamVideo resolution for the colour node lands there; when the capture
// ends, the node, the redirect and the writer are all gone.
func TestRealSenseBridge_ServesStreamVideoFromCapture(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48, alignedDepth)
	svc, loop, writer := newBridgedTestService(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, newStreamStub(ctx))
	waitForSubscribers(t, svc, "fake:1", 1)

	fake.Push(framesourcetest.Frame(1, 64, 48))
	waitForCond(t, "the pump to write the colour plane", func() bool { return writer.count() >= 1 })

	got := writer.frames[0]
	if got.Codec != ros2camera.CodecBGR24 || got.Width != 64 || got.Height != 48 {
		t.Fatalf("bridge wrote %+v, want 64x48 BGR24", got)
	}
	if len(got.Data) != 64*3*48 {
		t.Fatalf("bridge wrote %d bytes, want the colour plane's %d", len(got.Data), 64*3*48)
	}

	if bridged, ok := cameraOwners.redirect("/dev/video4"); !ok || bridged != "/dev/video250" {
		t.Fatalf("redirect = %q, %v; want /dev/video250", bridged, ok)
	}
	src, err := (&VideoService{}).resolveSource(4)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if src.path != "/dev/video250" || src.key != "/dev/video250" {
		t.Fatalf("resolveSource(4) = %+v, want the bridge node", src)
	}

	svc.Shutdown()
	<-errCh
	if _, ok := cameraOwners.redirect("/dev/video4"); ok {
		t.Fatal("capture ended but the redirect still stands")
	}
	if len(loop.auxRemoved) == 0 || loop.auxRemoved[0] != 250 {
		t.Fatalf("auxRemoved = %v, want the bridge node 250", loop.auxRemoved)
	}
	if !writer.closed {
		t.Fatal("capture ended but the node writer was not closed")
	}
}

// Without a colour node to redirect there is no bridge, and the capture still
// runs -- the documented lockout, not a failure.
func TestRealSenseBridge_NoColourNodeMeansNoBridge(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48)
	svc, loop, _ := newBridgedTestService(t, fake)
	svc.bridge.colourNodeFor = func([]string) string { return "" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, newStreamStub(ctx))
	waitForSubscribers(t, svc, "fake:1", 1)

	if _, ok := cameraOwners.redirect("/dev/video4"); ok {
		t.Fatal("a bridge redirect exists although no colour node was found")
	}
	if len(loop.auxCreated) != 0 {
		t.Fatalf("auxCreated = %v, want no bridge node", loop.auxCreated)
	}
	svc.Shutdown()
	<-errCh
}

// The tap never keeps a hub alive: with the pump attached and the last real
// subscriber gone, the capture drains as it always did.
func TestRealSenseBridge_TapDoesNotKeepTheHubAlive(t *testing.T) {
	fake := framesourcetest.NewFake("fake:1", 64, 48)
	svc, _, _ := newBridgedTestService(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, newStreamStub(ctx))
	waitForSubscribers(t, svc, "fake:1", 1)

	cancel() // the only real subscriber leaves
	waitForCond(t, "the subscriber to unsubscribe", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		hub, ok := svc.hubs["fake:1"]
		if !ok {
			return true
		}
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.subs) == 0 || hub.finished
	})
	// The next publish finds nobody and ends the hub, bridge tap or not.
	fake.Push(framesourcetest.Frame(2, 64, 48))
	waitForCond(t, "the hub to drain", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		_, ok := svc.hubs["fake:1"]
		return !ok
	})
	<-errCh
	svc.Shutdown()
}
