package services

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// exclusiveV4L2 stands in for the kernel's side of an H.264 camera with the one
// rule of a real one that these tests turn on: while a capture has the camera
// open, the next capture's VIDIOC_S_FMT is refused with EBUSY, whoever asks,
// and captureV4L2Native returns that as errCameraInUse. A capture whose context
// ended keeps the camera open until closing is closed, as one whose
// VIDIOC_STREAMOFF and close have not returned does.
type exclusiveV4L2 struct {
	closing chan struct{}

	open             atomic.Bool
	started, refused atomic.Int32
	// heldByAnotherProcess refuses every capture, as a camera does that a
	// process outside the agent has open.
	heldByAnotherProcess atomic.Bool
}

func (d *exclusiveV4L2) capture(ctx context.Context, emit func([]byte, frameTimestamp) bool, path string, cfg v4l2CaptureConfig) error {
	if cfg.pixelFormat != v4l2PixFmtH264 {
		return cfg.formatUnsupported("not this camera's format")
	}
	if d.heldByAnotherProcess.Load() || !d.open.CompareAndSwap(false, true) {
		d.refused.Add(1)
		return errCameraInUse(path)
	}
	defer d.open.Store(false)
	d.started.Add(1)
	for sequence := uint32(1); ; sequence++ {
		stamp := realtimeFrameTimestamp(time.Now())
		stamp.sequence, stamp.sequenceValid, stamp.auAligned = sequence, true, true
		if !emit(numberedH264(uint64(sequence), true).data, stamp) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Millisecond):
			continue
		}
		break
	}
	<-d.closing
	return ctx.Err()
}

const exclusiveCameraPath = "/dev/video1900"

var exclusiveCameraMode = sharedMode{Width: 640, Height: 480, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes}

// exclusiveCamera is a service whose producers are the real ones, capturing
// from an exclusiveV4L2. pipeWireLookups counts how often a capture went on to
// look the camera up in PipeWire, which it does only for a camera it takes to
// be held by another client.
func exclusiveCamera(t *testing.T) (svc *VideoService, device *exclusiveV4L2, pipeWireLookups *atomic.Int32) {
	t.Helper()
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtH264: {{640, 480}}})
	prevSize := captureSizeFor
	captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return 640, 480 }
	t.Cleanup(func() { captureSizeFor = prevSize })
	withFrameRates(t, map[uint32][]uint32{v4l2PixFmtH264: {30}})

	svc = newTestVideoService(nil, nil)
	svc.gstMemo.available = map[string]bool{"jpegdec": true}
	svc.capturePixelFormats = func(string) ([]uint32, bool) { return []uint32{v4l2PixFmtH264}, true }
	device = &exclusiveV4L2{closing: make(chan struct{})}
	svc.captureV4L2 = device.capture
	pipeWireLookups = new(atomic.Int32)
	svc.findCameraSource = func(context.Context, string) (uint64, bool) {
		pipeWireLookups.Add(1)
		return 0, false
	}
	t.Cleanup(func() {
		select {
		case <-device.closing:
		default:
			close(device.closing)
		}
		svc.Shutdown()
	})
	return svc, device, pipeWireLookups
}

func joinExclusiveCamera(t *testing.T, svc *VideoService) shareSubscription {
	t.Helper()
	sub, err := svc.joinHubForShare(context.Background(), exclusiveCameraPath, 0, exclusiveCameraMode, noPreset)
	if err != nil {
		t.Fatalf("share join: %v", err)
	}
	t.Cleanup(sub.Leave)
	return sub
}

