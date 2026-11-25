package services

import (
	"context"
	"io"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func subscribeEncodedAt(t *testing.T, h *deviceHub, preset appconfig.EncodePreset) int {
	t.Helper()
	id, _, err := h.subscribeAsKind("", encodedFrames, preset)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// One encoder serves every consumer of a camera, at the fastest preset any
// of them states.
func TestDeviceHub_StatedPresetIsTheFastestAmongEncodedSubscribersAsTheyJoinAndLeave(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	changed := h.demandChanged()
	expect := func(want appconfig.EncodePreset, after string) {
		t.Helper()
		expectDemandPoke(t, changed, after)
		if got := h.statedPreset(); got != want {
			t.Fatalf("after %s the hub states %q, want %q", after, got, want)
		}
	}
	if got := h.statedPreset(); got != noPreset {
		t.Fatalf("a hub without subscribers states %q", got)
	}
	viewer := subscribeEncodedAt(t, h, noPreset)
	expect(noPreset, "a viewer that states nothing joins")
	recording := subscribeEncodedAt(t, h, appconfig.EncodePresetMedium)
	expect(appconfig.EncodePresetMedium, "a recording at medium joins")
	app := subscribeEncodedAt(t, h, appconfig.EncodePresetVeryfast)
	expect(appconfig.EncodePresetVeryfast, "an app at veryfast joins")
	second := subscribeEncodedAt(t, h, appconfig.EncodePresetFast)
	expect(appconfig.EncodePresetVeryfast, "an app at fast joins")
	h.unsubscribe(app)
	expect(appconfig.EncodePresetFast, "the app at veryfast leaves")
	h.unsubscribe(second)
	expect(appconfig.EncodePresetMedium, "the app at fast leaves")
	h.unsubscribe(recording)
	expect(noPreset, "the recording leaves")
	h.unsubscribe(viewer)
}

func TestDeviceHub_OnlyAnEncodedSubscribersPresetCounts(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	for _, kind := range []frameKind{rawFrames, pixelFrames, nativeFrames} {
		id, _, err := h.subscribeAsKind("", kind, appconfig.EncodePresetMedium)
		if err != nil {
			t.Fatal(err)
		}
		defer h.unsubscribe(id)
	}
	if got := h.statedPreset(); got != noPreset {
		t.Fatalf("subscribers to no encoded frames state %q", got)
	}
	closed := subscribeEncodedAt(t, h, appconfig.EncodePresetMedium)
	defer h.unsubscribe(closed)
	h.mu.Lock()
	h.subs[closed].closed = true
	h.mu.Unlock()
	if got := h.statedPreset(); got != noPreset {
		t.Fatalf("a subscriber the producer closed still states %q", got)
	}
}

func TestDeviceHub_ResubscribingKeepsTheStatedPreset(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	id, _, err := h.subscribeAsKind(hubHolderEpisodeCapture, nativeFrames, appconfig.EncodePresetFast)
	if err != nil {
		t.Fatal(err)
	}
	newID, _, _, err := h.resubscribeAs(id, encodedFrames)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(newID)
	if got := h.statedPreset(); got != appconfig.EncodePresetFast {
		t.Fatalf("after resubscribing for video the hub states %q, want fast", got)
	}
}

// A frame says at which preset the agent's encoder made it, as the producer
// had reported when the frame was handed on.
func TestDeviceHub_EncodedFramesCarryThePresetTheProducerReported(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	id, ch, _ := h.subscribe()
	defer h.unsubscribe(id)
	native, nativeCh, _ := h.subscribeKind(nativeFrames)
	defer h.unsubscribe(native)
	h264 := func(native bool) *videoFrame {
		return &videoFrame{data: []byte{0, 0, 0, 1, 0x65}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, native: native}
	}
	h.produce(h264(false))
	if f := <-ch; f.encodedAt != noPreset {
		t.Fatalf("a frame from a producer that reported no preset carries %q", f.encodedAt)
	}
	h.setEncoderPreset(appconfig.EncodePresetVeryfast)
	if got := h.encoderPreset(); got != appconfig.EncodePresetVeryfast {
		t.Fatalf("the hub says its encoder runs at %q", got)
	}
	h.produce(h264(false))
	if f := <-ch; f.encodedAt != appconfig.EncodePresetVeryfast {
		t.Fatalf("a frame made at veryfast carries %q", f.encodedAt)
	}
	h.setEncoderPreset(appconfig.EncodePresetMedium)
	h.produce(h264(false))
	if f := <-ch; f.encodedAt != appconfig.EncodePresetMedium {
		t.Fatalf("a frame made after the switch to medium carries %q", f.encodedAt)
	}
	h.produce(h264(true))
	<-nativeCh
	if f := <-ch; f.encodedAt != noPreset {
		t.Fatalf("the camera's own H.264 frame carries %q; no encoder of the agent made it", f.encodedAt)
	}
	h.produce(&videoFrame{data: []byte{0xff, 0xd8}, codec: videoCodecMJPEG, native: true})
	if f := <-nativeCh; f.encodedAt != noPreset {
		t.Fatalf("the camera's own JPEG frame carries %q", f.encodedAt)
	}
}

// Every way into a hub takes the preset with the subscription: a join that
// starts the camera, one that finds it running, and both takeovers.
func TestJoins_StateThePresetWithTheSubscription(t *testing.T) {
	const path = "/dev/video0"
	ctx := context.Background()
	atDefaults := &agentpb.StreamVideoRequest{DeviceId: 0}

	t.Run("a recording that starts the camera, then one that finds it running", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		installFakeProducers(svc)
		hub, first, _, _, _, _, _, err := svc.joinHubForCapture(ctx, path, atDefaults, encodedFrames, appconfig.EncodePresetMedium)
		if err != nil {
			t.Fatal(err)
		}
		defer hub.unsubscribe(first)
		if got := hub.statedPreset(); got != appconfig.EncodePresetMedium {
			t.Fatalf("the hub the recording started states %q, want medium", got)
		}
		same, second, _, err := svc.joinHubKind(ctx, path, atDefaults, encodedFrames, appconfig.EncodePresetVeryfast)
		if err != nil || same != hub {
			t.Fatalf("the second join did not find the camera running: %v", err)
		}
		if got := hub.statedPreset(); got != appconfig.EncodePresetVeryfast {
			t.Fatalf("with a second recording at veryfast the hub states %q", got)
		}
		viewer, viewerID, _, err := svc.joinHub(ctx, path, atDefaults)
		if err != nil || viewer != hub {
			t.Fatal(err)
		}
		defer hub.unsubscribe(viewerID)
		hub.unsubscribe(second)
		if got := hub.statedPreset(); got != appconfig.EncodePresetMedium {
			t.Fatalf("once the recording at veryfast left the hub states %q, want medium", got)
		}
	})

	t.Run("a recording that takes the camera over at its own parameters", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		installFakeProducers(svc)
		oldHub, sensorID, _, err := svc.joinHub(ctx, path, atDefaults)
		if err != nil {
			t.Fatal(err)
		}
		defer oldHub.unsubscribe(sensorID)
		explicit := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
		hub, id, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, path, explicit, encodedFrames, appconfig.EncodePresetFast)
		if err != nil || !restarted || hub == oldHub {
			t.Fatalf("no takeover: restarted=%v err=%v", restarted, err)
		}
		defer hub.unsubscribe(id)
		if got := hub.statedPreset(); got != appconfig.EncodePresetFast {
			t.Fatalf("the hub the takeover installed states %q, want fast", got)
		}
	})

	t.Run("a recording that joins a camera held at other parameters as it runs", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		installFakeProducers(svc)
		held, viewerID, _, err := svc.getOrCreateHub(ctx, path, &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480}, hubHolderStreamClient)
		if err != nil {
			t.Fatal(err)
		}
		defer held.unsubscribe(viewerID)
		other := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1280, Height: 720}
		hub, id, _, err := svc.joinHubKind(ctx, path, other, encodedFrames, appconfig.EncodePresetFast)
		if err != nil || hub != held {
			t.Fatalf("the recording did not join the camera as it runs: %v", err)
		}
		defer hub.unsubscribe(id)
		if got := hub.statedPreset(); got != appconfig.EncodePresetFast {
			t.Fatalf("the hub states %q, want fast", got)
		}
	})

	t.Run("an app's h264 node", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		installFakeProducers(svc)
		shareModeForTest(svc, path, sfH264Mode, "cv-a")
		share, err := svc.joinHubForShare(ctx, path, 0, sfH264Mode, appconfig.EncodePresetFaster)
		if err != nil {
			t.Fatal(err)
		}
		hub := currentHub(svc, path)
		if got := hub.statedPreset(); got != appconfig.EncodePresetFaster {
			t.Fatalf("the hub the node's pump joined states %q, want faster", got)
		}
		share.Leave()
	})

	t.Run("an app's h264 node that takes the camera over from a viewer", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		installFakeProducers(svc)
		oldHub, viewerID, _, err := svc.getOrCreateHub(ctx, path, &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480}, hubHolderStreamClient)
		if err != nil {
			t.Fatal(err)
		}
		defer oldHub.unsubscribe(viewerID)
		shareModeForTest(svc, path, sfH264Mode, "cv-a")
		share, err := svc.joinHubForShare(ctx, path, 0, sfH264Mode, appconfig.EncodePresetSuperfast)
		if err != nil {
			t.Fatal(err)
		}
		defer share.Leave()
		hub := currentHub(svc, path)
		if hub == oldHub {
			t.Fatal("the share did not take the camera over")
		}
		if got := hub.statedPreset(); got != appconfig.EncodePresetSuperfast {
			t.Fatalf("the hub the share's takeover installed states %q, want superfast", got)
		}
	})

	t.Run("a node of pictures states nothing to the encoder", func(t *testing.T) {
		svc := newTestVideoService(nil, nil)
		installFakeProducers(svc)
		shareForTest(svc, path, "cv-a")
		share, err := svc.joinHubForShare(ctx, path, 0, testSharedMode, appconfig.EncodePresetMedium)
		if err != nil {
			t.Fatal(err)
		}
		defer share.Leave()
		if got := currentHub(svc, path).statedPreset(); got != noPreset {
			t.Fatalf("a pump that reads pictures states %q", got)
		}
	})
}

