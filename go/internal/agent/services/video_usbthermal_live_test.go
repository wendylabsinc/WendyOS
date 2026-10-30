package services

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/rawvideo"
	"github.com/wendylabsinc/wendy/go/internal/agent/usbthermal"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// shortenUSBThermalLivePolls makes the live view's polls fast enough for a test.
func shortenUSBThermalLivePolls(t *testing.T) {
	t.Helper()
	plug, watch, retry := usbThermalPlugPoll, usbThermalWatchPoll, usbThermalSubscribeRetry
	usbThermalPlugPoll, usbThermalWatchPoll, usbThermalSubscribeRetry = 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		usbThermalPlugPoll, usbThermalWatchPoll, usbThermalSubscribeRetry = plug, watch, retry
	})
}

// testPicture is a TC001 Max picture as the driver hands it out: 256x192
// YUY2 with neutral chroma.
func testPicture(grey byte, at time.Time) usbthermal.Frame {
	return usbthermal.Frame{
		Data:   bytes.Repeat([]byte{grey, 0x80}, 256*192),
		Width:  256,
		Height: 192,
		Format: usbthermal.FormatYUY2, Timestamp: at,
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestUSBThermalFormatsAreRawvideoFormats: pictures are passed to the feed
// with their Format untouched, so the two packages must spell formats alike.
func TestUSBThermalFormatsAreRawvideoFormats(t *testing.T) {
	for driver, feed := range map[string]string{
		usbthermal.FormatGRAY8:    rawvideo.FormatGRAY8,
		usbthermal.FormatYUY2:     rawvideo.FormatYUY2,
		usbthermal.FormatGRAY16LE: rawvideo.FormatGRAY16LE,
	} {
		if driver != feed {
			t.Errorf("usbthermal format %q is %q in rawvideo", driver, feed)
		}
	}
	at := time.Unix(1_800_000_000, 0)
	got := usbThermalPicture(testPicture(0x40, at))
	if err := got.Validate(); err != nil || got.Describe() != "256x192 YUY2" || !got.Timestamp.Equal(at) {
		t.Fatalf("converted picture %s (%v), ts %v", got.Describe(), err, got.Timestamp)
	}
}

// TestUSBThermalLiveViewFollowsPlugAndUnplug: the plug poll registers an
// attached camera under the id the driver gave it, with its raw frames left to
// the raw tap, and unregisters it when it goes; a replug registers it again.
func TestUSBThermalLiveViewFollowsPlugAndUnplug(t *testing.T) {
	shortenUSBThermalLivePolls(t)
	svc := newTestVideoService(nil, nil)
	t.Cleanup(svc.Shutdown)
	fake := newFakeUSBThermal()
	listing := fake.listed[0]
	fake.plug() // nothing attached yet
	useFakeUSBThermal(svc, fake)
	svc.usbThermalLive.run(usbThermalPlugPoll)

	id := uint32(usbthermal.IDBandStart)
	time.Sleep(20 * time.Millisecond)
	if _, ok := svc.inProcessCameraByID(id); ok {
		t.Fatal("registered a camera that is not attached")
	}

	fake.plug(listing)
	eventually(t, "the plugged camera is registered", func() bool {
		_, ok := svc.inProcessCameraByID(id)
		return ok
	})
	cam, _ := svc.inProcessCameraByID(id)
	if !cam.rawElsewhere {
		t.Fatal("registered without rawElsewhere: raw requests would be refused instead of reaching the raw tap")
	}
	if fake.pictureSubscriptions() != 0 {
		t.Fatal("an unwatched camera was started")
	}

	fake.plug()
	eventually(t, "the unplugged camera is unregistered", func() bool {
		_, ok := svc.inProcessCameraByID(id)
		return !ok
	})

	fake.plug(listing)
	eventually(t, "the replugged camera is registered again", func() bool {
		again, ok := svc.inProcessCameraByID(id)
		return ok && again != cam
	})
}

// TestUSBThermalLiveViewStreamsOnlyWhileWatched: the camera is subscribed to
// only while its feed has a viewer, every picture reaches the feed unchanged,
// and the subscription ends when the last viewer leaves.
func TestUSBThermalLiveViewStreamsOnlyWhileWatched(t *testing.T) {
	shortenUSBThermalLivePolls(t)
	svc, fake := newUSBThermalTestService(t)
	svc.usbThermalLive.sync()
	cam, ok := svc.inProcessCameraByID(usbthermal.IDBandStart)
	if !ok {
		t.Fatal("sync did not register the attached camera")
	}

	time.Sleep(20 * time.Millisecond)
	if n := fake.pictureSubscriptions(); n != 0 {
		t.Fatalf("%d picture subscription(s) with nobody watching", n)
	}

	frames, unsubscribe := cam.feed.Subscribe() // what the encoder's producer does
	eventually(t, "the bridge subscribes to the camera", func() bool { return fake.pictureSubscriptions() == 1 })
	want := testPicture(0x51, time.Unix(1_800_000_000, 5))
	select {
	case fake.pics <- want:
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge never read a picture")
	}
	select {
	case got := <-frames:
		if !bytes.Equal(got.Data, want.Data) || got.Width != 256 || got.Height != 192 ||
			got.Format != rawvideo.FormatYUY2 || !got.Timestamp.Equal(want.Timestamp) {
			t.Fatalf("feed got %s at %v", got.Describe(), got.Timestamp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the picture never reached the feed")
	}

	unsubscribe()
	eventually(t, "the camera subscription ends with the last viewer", func() bool { return fake.pictureSubscriptions() == 0 })

	// Watched again: subscribed again.
	_, unsubscribe = cam.feed.Subscribe()
	eventually(t, "the bridge resubscribes", func() bool { return fake.pictureSubscriptions() == 1 })
	unsubscribe()
	eventually(t, "the camera subscription ends again", func() bool { return fake.pictureSubscriptions() == 0 })
}

// TestUSBThermalLiveViewRoutesEncodedAndRaw is the routing through
// StreamVideo, on one device id at once: an encoded request is encoded from
// the camera's pictures by the in-process path, a raw request still gets the
// stacked raw frames from the raw tap, and an unplug ends the encoded stream as
// a disconnect. The live view has no plug poll here, so StreamVideo's own sync
// is what registers the camera.
func TestUSBThermalLiveViewRoutesEncodedAndRaw(t *testing.T) {
	shortenUSBThermalLivePolls(t)
	fakeGStreamer(t, "exec /bin/cat") // the "encoded" stream is the pipeline's input
	svc, fake := newUSBThermalTestService(t)
	id := uint32(usbthermal.IDBandStart)
	if _, ok := svc.inProcessCameraByID(id); ok {
		t.Fatal("registered before any StreamVideo call; this test is about StreamVideo's sync")
	}

	ctx, stopCamera := context.WithCancel(context.Background())
	defer stopCamera()
	go func() { // the camera, at its own pace, to whoever subscribes
		at := time.Unix(1_800_000_000, 0)
		for i := 0; ; i++ {
			select {
			case fake.pics <- testPicture(0x60, at.Add(time.Duration(i)*35*time.Millisecond)):
			case <-ctx.Done():
				return
			}
		}
	}()

	encoded := startInProcessViewerFor(svc, &agentpb.StreamVideoRequest{DeviceId: id})
	picture := testPicture(0x60, time.Time{}).Data // 512-byte rows: no stride padding
	got := encoded.waitBytes(t, 2*len(picture))
	if !bytes.Equal(got[:len(picture)], picture) {
		t.Fatal("the encoder's input is not the camera's picture")
	}
	if cam, ok := svc.inProcessCameraByID(id); !ok || !cam.rawElsewhere {
		t.Fatal("StreamVideo did not register the camera for its live view")
	}

	raw, cancelRaw, rawDone := startStream(svc, &agentpb.StreamVideoRequest{DeviceId: id, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW})
	want := testRawFrame(0x4b)
	select {
	case fake.feed <- want:
	case err := <-rawDone:
		t.Fatalf("raw StreamVideo ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("a raw request did not reach the raw tap")
	}
	select {
	case f := <-raw.frames:
		if f.GetCodec() != agentpb.VideoCodec_VIDEO_CODEC_RAW || !bytes.Equal(f.GetData(), want.Data) ||
			f.GetRawFormat().GetHeight() != 384 {
			t.Fatalf("raw frame: codec %v, %d bytes, format %v", f.GetCodec(), len(f.GetData()), f.GetRawFormat())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no raw frame reached the raw viewer")
	}
	cancelRaw()
	<-rawDone

	fake.plug()               // unplugged
	svc.usbThermalLive.sync() // what the plug poll does
	err := encoded.wait(t)
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("encoded stream after the unplug: %v, want Unavailable naming the disconnect", err)
	}
	eventually(t, "the camera subscription ends", func() bool { return fake.pictureSubscriptions() == 0 })

	// Gone from the bus, the id is an ordinary V4L2 number again.
	after := startInProcessViewerFor(svc, &agentpb.StreamVideoRequest{DeviceId: id})
	if err := after.wait(t); status.Code(err) != codes.NotFound {
		t.Fatalf("after the unplug: %v, want NotFound", err)
	}
}

// TestUSBThermalLiveViewRegistrationRefused: when the id is already an
// in-process camera, the live view says so once and leaves the raw tap alone.
func TestUSBThermalLiveViewRegistrationRefused(t *testing.T) {
	svc, fake := newUSBThermalTestService(t)
	unregister, err := svc.RegisterInProcessCamera(usbthermal.IDBandStart, rawvideo.NewFeed())
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	svc.usbThermalLive.sync()
	svc.usbThermalLive.sync()
	svc.usbThermalLive.mu.Lock()
	live, refused := len(svc.usbThermalLive.cams), svc.usbThermalLive.refused[testThermalStableID]
	svc.usbThermalLive.mu.Unlock()
	if live != 0 || !strings.Contains(refused, "already registered") {
		t.Fatalf("live cameras %d, refusal %q", live, refused)
	}
	if fake.pictureSubscriptions() != 0 {
		t.Fatal("a refused camera was subscribed to")
	}
}

// startInProcessViewerFor is startInProcessViewer for any request.
func startInProcessViewerFor(s *VideoService, req *agentpb.StreamVideoRequest) *inProcessViewer {
	ctx, cancel := context.WithCancel(context.Background())
	v := &inProcessViewer{stream: newFakeVideoStream(ctx), cancel: cancel, done: make(chan error, 1)}
	go func() { v.done <- s.StreamVideo(req, v.stream) }()
	return v
}
