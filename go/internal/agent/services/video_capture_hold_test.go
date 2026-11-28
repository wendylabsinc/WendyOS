package services

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
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

const (
	exclusiveCameraPath  = "/dev/video1900"
	otherExclusiveCamera = "/dev/video1901"
)

var exclusiveCameraMode = sharedMode{Width: 640, Height: 480, Fourcc: "H264", FPS: 30, Format: appconfig.CameraFormatNative, MaxFrameBytes: maxFrameBytes}

// exclusiveCameras is a service whose producers are the real ones, each
// capturing from the exclusiveV4L2 at its path. pipeWireLookups counts how
// often a capture went on to look a camera up in PipeWire, which it does only
// for one it takes to be held by another client. Under synctest the cameras'
// clocks are the bubble's, so a test holds one open for as long as it says.
func exclusiveCameras(t *testing.T, paths ...string) (svc *VideoService, cameras map[string]*exclusiveV4L2, pipeWireLookups *atomic.Int32) {
	t.Helper()
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtH264: {{640, 480}}})
	prevSize := captureSizeFor
	captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return 640, 480 }
	t.Cleanup(func() { captureSizeFor = prevSize })
	withFrameRates(t, map[uint32][]uint32{v4l2PixFmtH264: {30}})

	svc = newTestVideoService(nil, nil)
	svc.gstMemo.available = map[string]bool{"jpegdec": true}
	svc.capturePixelFormats = func(string) ([]uint32, bool) { return []uint32{v4l2PixFmtH264}, true }
	cameras = make(map[string]*exclusiveV4L2)
	for _, path := range paths {
		cameras[path] = &exclusiveV4L2{closing: make(chan struct{})}
	}
	svc.captureV4L2 = func(ctx context.Context, emit func([]byte, frameTimestamp) bool, path string, cfg v4l2CaptureConfig) error {
		return cameras[path].capture(ctx, emit, path, cfg)
	}
	pipeWireLookups = new(atomic.Int32)
	svc.findCameraSource = func(context.Context, string) (uint64, bool) {
		pipeWireLookups.Add(1)
		return 0, false
	}
	t.Cleanup(func() {
		for _, camera := range cameras {
			camera.close()
		}
		svc.Shutdown()
	})
	return svc, cameras, pipeWireLookups
}

func exclusiveCamera(t *testing.T) (svc *VideoService, device *exclusiveV4L2, pipeWireLookups *atomic.Int32) {
	t.Helper()
	svc, cameras, pipeWireLookups := exclusiveCameras(t, exclusiveCameraPath)
	return svc, cameras[exclusiveCameraPath], pipeWireLookups
}

// close lets every capture that ended, and every one that ends later, close
// the camera.
func (d *exclusiveV4L2) close() {
	select {
	case <-d.closing:
	default:
		close(d.closing)
	}
}

func joinExclusiveCamera(t *testing.T, svc *VideoService, path string) shareSubscription {
	t.Helper()
	sub, err := svc.joinHubForShare(context.Background(), path, 0, exclusiveCameraMode, noPreset)
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

// keptClosedToOthers fails unless, for as long as the bubble's clock ran while
// the camera was open, exactly captures captures were started on it and none
// was refused as busy. readers are the subscriptions a refused capture would
// have ended.
func (d *exclusiveV4L2) keptClosedToOthers(t *testing.T, captures int32, readers ...shareSubscription) {
	t.Helper()
	synctest.Wait()
	if started, refused := d.started.Load(), d.refused.Load(); started != captures || refused != 0 {
		told := ""
		for _, reader := range readers {
			if !channelStillOpen(reader.Frames()) {
				told = "; the reader was told: " + reader.Err().Error()
			}
		}
		t.Fatalf("while the agent's previous capture still had the camera open: %d capture(s) started, want %d, and %d refused as busy%s", started, captures, refused, told)
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

// awaitsCamera is what a subscription says of its capture waiting for the
// camera's previous capture to close it.
func awaitsCamera(t *testing.T, sub shareSubscription) bool {
	t.Helper()
	awaiter, says := sub.(interface{ AwaitsCamera() bool })
	if !says {
		t.Fatalf("a %T does not say whether its capture waits for the camera", sub)
	}
	return awaiter.AwaitsCamera()
}

// A reader that opens its node while the capture the last reader left is still
// closing the camera gets the camera's frames from a capture that starts once
// the camera is closed, however long that takes: a day here, on the bubble's
// clock.
func TestJoinHubForShare_ACaptureStartsOnlyOnceThePreviousOneHasClosedTheCamera(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, pipeWireLookups := exclusiveCamera(t)

		first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		ownFrameFrom(t, first, "the first reader")
		first.Leave()

		second := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		for _, held := range []time.Duration{time.Second, time.Minute, 24 * time.Hour} {
			time.Sleep(held)
			device.keptClosedToOthers(t, 1, second)
		}

		device.close()
		ownFrameFrom(t, second, "the reader that joined while the camera was closing")
		device.servedByItsOwnCaptures(t, 2, pipeWireLookups)
	})
}

// A reader opens its node while the camera's node is gone and its previous
// capture is still closing it, and the camera is back by the time the capture
// has it. The frames are numbered for the camera the capture has: on from the
// ones before, not from 1 as for the path that named no camera when the hub
// was made.
func TestJoinHubForShare_ACaptureNumbersItsFramesForTheCameraItHas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		base := filepath.Base(exclusiveCameraPath)
		id := camera.Identity{Device: csPort("1-2.1"), Index: "0", Name: "Exclusive"}
		var there atomic.Bool
		there.Store(true)
		svc.cameraIdentity = func(b string) (camera.Identity, error) {
			if b != base || !there.Load() {
				return camera.Identity{}, os.ErrNotExist
			}
			return id, nil
		}
		svc.presentCameras = func() map[string]camera.Identity {
			if !there.Load() {
				return nil
			}
			return map[string]camera.Identity{base: id}
		}
		svc.cameraUSB = nil

		first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		ownFrameFrom(t, first, "the first reader")
		first.Leave()
		synctest.Wait()
		key := svc.sampleSeqKey(exclusiveCameraPath)
		svc.mu.Lock()
		numbered := svc.sampleSeqs[key].Load()
		svc.mu.Unlock()
		if numbered == 0 {
			t.Fatal("the first capture numbered no frame")
		}

		there.Store(false)
		second := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		time.Sleep(time.Second)
		device.keptClosedToOthers(t, 1, second)
		there.Store(true)
		device.close()
		select {
		case frame, open := <-second.Frames():
			if !open {
				t.Fatalf("the second reader got no frame: its capture ended with: %v", second.Err())
			}
			if frame.sampleID != numbered+1 {
				t.Fatalf("the camera's next frame is numbered %d, want %d: it numbered %d before", frame.sampleID, numbered+1, numbered)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the second reader got no frame within 5s")
		}
	})
}