// A feed of JPEG frames is not run in the shape its subscribers need, so its
// encoder runs at the default whatever they state, and the hub and each frame
// say so.
func TestJPEGFeed_RunsItsEncoderAtTheDefaultAndSaysSo(t *testing.T) {
	fakeGStreamer(t, 0)
	gst, err := resolveGSTBinary("gst-launch-1.0")
	if err != nil {
		t.Fatal(err)
	}
	h, cancelHub := newTestHub(t)
	defer cancelHub()
	id, frames, err := h.subscribeAsKind("", encodedFrames, appconfig.EncodePresetMedium)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(id)
	enc := gstEncoderResult{element: "x264enc", codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: true}
	plan, err := planJPEGFeedPipeline(gst, "/dev/video96", &agentpb.StreamVideoRequest{}, enc, 640, 480, "test feed", "the feed has no raw frames")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(plan.args, " "); !strings.Contains(args, " speed-preset=ultrafast ") {
		t.Fatalf("the feed's pipeline is %q, want x264enc at the default", args)
	}
	svc := &VideoService{logger: zap.NewNop()}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- svc.runJPEGFeed(ctx, h.produceCaptured, "/dev/video96", h, enc, plan,
			func(ctx context.Context, _ io.Writer) error { <-ctx.Done(); return ctx.Err() }, "the test feed stopped")
	}()
	defer func() {
		stop()
		<-done
	}()
	if frame := waitForFrame(t, frames, "encoded chunk"); frame.encodedAt != appconfig.DefaultEncodePreset {
		t.Errorf("a frame of the feed says it was encoded at %q, want the default", frame.encodedAt)
	}
	if got := h.encoderPreset(); got != appconfig.DefaultEncodePreset {
		t.Errorf("the hub says its encoder runs at %q, want the default", got)
	}
}
