package services

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	exclusiveCameraID     = 1900
	exclusiveCameraSource = "v4l2:" + exclusiveCameraPath

	// What each note of a wait says, up to the duration it names.
	waitedFor         = "this recording waited %s for the camera's previous capture to close the camera"
	waitAtTheStart    = waitedFor + "; recording started after it"
	waitInTheEpisode  = waitedFor + " after the capture it read from was replaced; no frames were recorded during that time"
	waitInThePreRoll  = waitedFor + ", until %s before the trigger; its pre-roll holds no frame from before then"
	stillHeldAtTheEnd = "the camera was still held by its previous capture when the recording ended, %s after this recording began to wait for it; no frames were recorded during that time"
	anyWaitNote       = "for the camera's previous capture"
	anyHeldNote       = "the camera was still held by its previous capture"
	startedLate       = "recording started after it"
	armedTooLate      = "armed less than the buffer before the trigger"
	noFirstFrame      = "timed out waiting for the first camera frame"
)

func noteOf(format string, durations ...time.Duration) string {
	args := make([]any, len(durations))
	for i, d := range durations {
		args[i] = d
	}
	return fmt.Sprintf(format, args...)
}

// leftClosing starts and ends a capture of the camera, which then stays in its
// close until device.close.
func leftClosing(t *testing.T, svc *VideoService, device *exclusiveV4L2) {
	t.Helper()
	first := joinExclusiveCamera(t, svc, exclusiveCameraPath)
	ownFrameFrom(t, first, "the first reader")
	first.Leave()
	synctest.Wait()
	if !device.open.Load() {
		t.Fatal("the camera is not open: its previous capture did not stay in its close")
	}
}

// silentWhile makes the camera's captures deliver no frame while silent is
// set, as a camera does that is open and sends nothing.
func silentWhile(svc *VideoService, silent *atomic.Bool) {
	capture := svc.captureV4L2
	svc.captureV4L2 = func(ctx context.Context, emit func([]byte, frameTimestamp) bool, path string, cfg v4l2CaptureConfig) error {
		return capture(ctx, func(frame []byte, stamp frameTimestamp) bool {
			if silent.Load() {
				return ctx.Err() == nil
			}
			return emit(frame, stamp)
		}, path, cfg)
	}
}

func recordingSession(t *testing.T, campaign string) data.CaptureSession {
	t.Helper()
	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	return data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin, CampaignKey: campaign}
}

func exclusiveSource() data.Source {
	return data.Source{ID: exclusiveCameraSource, Kind: "camera", Detail: "exclusive camera"}
}

type startedRecording struct {
	capture *cameraCapture
	err     error
}

// startRecording starts a recording of the camera that is not armed, which
// returns once its first frame is stored.
func startRecording(t *testing.T, svc *VideoService) <-chan startedRecording {
	t.Helper()
	session := recordingSession(t, "")
	started := make(chan startedRecording, 1)
	go func() {
		capture, err := (&cameraDataAdapter{video: svc}).startOne(context.Background(), session, exclusiveSource(), exclusiveCameraID)
		started <- startedRecording{capture, err}
	}()
	return started
}

func (s startedRecording) running(t *testing.T) *cameraCapture {
	t.Helper()
	if s.err != nil {
		t.Fatalf("the recording did not start: %v", s.err)
	}
	return s.capture
}

// takeOver replaces the camera's hub with one at explicit parameters, as
// another episode's capture does, and holds the replacement until the test
// ends.
func takeOver(t *testing.T, svc *VideoService) {
	t.Helper()
	req := &agentpb.StreamVideoRequest{Width: 640, Height: 480, Framerate: 15}
	hub, id, frames, _, _, _, restarted, err := svc.joinHubForCapture(context.Background(), exclusiveCameraPath, req, encodedFrames, noPreset)
	if err != nil || !restarted {
		t.Fatalf("takeover: restarted=%v, %v", restarted, err)
	}
	left := make(chan struct{})
	go func() {
		for {
			select {
			case <-frames:
			case <-left:
				return
			}
		}
	}()
	t.Cleanup(func() {
		hub.unsubscribe(id)
		close(left)
	})
}

