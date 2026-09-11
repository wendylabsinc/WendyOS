package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeProducers records every producer the service started and performs
// runProducer's teardown duties when a producer's context ends, without
// opening any device. Tests broadcast frames on the recorded hubs by hand.
type fakeProducers struct {
	mu      sync.Mutex
	started []*agentpb.StreamVideoRequest
	hubs    []*deviceHub
}

func installFakeProducers(svc *VideoService) *fakeProducers {
	fp := &fakeProducers{}
	svc.startProducer = func(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) {
		fp.mu.Lock()
		fp.started = append(fp.started, req)
		fp.hubs = append(fp.hubs, h)
		fp.mu.Unlock()
		go func() {
			<-ctx.Done()
			svc.mu.Lock()
			if svc.hubs[path] == h {
				delete(svc.hubs, path)
			}
			svc.mu.Unlock()
			h.mu.Lock()
			for _, sub := range h.subs {
				if !sub.closed {
					sub.closed = true
					close(sub.ch)
				}
			}
			h.mu.Unlock()
			close(h.done)
		}()
	}
	return fp
}

func (fp *fakeProducers) count() int {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return len(fp.hubs)
}

func (fp *fakeProducers) hub(i int) *deviceHub {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.hubs[i]
}

func (fp *fakeProducers) request(i int) *agentpb.StreamVideoRequest {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.started[i]
}

// Episode capture with explicit campaign parameters takes over a hub running
// at defaults for parameter-less subscribers: the producer is restarted at the
// campaign's parameters and the old subscriber's stream ends, marked as a
// restart so the subscriber reattaches rather than giving up.
func TestCaptureTakesOverDefaultedHub(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx := context.Background()

	// A parameter-less subscriber (a model app on the sensor socket) opens the
	// camera first; the producer runs at its defaults.
	oldHub, sensorID, sensorCh, err := svc.joinHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer oldHub.unsubscribe(sensorID)

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	newHub, captureID, _, w, h, fps, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req)
	if err != nil {
		t.Fatalf("takeover failed: %v", err)
	}
	defer newHub.unsubscribe(captureID)
	if !restarted {
		t.Fatal("takeover not reported as a restart")
	}
	if w != 640 || h != 480 || fps != 15 {
		t.Fatalf("achieved parameters = %dx%d@%d, want the campaign's 640x480@15", w, h, fps)
	}
	if newHub == oldHub {
		t.Fatal("takeover reused the defaulted hub instead of replacing it")
	}
	if fp.count() != 2 {
		t.Fatalf("started %d producers, want 2 (default, then campaign)", fp.count())
	}
	got := fp.request(1)
	if got.GetWidth() != 640 || got.GetHeight() != 480 || got.GetFramerate() != 15 {
		t.Fatalf("replacement producer started with %dx%d@%d, want 640x480@15", got.GetWidth(), got.GetHeight(), got.GetFramerate())
	}

	// The old subscriber's stream ends (no mid-stream splice) and the hub says
	// why, so a parameter-less subscriber knows to reattach.
	select {
	case _, ok := <-sensorCh:
		if ok {
			t.Fatal("old subscriber received a frame after the takeover")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old subscriber channel not closed by the takeover")
	}
	if !oldHub.wasRestarted() {
		t.Fatal("old hub not marked restarted")
	}
	if err := oldHub.terminalErr(); err != nil {
		t.Fatalf("takeover recorded a terminal error on the old hub: %v", err)
	}

	// A reattaching parameter-less subscriber lands on the replacement hub.
	rejoined, rejoinedID, _, err := svc.joinHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer rejoined.unsubscribe(rejoinedID)
	if rejoined != newHub {
		t.Fatal("reattaching subscriber did not join the replacement hub")
	}
}

