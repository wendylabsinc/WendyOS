package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// testSharedMode is the shared mode every test here puts on /dev/video0.
var testSharedMode = sharedMode{Width: 1280, Height: 720, Fourcc: "YUYV", BytesPerLine: 2560, FPS: 30}

// shareForTest records that each container holds a share node for path, as
// the share manager would. cv-b goes in before cv-a so the listing is seen to
// sort.
func shareForTest(svc *VideoService, path string, containers ...string) {
	svc.shares.mu.Lock()
	defer svc.shares.mu.Unlock()
	if svc.shares.containers == nil {
		svc.shares.containers = make(map[string]*shareContainer)
	}
	for i, id := range containers {
		svc.shares.containers[id] = &shareContainer{
			app:    "sh.wendy." + id,
			shares: []*shareRecord{{cameraName: "Brio 101", cameraPath: path, mode: testSharedMode, node: 254 - i}},
		}
	}
}

func unshareForTest(svc *VideoService, containers ...string) {
	svc.shares.mu.Lock()
	defer svc.shares.mu.Unlock()
	for _, id := range containers {
		delete(svc.shares.containers, id)
	}
}

func hubParams(h *deviceHub) [3]uint32 { return [3]uint32{h.width, h.height, h.framerate} }

func currentHub(svc *VideoService, path string) *deviceHub {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.hubs[path]
}

// testPixelFrame is one decoded frame at the test's shared mode.
func testPixelFrame() *videoFrame {
	return &videoFrame{
		data:  make([]byte, 16),
		codec: agentpb.VideoCodec_VIDEO_CODEC_RAW,
		rawFmt: &agentpb.RawFormat{
			Width: testSharedMode.Width, Height: testSharedMode.Height,
			Fourcc: testSharedMode.Fourcc, BytesPerLine: testSharedMode.BytesPerLine,
		},
	}
}

func channelClosesWithin(ch <-chan *videoFrame, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func channelStillOpen(ch <-chan *videoFrame) bool {
	select {
	case _, ok := <-ch:
		return ok
	default:
		return true
	}
}

func TestSharedMode_ViewerWithNoParametersJoins(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	shareForTest(svc, "/dev/video0", "cv-b", "cv-a")

	req, err := svc.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatalf("a viewer naming no parameters was refused: %v", err)
	}
	if streamRequestAssertsParams(req) {
		t.Fatalf("request now names parameters: %v", req)
	}
	h, id, _, err := svc.getOrCreateHub(context.Background(), "/dev/video0", req, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(id)
	if got := hubParams(h); got != [3]uint32{} {
		t.Fatalf("hub runs at %v, want defaults", got)
	}
	if fp.count() != 1 {
		t.Fatalf("producers started = %d, want 1", fp.count())
	}
}

func TestSharedMode_ViewerAskingTheSharedModeJoinsAsDefault(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx := context.Background()
	shareForTest(svc, "/dev/video0", "cv-b", "cv-a")

	// The share's capture, at defaults.
	share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	if err != nil {
		t.Fatal(err)
	}
	defer share.Leave()

	asked := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1280, Height: 720, Framerate: 30}
	req, err := svc.applySharedMode("/dev/video0", asked)
	if err != nil {
		t.Fatalf("a viewer asking for exactly the shared mode was refused: %v", err)
	}
	if req == asked {
		t.Fatal("applySharedMode changed the caller's request instead of returning a copy")
	}
	if asked.GetWidth() != 1280 || asked.GetHeight() != 720 || asked.GetFramerate() != 30 {
		t.Fatalf("the caller's request was modified: %v", asked)
	}
	if streamRequestAssertsParams(req) {
		t.Fatalf("admitted request still names parameters: %v", req)
	}
	h, id, _, err := svc.getOrCreateHub(ctx, "/dev/video0", req, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(id)
	if got := hubParams(h); got != [3]uint32{} {
		t.Fatalf("viewer joined a hub at %v, want (0,0,0)", got)
	}
	if fp.count() != 1 {
		t.Fatalf("producers started = %d, want 1: the admitted viewer restarted the camera", fp.count())
	}
	if _, held := h.heldExplicitly(); held {
		t.Fatal("the admitted viewer holds the camera explicitly")
	}
}

func TestSharedMode_ViewerAskingAnotherSizeIsRefusedWithTheMode(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	shareForTest(svc, "/dev/video0", "cv-b", "cv-a")

	_, err := svc.applySharedMode("/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1920, Height: 1080})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v (%v), want InvalidArgument", st.Code(), err)
	}
	const want = "camera /dev/video0 is shared with cv-a, cv-b and runs only at its shared mode, " +
		"1280x720 YUYV, 30 fps; ask for no size or rate, or for exactly that mode"
	if st.Message() != want {
		t.Fatalf("message = %q\nwant      %q", st.Message(), want)
	}

	for _, req := range []*agentpb.StreamVideoRequest{
		{Width: 1280, Height: 720, Framerate: 15},
		{Framerate: 60},
		{Width: 640},
	} {
		if _, err := svc.applySharedMode("/dev/video0", req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: err = %v, want InvalidArgument", req, err)
		}
	}
	if fp.count() != 0 {
		t.Fatalf("producers started = %d, want 0", fp.count())
	}
}

