package services

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

const (
	exclusiveCameraID     = 1900
	exclusiveCameraSource = "v4l2:" + exclusiveCameraPath
	noteOfAWait           = "the camera's previous capture took "
	noteOfACameraHeld     = "the camera was still held by its previous capture"
)

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

func recordingSession(t *testing.T, campaign string) data.CaptureSession {
	t.Helper()
	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	return data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin, CampaignKey: campaign}
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

// A recording that starts while the camera's previous capture is still closing
// it starts late, and its episode says by how much.
func TestCameraRecording_ACaptureThatWaitedForTheCameraSaysHowLongInItsEpisode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		adapter := &cameraDataAdapter{video: svc}
		source := data.Source{ID: exclusiveCameraSource, Kind: "camera", Detail: "exclusive camera"}
		type started struct {
			capture *cameraCapture
			err     error
		}
		recording := make(chan started, 1)
		go func() {
			capture, err := adapter.startOne(context.Background(), recordingSession(t, ""), source, exclusiveCameraID)
			recording <- started{capture, err}
		}()
		time.Sleep(1200 * time.Millisecond)
		device.keptClosedToOthers(t, 1)
		device.close()
		got := <-recording
		if got.err != nil {
			t.Fatalf("the recording did not start once the camera was closed: %v", got.err)
		}
		time.Sleep(100 * time.Millisecond)

		count, detail := recordedDetail(t, got.capture)
		if want := noteOfAWait + "1.2s to close; recording started after it"; count == 0 || !strings.Contains(detail, want) {
			t.Fatalf("the episode recorded %d frame(s) with the detail %q, want frames and %q", count, detail, want)
		}
		if strings.Contains(detail, noteOfACameraHeld) {
			t.Fatalf("the detail of a recording that got the camera says it was still held: %q", detail)
		}
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

				adapter := &cameraDataAdapter{video: svc}
				capture, err := adapter.startOne(context.Background(), recordingSession(t, ""),
					data.Source{ID: exclusiveCameraSource, Kind: "camera", Detail: "exclusive camera"}, exclusiveCameraID)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond)
				count, detail := recordedDetail(t, capture)
				if count == 0 || strings.Contains(detail, noteOfAWait) || strings.Contains(detail, noteOfACameraHeld) {
					t.Fatalf("the episode recorded %d frame(s) with the detail %q, want frames and nothing of a wait", count, detail)
				}
			})
		})
	}
}

// An armed campaign's recording does not wait for a first frame, so its
// episode can open, and end, while the camera's previous capture still has not
// closed it.
func TestCameraRecording_AnArmedCaptureSaysWhatTheCamerasPreviousCaptureDidToItsEpisode(t *testing.T) {
	const armed, beforeClose = 2 * time.Second, 500 * time.Millisecond
	for name, tc := range map[string]struct {
		closes   bool
		want     string
		wantNone string
	}{
		"the previous capture closes the camera during the episode": {true,
			noteOfAWait + "2.5s to close; recording started after it", noteOfACameraHeld},
		"the previous capture never closes the camera": {false,
			noteOfACameraHeld + " when the recording ended, 7s after its capture asked for it; that capture never started", noteOfAWait},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, device, _ := exclusiveCamera(t)
				leftClosing(t, svc, device)

				adapter := &cameraDataAdapter{video: svc}
				sources := []data.Source{{ID: exclusiveCameraSource, Kind: "camera", Detail: "exclusive camera"}}
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
				if (count > 0) != tc.closes || !strings.Contains(detail, tc.want) || strings.Contains(detail, tc.wantNone) {
					t.Fatalf("the episode recorded %d frame(s) with the detail %q, want %q and nothing of %q", count, detail, tc.want, tc.wantNone)
				}
			})
		})
	}
}

// A wait that ended while the campaign was only armed kept no episode waiting.
func TestCameraRecording_AWaitThatEndedBeforeTheTriggerIsNotTheEpisodesToReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		adapter := &cameraDataAdapter{video: svc}
		sources := []data.Source{{ID: exclusiveCameraSource, Kind: "camera", Detail: "exclusive camera"}}
		adapter.Arm("cam", 3*time.Second, sources)
		time.Sleep(time.Second)
		device.close()
		time.Sleep(time.Second)
		capture, err := adapter.Start(context.Background(), recordingSession(t, "cam"), sources)
		if err != nil || capture == nil {
			t.Fatalf("the armed campaign's episode did not open: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		count, detail := recordedDetail(t, capture)
		if count == 0 || strings.Contains(detail, noteOfAWait) || strings.Contains(detail, noteOfACameraHeld) {
			t.Fatalf("the episode recorded %d frame(s) with the detail %q, want frames and nothing of a wait", count, detail)
		}
	})
}

// A recording that is not armed waits for its first frame, and gives up after
// twenty seconds. When that is because the camera's previous capture still
// has not closed it, the failure says so.
func TestCameraRecording_AStartThatTimesOutBehindTheCamerasPreviousCaptureSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, device, _ := exclusiveCamera(t)
		leftClosing(t, svc, device)

		adapter := &cameraDataAdapter{video: svc}
		_, err := adapter.startOne(context.Background(), recordingSession(t, ""),
			data.Source{ID: exclusiveCameraSource, Kind: "camera", Detail: "exclusive camera"}, exclusiveCameraID)
		if want := "timed out waiting for the first camera frame: " + noteOfACameraHeld; err == nil || err.Error() != want {
			t.Fatalf("the start ended with %v, want %q", err, want)
		}
		device.keptClosedToOthers(t, 1)
	})
}