// recordedDetail stops a recording and returns its one source's count and
// manifest detail.
func recordedDetail(t *testing.T, capture runningDataCapture) (count uint64, detail string) {
	t.Helper()
	results, err := capture.Stop(context.Background())
	if err != nil || len(results) != 1 {
		t.Fatalf("stopping the recording: %d result(s), %v", len(results), err)
	}
	return results[0].Count, results[0].SourceDetail
}

// wantDetail fails unless the detail holds each of want and none of unwanted.
func wantDetail(t *testing.T, count uint64, detail string, wantFrames bool, want []string, unwanted ...string) {
	t.Helper()
	if (count > 0) != wantFrames {
		t.Errorf("the episode recorded %d frame(s), want frames: %v", count, wantFrames)
	}
	for _, note := range want {
		if !strings.Contains(detail, note) {
			t.Errorf("the detail lacks %q", note)
		}
	}
	for _, note := range unwanted {
		if strings.Contains(detail, note) {
			t.Errorf("the detail says %q", note)
		}
	}
	if t.Failed() {
		t.Fatalf("the detail is %q", detail)
	}
}

// A recording that starts while the camera's previous capture is still closing
// it starts late, and its episode says by how much.
func TestCameraRecording_ACaptureThatWaitedForTheCameraSaysHowLongInItsEpisode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		recording := startRecording(t, svc)
		time.Sleep(1200 * time.Millisecond)
		device.keptClosedToOthers(t, 1)
		device.close()
		capture := (<-recording).running(t)
		time.Sleep(100 * time.Millisecond)

		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, []string{noteOf(waitAtTheStart, 1200*time.Millisecond)}, anyHeldNote, "no frames were recorded")
	})
}

// Only a wait the recording itself sat through is its episode's to report: one
// that ended before the recording joined the capture is not, and a capture
// that never waited has none.
func TestCameraRecording_ACaptureThatDidNotKeepTheRecordingWaitingSaysNothingOfAWait(t *testing.T) {
	for name, heldBefore := range map[string]bool{"the camera was free": false, "the wait ended before the recording joined": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, device, _ := exclusiveCamera(t)
				if heldBefore {
					leftClosing(t, svc, device)
					reader := joinExclusiveCamera(t, svc, exclusiveCameraPath)
					time.Sleep(1200 * time.Millisecond)
					device.close()
					ownFrameFrom(t, reader, "the reader that waited for the camera")
					time.Sleep(time.Second)
				}

				capture := (<-startRecording(t, svc)).running(t)
				time.Sleep(100 * time.Millisecond)
				count, detail := recordedDetail(t, capture)
				wantDetail(t, count, detail, true, nil, anyWaitNote, anyHeldNote)
			})
		})
	}
}

// The wait an episode reports is the recording's own, not its hub's: a
// recording that joins a capture already five seconds into its wait, and has
// the camera a second later, waited one second.
func TestCameraRecording_ARecordingThatJoinsACaptureAlreadyWaitingReportsItsOwnWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)
		reader := joinExclusiveCamera(t, svc, exclusiveCameraPath)
		time.Sleep(5 * time.Second)
		if !awaitsCamera(t, reader) {
			t.Fatal("the reader's capture is not waiting for the camera")
		}

		recording := startRecording(t, svc)
		time.Sleep(time.Second)
		device.close()
		capture := (<-recording).running(t)
		time.Sleep(100 * time.Millisecond)

		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, []string{noteOf(waitAtTheStart, time.Second)}, "6s", anyHeldNote)
	})
}