func ownFrameFrom(t *testing.T, sub shareSubscription, what string) {
	t.Helper()
	select {
	case frame, open := <-sub.Frames():
		if !open {
			t.Fatalf("%s got no frame: its capture ended with: %v", what, sub.Err())
		}
		if !frame.native || frame.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 {
			t.Fatalf("%s got codec=%v native=%v, want the camera's own H.264", what, frame.codec, frame.native)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s got no frame within 5s", what)
	}
}

// startsNoCaptureWhileOpen watches, for a time in which a capture started on
// the open camera would have been refused, that none was. readers are the
// subscriptions such a capture would have ended.
func (d *exclusiveV4L2) startsNoCaptureWhileOpen(t *testing.T, readers ...shareSubscription) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := d.refused.Load(); n > 0 {
			told := ""
			for _, reader := range readers {
				if channelClosesWithin(reader.Frames(), time.Second) {
					told = fmt.Sprintf("; the reader was told: %v", reader.Err())
				}
			}
			t.Fatalf("%d capture(s) started on the camera while the agent's previous capture still had it open, and were refused as busy%s", n, told)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (d *exclusiveV4L2) servedByItsOwnCaptures(t *testing.T, want int32, pipeWireLookups *atomic.Int32) {
	t.Helper()
	if started, refused := d.started.Load(), d.refused.Load(); started != want || refused != 0 {
		t.Errorf("captures: %d started, %d refused as busy; want %d started and none refused", started, refused, want)
	}
	if n := pipeWireLookups.Load(); n != 0 {
		t.Errorf("the camera was looked up in PipeWire %d time(s), as one held by another client; only the agent ever held it", n)
	}
}

// A reader that opens its node while the capture the last reader left is still
// closing the camera, however long that takes, gets the camera's frames from a
// capture that starts once the camera is closed.
func TestJoinHubForShare_ACaptureStartsOnlyOnceThePreviousOneHasClosedTheCamera(t *testing.T) {
	svc, device, pipeWireLookups := exclusiveCamera(t)

	first := joinExclusiveCamera(t, svc)
	ownFrameFrom(t, first, "the first reader")
	first.Leave()

	second := joinExclusiveCamera(t, svc)
	// Longer than a join once waited for the camera before it started a
	// capture on it regardless.
	time.Sleep(hubTeardownTimeout)
	device.startsNoCaptureWhileOpen(t, second)

	close(device.closing)
	ownFrameFrom(t, second, "the reader that joined while the camera was closing")
	device.servedByItsOwnCaptures(t, 2, pipeWireLookups)
}

// Two readers that open their nodes while the camera is closing share one
// capture, which starts once the camera is closed.
func TestJoinHubForShare_ReadersJoiningWhileTheCameraClosesShareOneCaptureAfterIt(t *testing.T) {
	svc, device, pipeWireLookups := exclusiveCamera(t)

	first := joinExclusiveCamera(t, svc)
	ownFrameFrom(t, first, "the first reader")
	first.Leave()

	type joined struct {
		sub shareSubscription
		err error
	}
	readers := make(chan joined, 2)
	for range 2 {
		go func() {
			sub, err := svc.joinHubForShare(context.Background(), exclusiveCameraPath, 0, exclusiveCameraMode, noPreset)
			readers <- joined{sub, err}
		}()
	}
	device.startsNoCaptureWhileOpen(t)

	close(device.closing)
	for range 2 {
		reader := <-readers
		if reader.err != nil {
			t.Fatalf("share join: %v", reader.err)
		}
		t.Cleanup(reader.sub.Leave)
		ownFrameFrom(t, reader.sub, "a reader that joined while the camera was closing")
	}
	device.servedByItsOwnCaptures(t, 2, pipeWireLookups)
}

// A camera a process outside the agent has open is still what it was: looked
// up in PipeWire, and reported to the share's pump as held.
func TestJoinHubForShare_ACameraAnotherProcessHoldsIsStillReportedAsHeld(t *testing.T) {
	svc, device, pipeWireLookups := exclusiveCamera(t)
	device.heldByAnotherProcess.Store(true)

	reader := joinExclusiveCamera(t, svc)
	if !channelClosesWithin(reader.Frames(), 10*time.Second) {
		t.Fatal("the capture of a camera another process holds did not end")
	}
	if err := reader.Err(); !isCameraInUse(err) || !cameraHeldElsewhere(err) {
		t.Fatalf("the capture ended with %v, want the camera reported as in use by another application", err)
	}
	if n := pipeWireLookups.Load(); n != 1 {
		t.Fatalf("the camera was looked up in PipeWire %d time(s), want once", n)
	}
}