// Two readers that open their nodes while the camera is closing share one
// capture, which starts once the camera is closed.
func TestJoinHubForShare_ReadersJoiningWhileTheCameraClosesShareOneCaptureAfterIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, pipeWireLookups := exclusiveCamera(t)

		first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
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
		time.Sleep(time.Hour)
		device.keptClosedToOthers(t, 1)

		device.close()
		for range 2 {
			reader := <-readers
			if reader.err != nil {
				t.Fatalf("share join: %v", reader.err)
			}
			t.Cleanup(reader.sub.Leave)
			ownFrameFrom(t, reader.sub, "a reader that joined while the camera was closing")
		}
		device.servedByItsOwnCaptures(t, 2, pipeWireLookups)
	})
}

// The turn is one camera's: a capture of another camera starts while this
// one's previous capture is still closing it.
func TestJoinHubForShare_AnotherCamerasCaptureStartsWhileThisOneIsStillClosing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, cameras, pipeWireLookups := exclusiveCameras(t, exclusiveCameraPath, otherExclusiveCamera)
		closing, other := cameras[exclusiveCameraPath], cameras[otherExclusiveCamera]

		first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		ownFrameFrom(t, first, "the first camera's reader")
		first.Leave()
		synctest.Wait()
		if !closing.open.Load() {
			t.Fatal("the first camera is not open: its capture did not stay in its close")
		}

		ownFrameFrom(t, joinExclusiveCamera(t, svc, otherExclusiveCamera), "the other camera's reader, while the first camera was closing")
		if !closing.open.Load() {
			t.Fatal("the first camera was closed before the other camera's capture started, so this showed nothing")
		}
		other.servedByItsOwnCaptures(t, 1, pipeWireLookups)
	})
}

// A reader that leaves while its capture waits for the camera ends that
// capture there: its producer returns at once, and the camera is not opened
// for it when the previous capture closes it.
func TestJoinHubForShare_AReaderThatLeavesWhileItsCaptureWaitsNeverHasTheCameraOpened(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, pipeWireLookups := exclusiveCamera(t)

		first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		ownFrameFrom(t, first, "the first reader")
		first.Leave()

		second := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		time.Sleep(time.Second)
		device.keptClosedToOthers(t, 1, second)
		second.Leave()
		synctest.Wait()
		select {
		case <-second.(*hubShareSubscription).hub.done:
		default:
			t.Fatal("the producer of a hub that ended while it waited for the camera has not returned")
		}

		device.close()
		time.Sleep(time.Hour)
		synctest.Wait()
		device.servedByItsOwnCaptures(t, 1, pipeWireLookups)
	})
}