// A recording whose waiting capture is replaced before it has the camera goes
// on waiting behind the replacement, and its episode reports the whole wait.
func TestCameraRecording_ARecordingWhoseWaitingCaptureIsReplacedReportsTheWholeWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		recording := startRecording(t, svc)
		time.Sleep(2 * time.Second)
		takeOver(t, svc)
		time.Sleep(time.Second)
		device.keptClosedToOthers(t, 1)
		device.close()
		capture := (<-recording).running(t)
		time.Sleep(100 * time.Millisecond)

		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, []string{noteOf(waitAtTheStart, 3*time.Second)}, anyHeldNote, "no frames were recorded")
	})
}

// A wait behind a capture that replaced the one a recording was storing frames
// from is a gap in its episode, not a late start.
func TestCameraRecording_AWaitAfterTheCaptureWasReplacedMidEpisodeIsReportedAsAGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)

		capture := (<-startRecording(t, svc)).running(t)
		time.Sleep(100 * time.Millisecond)
		takeOver(t, svc)
		time.Sleep(1500 * time.Millisecond)
		device.keptClosedToOthers(t, 1)
		device.close()
		time.Sleep(100 * time.Millisecond)

		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, []string{noteOf(waitInTheEpisode, 1500*time.Millisecond), "reattached"}, startedLate, anyHeldNote)
	})
}

// A wait at the start is on record with the capture it was behind, so the
// episode still reports it after that capture was replaced by one that did
// not wait.
func TestCameraRecording_AWaitAtTheStartIsStillReportedAfterTheCaptureWasReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		recording := startRecording(t, svc)
		time.Sleep(1200 * time.Millisecond)
		device.close()
		capture := (<-recording).running(t)
		time.Sleep(100 * time.Millisecond)
		takeOver(t, svc)
		time.Sleep(time.Second)

		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, []string{noteOf(waitAtTheStart, 1200*time.Millisecond), "reattached"}, "no frames were recorded", anyHeldNote)
	})
}

// An armed campaign's recording does not wait for a first frame, so its
// episode can open, and end, while the camera's previous capture still has not
// closed it.
func TestCameraRecording_AnArmedCaptureSaysWhatTheCamerasPreviousCaptureDidToItsEpisode(t *testing.T) {
	const armed, beforeClose = 2 * time.Second, 500 * time.Millisecond
	for name, tc := range map[string]struct {
		closes   bool
		want     string
		unwanted string
	}{
		"the previous capture closes the camera during the episode": {true, noteOf(waitAtTheStart, 2500*time.Millisecond), anyHeldNote},
		"the previous capture never closes the camera":              {false, noteOf(stillHeldAtTheEnd, 7*time.Second), startedLate},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, device, _ := exclusiveCamera(t)
				leftClosing(t, svc, device)

				adapter := &cameraDataAdapter{video: svc}
				sources := []data.Source{exclusiveSource()}
				adapter.Arm("cam", 3*time.Second, sources)
				time.Sleep(armed)
				capture, err := adapter.Start(context.Background(), recordingSession(t, "cam"), sources)
				if err != nil || capture == nil {
					t.Fatalf("the armed campaign's episode did not open while the camera was held: %v", err)
				}
				time.Sleep(beforeClose)
				device.keptClosedToOthers(t, 1)
				if tc.closes {
					device.close()
					time.Sleep(100 * time.Millisecond)
				} else {
					time.Sleep(7*time.Second - armed - beforeClose)
				}

				count, detail := recordedDetail(t, capture)
				wantDetail(t, count, detail, tc.closes, []string{tc.want}, tc.unwanted, "pre-roll holds no frame")
			})
		})
	}
}