func TestSharedMode_RecordingAtOtherParametersIsRefusedByName(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	shareForTest(svc, "/dev/video0", "cv-b", "cv-a")

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1920, Height: 1080, Framerate: 30}
	_, _, _, _, _, _, _, err := svc.joinHubForCapture(context.Background(), "/dev/video0", req)
	if !errors.Is(err, errCameraShared) {
		t.Fatalf("err = %v, want errCameraShared", err)
	}
	if !isCameraCaptureRefusal(err) {
		t.Fatal("the refusal is not one the campaign records in its manifest")
	}
	for _, want := range []string{"/dev/video0", "cv-a, cv-b", "1280x720 YUYV, 30 fps", "1920x1080 at 30 fps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
	if fp.count() != 0 {
		t.Fatalf("producers started = %d, want 0", fp.count())
	}
}

func TestSharedMode_RecordingWithoutParametersRecordsAtTheSharedMode(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx := context.Background()
	shareForTest(svc, "/dev/video0", "cv-a")

	share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	if err != nil {
		t.Fatal(err)
	}
	defer share.Leave()

	hub, id, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.unsubscribe(id)
	if restarted {
		t.Fatal("a parameter-less recording restarted the shared camera")
	}
	if hub != currentHub(svc, "/dev/video0") || hubParams(hub) != [3]uint32{} {
		t.Fatalf("recording joined a hub at %v, want the share's hub at defaults", hubParams(hub))
	}
	if fp.count() != 1 {
		t.Fatalf("producers started = %d, want 1", fp.count())
	}
	if !channelStillOpen(share.Frames()) {
		t.Fatal("the share's capture ended when the recording joined")
	}
}

// A recording that names exactly the shared mode joins as a default join, and
// reports the mode it records at, so its manifest does not claim it got
// another one.
func TestSharedMode_RecordingAskingTheSharedModeJoinsAsDefault(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx := context.Background()
	shareForTest(svc, "/dev/video0", "cv-a")

	share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	if err != nil {
		t.Fatal(err)
	}
	defer share.Leave()

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1280, Height: 720, Framerate: 30}
	hub, id, _, w, h, fps, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.unsubscribe(id)
	if restarted || fp.count() != 1 {
		t.Fatalf("restarted=%v producers=%d, want no restart", restarted, fp.count())
	}
	if w != 1280 || h != 720 || fps != 30 {
		t.Fatalf("reported %dx%d@%d, want the shared mode 1280x720@30", w, h, fps)
	}
	if _, held := hub.heldExplicitly(); held {
		t.Fatal("the admitted recording holds the camera explicitly")
	}
}