// A hub that has ended by the time its producer's turn comes takes none, so
// its camera is not opened, and the turn is the next capture's.
func TestHoldCamera_AHubThatHasEndedTakesNoTurn(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	t.Cleanup(svc.Shutdown)
	var wait cameraWait

	ended, end := context.WithCancel(context.Background())
	end()
	if release, held := svc.holdCamera(ended, exclusiveCameraPath, &wait); held {
		release()
		t.Fatal("the producer of a hub that had ended was given the camera")
	}

	next, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, held := svc.holdCamera(next, exclusiveCameraPath, &wait)
	if !held {
		t.Fatal("the turn an ended hub did not take was not there for the next capture")
	}
	release()
}

// A capture that waits says so in the log every ten seconds for as long as it
// waits, and says how long it waited when it has the camera.
func TestJoinHubForShare_ACaptureThatWaitsForTheCameraSaysSoEveryTenSeconds(t *testing.T) {
	const stillHeld, released = "previous capture has not released the camera; the next capture starts when it does", "previous capture released the camera"
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		core, logs := observer.New(zap.InfoLevel)
		svc.logger = zap.New(core)
		said := func(message string, level zapcore.Level) (n int) {
			for _, entry := range logs.FilterMessage(message).All() {
				if entry.Level == level && entry.ContextMap()["device"] == exclusiveCameraPath {
					n++
				}
			}
			return n
		}

		first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		ownFrameFrom(t, first, "the first reader")
		first.Leave()
		second := joinExclusiveCamera(t, svc, exclusiveCameraPath)

		waited := time.Duration(0)
		for _, step := range []struct {
			until time.Duration
			warns int
		}{{9900 * time.Millisecond, 0}, {10100 * time.Millisecond, 1}, {20100 * time.Millisecond, 2}, {65 * time.Second, 6}} {
			time.Sleep(step.until - waited)
			waited = step.until
			synctest.Wait()
			if got := said(stillHeld, zap.WarnLevel); got != step.warns {
				t.Fatalf("after %s of waiting the capture warned %d time(s) that the camera is still held, want %d", waited, got, step.warns)
			}
		}

		device.close()
		ownFrameFrom(t, second, "the reader that joined while the camera was closing")
		entries := logs.FilterMessage(released).All()
		if len(entries) != 1 || entries[0].Level != zap.InfoLevel || entries[0].ContextMap()["waited"] != waited {
			t.Fatalf("said of the release: %v, want once, at info, with the %s waited", entries, waited)
		}
	})
}

// While its capture waits for the camera, a subscription says so, and no
// longer once the capture has the camera.
func TestJoinHubForShare_ASubscriptionSaysItsCaptureWaitsForTheCamera(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)

		first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		ownFrameFrom(t, first, "the first reader")
		if awaitsCamera(t, first) {
			t.Fatal("a subscription whose capture runs says it waits for the camera")
		}
		first.Leave()

		second := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		synctest.Wait()
		if !awaitsCamera(t, second) {
			t.Fatal("a subscription whose capture waits for the camera does not say so")
		}
		device.close()
		ownFrameFrom(t, second, "the reader that joined while the camera was closing")
		if awaitsCamera(t, second) {
			t.Fatal("a subscription whose capture has the camera still says it waits for it")
		}
	})
}

// spAwaitingSub is a subscription that says its capture waits for the camera.
type spAwaitingSub struct {
	*spFakeSub
	awaits atomic.Bool
}

func (s *spAwaitingSub) AwaitsCamera() bool { return s.awaits.Load() }

// A node whose capture waits for the camera's previous capture to close it
// says so where its reader looks, instead of "reading".
func TestSharePump_ANodeWhoseCaptureWaitsForTheCameraSaysSoInsteadOfReading(t *testing.T) {
	const detail = "the camera's previous capture has not closed it yet"
	sub := &spAwaitingSub{spFakeSub: newSpFakeSub()}
	sub.awaits.Store(true)
	j := newSpJoiner(func() (shareSubscription, error) { return sub, nil })
	h := startSharePump(t, j)

	h.node.send(t, 1)
	j.waitJoin(t)
	if state := h.waitState(t, shareWaiting); state.Detail != detail {
		t.Fatalf("state = %+v, want waiting with %q", state, detail)
	}

	sub.awaits.Store(false)
	if state := h.pump.State(); state != (shareState{Phase: shareReading}) {
		t.Fatalf("state once the capture has the camera = %+v, want reading", state)
	}

	sub.awaits.Store(true)
	h.node.send(t, 0)
	spWaitClosed(t, sub.left, "the pump leaving the capture")
	if state := h.waitState(t, shareIdle); state.Detail != "" {
		t.Fatalf("state with no reader = %+v, want idle", state)
	}
}

// A camera a process outside the agent has open is still what it was: looked
// up in PipeWire, and reported to the share's pump as held.
func TestJoinHubForShare_ACameraAnotherProcessHoldsIsStillReportedAsHeld(t *testing.T) {
	svc, device, pipeWireLookups := exclusiveCamera(t)
	device.heldByAnotherProcess.Store(true)

	reader := joinExclusiveCamera(t, svc, exclusiveCameraPath)
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