// An armed campaign's wait is carried across a replaced capture as a running
// recording's is.
func TestCameraRecording_AnArmedCaptureWhoseWaitingCaptureIsReplacedReportsTheWholeWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		adapter := &cameraDataAdapter{video: svc}
		sources := []data.Source{exclusiveSource()}
		adapter.Arm("cam", 3*time.Second, sources)
		time.Sleep(time.Second)
		takeOver(t, svc)
		time.Sleep(time.Second)
		capture, err := adapter.Start(context.Background(), recordingSession(t, "cam"), sources)
		if err != nil || capture == nil {
			t.Fatalf("the armed campaign's episode did not open while the camera was held: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		device.keptClosedToOthers(t, 1)
		device.close()
		time.Sleep(100 * time.Millisecond)

		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, []string{noteOf(waitAtTheStart, 2500*time.Millisecond)}, anyHeldNote, "no frames were recorded")
	})
}

// A wait that ended before the trigger and inside the pre-roll window cost the
// episode pre-roll: it is reported as that, and the pre-roll's own note does
// not blame a campaign that was armed in time.
func TestCameraRecording_AWaitThatEndedInsideThePreRollWindowIsReportedAsLostPreRoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		adapter := &cameraDataAdapter{video: svc}
		sources := []data.Source{exclusiveSource()}
		adapter.Arm("cam", 3*time.Second, sources)
		time.Sleep(9500 * time.Millisecond)
		device.close()
		time.Sleep(500 * time.Millisecond)
		capture, err := adapter.Start(context.Background(), recordingSession(t, "cam"), sources)
		if err != nil || capture == nil {
			t.Fatalf("the armed campaign's episode did not open: %v", err)
		}
		time.Sleep(100 * time.Millisecond)

		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, []string{noteOf(waitInThePreRoll, 9500*time.Millisecond, 500*time.Millisecond), "camera pre-roll: requested 3s buffer, flushed "},
			armedTooLate, startedLate, anyHeldNote)
	})
}

// A wait that ended before the pre-roll window began kept nothing of the
// episode waiting.
func TestCameraRecording_AWaitThatEndedBeforeThePreRollWindowIsNotTheEpisodesToReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		adapter := &cameraDataAdapter{video: svc}
		sources := []data.Source{exclusiveSource()}
		adapter.Arm("cam", 3*time.Second, sources)
		time.Sleep(time.Second)
		device.close()
		time.Sleep(4 * time.Second)
		capture, err := adapter.Start(context.Background(), recordingSession(t, "cam"), sources)
		if err != nil || capture == nil {
			t.Fatalf("the armed campaign's episode did not open: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		count, detail := recordedDetail(t, capture)
		wantDetail(t, count, detail, true, nil, anyWaitNote, anyHeldNote)
	})
}

// A recording that is not armed waits twenty seconds for its first frame and
// then fails its episode's start. The failure names the camera's previous
// capture when the recording spent most of that time waiting for it, whether
// or not it has the camera by then, and nothing of it otherwise.
func TestCameraRecording_AStartThatTimesOutSaysWhatItWaitedForTheCamerasPreviousCapture(t *testing.T) {
	for name, tc := range map[string]struct {
		held     bool
		released time.Duration
		want     string
	}{
		"the camera is still held": {true, 0,
			noFirstFrame + ": the camera was still held by its previous capture, after this recording had waited 20s for it"},
		"the camera was released just before the limit and sent nothing": {true, 19990 * time.Millisecond,
			noFirstFrame + ": this recording waited 19.99s of those 20s for the camera's previous capture to close the camera"},
		"the camera was released early and sent nothing": {true, 2 * time.Second, noFirstFrame},
		"the camera was free and sent nothing":           {false, 0, noFirstFrame},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, device, _ := exclusiveCamera(t)
				var silent atomic.Bool
				silentWhile(svc, &silent)
				if tc.held {
					leftClosing(t, svc, device)
				}
				silent.Store(true)

				recording := startRecording(t, svc)
				if tc.released > 0 {
					time.Sleep(tc.released)
					device.close()
				}
				got := <-recording
				if got.err == nil || got.err.Error() != tc.want {
					t.Fatalf("the start ended with %v, want %q", got.err, tc.want)
				}
			})
		})
	}
}