func TestSharedMode_RulesLiftWhenTheLastRecordGoes(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	shareForTest(svc, "/dev/video0", "cv-b", "cv-a")

	other := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1920, Height: 1080}
	unshareForTest(svc, "cv-a")
	_, err := svc.applySharedMode("/dev/video0", other)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("with cv-b still sharing: err = %v, want InvalidArgument", err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "shared with cv-b and") {
		t.Fatalf("refusal %q still names a container that no longer shares", msg)
	}

	unshareForTest(svc, "cv-b")
	if _, _, ok := svc.sharedModeOf("/dev/video0"); ok {
		t.Fatal("camera still reported as shared")
	}
	got, err := svc.applySharedMode("/dev/video0", other)
	if err != nil {
		t.Fatalf("explicit viewer refused after the last share went: %v", err)
	}
	if got != other {
		t.Fatal("an unshared camera's request was not left untouched")
	}

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1920, Height: 1080, Framerate: 30}
	hub, id, _, _, _, _, _, err := svc.joinHubForCapture(context.Background(), "/dev/video0", req)
	if err != nil {
		t.Fatalf("explicit recording refused after the last share went: %v", err)
	}
	defer hub.unsubscribe(id)
	if hubParams(hub) != [3]uint32{1920, 1080, 30} || fp.count() != 1 {
		t.Fatalf("recording hub at %v (producers %d), want 1920x1080@30", hubParams(hub), fp.count())
	}
}

// A zero-value VideoService shares nothing.
func TestSharedMode_ZeroValueServiceSharesNothing(t *testing.T) {
	svc := &VideoService{}
	if _, _, ok := svc.sharedModeOf("/dev/video0"); ok {
		t.Fatal("zero-value service reports a shared camera")
	}
	req := &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}
	if got, err := svc.applySharedMode("/dev/video0", req); err != nil || got != req {
		t.Fatalf("got %v, %v; want the request untouched", got, err)
	}
}