// A hub whose parameters a consumer asserted explicitly is never restarted:
// the campaign is refused with the named error, and the holder keeps its
// stream.
func TestCaptureRefusedWhenHubHeldExplicitly(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	_ = installFakeProducers(svc)
	ctx := context.Background()

	viewerReq := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1280, Height: 720, Framerate: 30}
	viewerHub, viewerID, viewerCh, err := svc.getOrCreateHub(ctx, "/dev/video0", viewerReq, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer viewerHub.unsubscribe(viewerID)

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	_, _, _, _, _, _, _, err = svc.joinHubForCapture(ctx, "/dev/video0", req)
	if !errors.Is(err, errCameraHeldExplicitly) {
		t.Fatalf("expected errCameraHeldExplicitly, got %v", err)
	}
	for _, want := range []string{hubHolderStreamClient, "1280x720 at 30 fps", "640x480 at 15 fps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
	if viewerHub.wasRestarted() {
		t.Fatal("refusal restarted the held hub")
	}
	select {
	case _, ok := <-viewerCh:
		if !ok {
			t.Fatal("holder's stream was closed by the refused takeover")
		}
	default:
	}
}

// Explicit possession follows the subscribers, not the hub's history: once the
// explicit consumer leaves and only parameter-less subscribers remain, the
// hub's parameters are defaulted again and a campaign may take it over.
func TestTakeoverAllowedAfterExplicitHolderLeaves(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	_ = installFakeProducers(svc)
	ctx := context.Background()

	viewerReq := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1280, Height: 720, Framerate: 30}
	viewerHub, viewerID, _, err := svc.getOrCreateHub(ctx, "/dev/video0", viewerReq, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	// A parameter-less subscriber joins the viewer's hub and keeps it alive.
	sensorHub, sensorID, _, err := svc.joinHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer sensorHub.unsubscribe(sensorID)
	if sensorHub != viewerHub {
		t.Fatal("parameter-less subscriber did not share the viewer's hub")
	}
	viewerHub.unsubscribe(viewerID)

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	newHub, captureID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req)
	if err != nil {
		t.Fatalf("takeover after the explicit holder left failed: %v", err)
	}
	defer newHub.unsubscribe(captureID)
	if !restarted {
		t.Fatal("takeover not reported as a restart")
	}
}

// A capture takeover through the adapter records the takeover note in the
// manifest alongside the parameters it achieved.
func TestStartOneNotesTakeoverOfDefaultedHub(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0"}, nil },
		func(string) (string, error) { return "USB Cam", nil },
	)
	fp := installFakeProducers(svc)
	ctx := context.Background()

	// The defaulted hub a model app would have created.
	oldHub, sensorID, _, err := svc.joinHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer oldHub.unsubscribe(sensorID)

	// Feed whatever producer is newest so the capture sees its first frame.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				fp.hub(fp.count() - 1).broadcast(testH264Frame(testRAUFrame))
			}
		}
	}()

	adapter := &cameraDataAdapter{video: svc}
	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin}
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Detail: "USB Cam", Capture: &data.SourceCapture{Rate: 15, MaxResolution: "640x480"}}
	capture, err := adapter.startOne(ctx, session, source, 0)
	if err != nil {
		t.Fatalf("takeover through the adapter failed: %v", err)
	}
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	detail := results[0].SourceDetail
	if !strings.Contains(detail, "took over the camera") || !strings.Contains(detail, "640x480 at 15 fps") {
		t.Fatalf("takeover note not recorded: %q", detail)
	}
	if results[0].Count == 0 {
		t.Fatal("no frames captured after the takeover")
	}
	if got := fp.request(fp.count() - 1); got.GetWidth() != 640 || got.GetHeight() != 480 || got.GetFramerate() != 15 {
		t.Fatalf("campaign producer started with %dx%d@%d, want 640x480@15", got.GetWidth(), got.GetHeight(), got.GetFramerate())
	}
}

// A parameter-less episode capture whose hub is taken over by a concurrent
// campaign reattaches and keeps recording, starting a fresh segment rather
// than splicing the new stream into the old one.
func TestParameterlessCaptureReattachesAcrossTakeover(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0"}, nil },
		func(string) (string, error) { return "USB Cam", nil },
	)
	fp := installFakeProducers(svc)
	ctx := context.Background()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				if fp.count() > 0 {
					fp.hub(fp.count() - 1).broadcast(testH264Frame(testRAUFrame))
				}
			}
		}
	}()

	adapter := &cameraDataAdapter{video: svc}
	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin}
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Detail: "USB Cam"}
	capture, err := adapter.startOne(ctx, session, source, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A concurrent campaign takes the camera over at explicit parameters.
	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	newHub, captureID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req)
	if err != nil || !restarted {
		t.Fatalf("takeover failed: restarted=%v err=%v", restarted, err)
	}
	defer newHub.unsubscribe(captureID)

	// The capture reattaches as one more subscriber of the replacement hub
	// (alongside the campaign subscription made above).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && hubSubscriberCount(newHub) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if hubSubscriberCount(newHub) < 2 {
		t.Fatal("capture never reattached to the replacement hub")
	}

	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	if !strings.Contains(results[0].SourceDetail, "reattached") {
		t.Fatalf("reattach note not recorded: %q", results[0].SourceDetail)
	}
	if results[0].Count == 0 {
		t.Fatal("no frames captured")
	}
}

func hubSubscriberCount(h *deviceHub) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