func TestJoinHubForShare_EndsAViewerHoldingAnotherMode(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx := context.Background()

	// A viewer holds the camera at 640x480, and a parameter-less consumer rides along.
	viewerReq := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480}
	oldHub, viewerID, viewerCh, err := svc.getOrCreateHub(ctx, "/dev/video0", viewerReq, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	viewerDone := make(chan error, 1)
	go func() {
		defer oldHub.unsubscribe(viewerID)
		viewerDone <- svc.pumpFrames(newFakeVideoStream(ctx), oldHub, viewerID, viewerCh)
	}()
	_, sensorID, sensorCh, err := svc.joinHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer oldHub.unsubscribe(sensorID)

	// An app starts reading its share node.
	shareForTest(svc, "/dev/video0", "cv-a")
	share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	if err != nil {
		t.Fatalf("share join: %v", err)
	}
	defer share.Leave()

	select {
	case err := <-viewerDone:
		st, _ := status.FromError(err)
		const want = "video stream ended: camera /dev/video0 is now shared with cv-a and runs only at its shared mode, " +
			"1280x720 YUYV, 30 fps; reconnect without a size or rate"
		if st.Code() != codes.InvalidArgument || st.Message() != want {
			t.Fatalf("viewer ended with %v\nwant InvalidArgument %q", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the viewer's stream was not ended")
	}
	if !channelClosesWithin(sensorCh, 2*time.Second) {
		t.Fatal("the parameter-less consumer's stream was not ended")
	}
	if !oldHub.wasRestarted() {
		t.Fatal("old hub not marked restarted, so parameter-less consumers would not rejoin")
	}

	newHub := currentHub(svc, "/dev/video0")
	if newHub == nil || newHub == oldHub {
		t.Fatal("no replacement hub installed")
	}
	if got := hubParams(newHub); got != [3]uint32{} {
		t.Fatalf("replacement hub at %v, want (0,0,0)", got)
	}
	if fp.count() != 2 || streamRequestAssertsParams(fp.request(1)) {
		t.Fatalf("producers = %d, want a second one started at defaults", fp.count())
	}

	newHub.broadcast(testPixelFrame())
	select {
	case f, ok := <-share.Frames():
		if !ok || f == nil {
			t.Fatal("share subscription closed instead of receiving a frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("share subscription received no frame")
	}
	if share.Restarted() || share.Err() != nil {
		t.Fatalf("restarted=%v err=%v on a live share", share.Restarted(), share.Err())
	}
}

func TestJoinHubForShare_WaitsForARecordingHoldingAnotherMode(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx := context.Background()

	// A recording runs at 1920x1080 explicitly before the camera is shared.
	recReq := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1920, Height: 1080, Framerate: 30}
	recHub, recID, recCh, _, _, _, _, err := svc.joinHubForCapture(ctx, "/dev/video0", recReq)
	if err != nil {
		t.Fatal(err)
	}
	shareForTest(svc, "/dev/video0", "cv-a")

	_, err = svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	var wait *shareWaitError
	if !errors.As(err, &wait) {
		recHub.unsubscribe(recID)
		t.Fatalf("err = %v, want *shareWaitError", err)
	}
	if wait.Holder != "episode capture" || wait.Params != "1920x1080 at 30 fps" {
		t.Fatalf("wait = %+v, want episode capture at 1920x1080 at 30 fps", wait)
	}
	if !channelStillOpen(recCh) || recHub.wasRestarted() {
		t.Fatal("the recording was interrupted")
	}
	if hubSubscriberCount(recHub) != 1 {
		t.Fatalf("hub has %d subscribers, want only the recording", hubSubscriberCount(recHub))
	}
	if fp.count() != 1 {
		t.Fatalf("producers started = %d, want 1", fp.count())
	}
	select {
	case <-wait.Done:
		t.Fatal("Done closed while the recording still runs")
	default:
	}

	recHub.unsubscribe(recID)
	select {
	case <-wait.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close once the recording left")
	}

	share, err := svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	if err != nil {
		t.Fatalf("join after the recording left: %v", err)
	}
	defer share.Leave()
	if got := hubParams(currentHub(svc, "/dev/video0")); got != [3]uint32{} {
		t.Fatalf("share hub at %v, want defaults", got)
	}
}

// A viewer and a recording can hold one hub at the same explicit parameters.
// Whichever joined first, the recording is not interrupted.
func TestJoinHubForShare_WaitsWhenARecordingSharesTheHoldWithAViewer(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	_ = installFakeProducers(svc)
	ctx := context.Background()

	params := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1920, Height: 1080, Framerate: 30}
	viewerHub, viewerID, viewerCh, err := svc.getOrCreateHub(ctx, "/dev/video0", params, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer viewerHub.unsubscribe(viewerID)
	recHub, recID, recCh, _, _, _, _, err := svc.joinHubForCapture(ctx, "/dev/video0", params)
	if err != nil {
		t.Fatal(err)
	}
	defer recHub.unsubscribe(recID)
	if recHub != viewerHub {
		t.Fatal("recording did not join the viewer's hub")
	}
	shareForTest(svc, "/dev/video0", "cv-a")

	_, err = svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	var wait *shareWaitError
	if !errors.As(err, &wait) || wait.Holder != hubHolderEpisodeCapture {
		t.Fatalf("err = %v, want a wait on the episode capture", err)
	}
	if !channelStillOpen(recCh) || !channelStillOpen(viewerCh) || recHub.wasRestarted() {
		t.Fatal("the hub was taken over while a recording held it")
	}
}

// A takeover whose old producer does not let go of the camera in time is
// abandoned with the named refusal, promptly, rather than blocking the pump.
func TestJoinHubForShare_AbortsWhenTheCameraDoesNotReleaseInTime(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installSlowProducers(svc, 4*hubTeardownTimeout)
	ctx := context.Background()

	viewerReq := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480}
	oldHub, viewerID, _, err := svc.getOrCreateHub(ctx, "/dev/video0", viewerReq, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer oldHub.unsubscribe(viewerID)
	shareForTest(svc, "/dev/video0", "cv-a")

	started := time.Now()
	_, err = svc.joinHubForShare(ctx, "/dev/video0", 0, testSharedMode)
	if !errors.Is(err, errCameraReleaseTimeout) {
		t.Fatalf("err = %v, want errCameraReleaseTimeout", err)
	}
	if took := time.Since(started); took > 3*hubTeardownTimeout {
		t.Fatalf("join took %s, want it bounded by the %s teardown wait", took, hubTeardownTimeout)
	}
	if fp.count() != 1 {
		t.Fatalf("producers started = %d, want 1: a replacement was started for a camera still held", fp.count())
	}
	if h := currentHub(svc, "/dev/video0"); h != nil && h != oldHub {
		t.Fatal("the abandoned replacement hub was left registered")
	}
}
