package services

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// encodedAt is an access unit the agent's encoder made at preset, as the hub
// hands it on.
func encodedAt(preset appconfig.EncodePreset, sampleID uint64, at time.Duration, payload []byte) *videoFrame {
	frame := h264At(sampleID, at, payload)
	frame.encodedAt = preset
	return frame
}

func storePresetNote(t *testing.T, c *cameraCapture) string {
	t.Helper()
	var said []string
	for _, note := range c.storeNotes() {
		if strings.Contains(note, "preset") {
			said = append(said, note)
		}
	}
	if len(said) > 1 {
		t.Fatalf("the recording speaks of its preset %d times: %q", len(said), said)
	}
	return strings.Join(said, "")
}

// A recording whose video was not encoded at the preset its source states
// says so in the source's detail: one encoder serves every consumer of the
// camera, and not every camera's video comes from an encoder that takes one.
func TestRecordingSaysWhichVideoFramesWereNotEncodedAtTheStatedPreset(t *testing.T) {
	const veryfast, ultrafast, superfast = appconfig.EncodePresetVeryfast, appconfig.EncodePresetUltrafast, appconfig.EncodePresetSuperfast
	for name, tc := range map[string]struct {
		stated string
		stored []appconfig.EncodePreset
		want   string
	}{
		"every frame at the stated preset": {"veryfast", []appconfig.EncodePreset{veryfast, veryfast, veryfast}, ""},
		"no preset stated":                 {"", []appconfig.EncodePreset{ultrafast, ultrafast, noPreset}, ""},
		"another consumer stated a faster one for a while": {"veryfast", []appconfig.EncodePreset{veryfast, ultrafast, ultrafast, veryfast},
			"preset: veryfast was requested, but 2 video frame(s) were encoded at ultrafast"},
		"two other presets, fastest first": {"veryfast", []appconfig.EncodePreset{superfast, ultrafast, ultrafast, ultrafast, veryfast},
			"preset: veryfast was requested, but 3 video frame(s) were encoded at ultrafast and 1 at superfast"},
		"an encoder that takes no preset, or the camera's own video": {"veryfast", []appconfig.EncodePreset{noPreset, noPreset},
			"preset: veryfast was requested, but 2 video frame(s) were not encoded at a preset: they are the camera's own or come from an encoder that takes none"},
		"both": {"medium", []appconfig.EncodePreset{ultrafast, noPreset, appconfig.EncodePresetMedium},
			"preset: medium was requested, but 1 video frame(s) were encoded at ultrafast; 1 video frame(s) were not encoded at a preset: they are the camera's own or come from an encoder that takes none"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreVideo, Preset: tc.stated})
			for i, preset := range tc.stored {
				if err := c.handleFrame(encodedAt(preset, uint64(i+1), time.Duration(i+1)*100*time.Millisecond, testRAUFrame)); err != nil {
					t.Fatal(err)
				}
			}
			if int(c.result.Count) != len(tc.stored) {
				t.Fatalf("%d frames stored, want %d", c.result.Count, len(tc.stored))
			}
			if got := storePresetNote(t, c); got != tc.want {
				t.Errorf("note = %q\nwant   %q", got, tc.want)
			}
			c.finishResultDetail(0)
			if tc.want != "" && !strings.Contains(c.result.SourceDetail, tc.want) {
				t.Errorf("the source's detail %q does not hold the note", c.result.SourceDetail)
			}
		})
	}
}

// The camera's own JPEG frames are not video: store: both counts only what the
// encoder made.
func TestRecordingCountsOnlyVideoFramesAgainstTheStatedPreset(t *testing.T) {
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreBoth, Preset: "fast"})
	if err := c.handleFrame(encodedAt(appconfig.EncodePresetFast, 1, 100*time.Millisecond, testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := c.handleFrame(jpegAt(uint64(i+1), time.Duration(i+1)*100*time.Millisecond, testJPEG(byte(i), 20))); err != nil {
			t.Fatal(err)
		}
	}
	if c.result.Count != 4 {
		t.Fatalf("%d frames stored, want 3 JPEG frames and one access unit", c.result.Count)
	}
	if got := storePresetNote(t, c); got != "" {
		t.Errorf("JPEG frames were counted against the preset: %q", got)
	}
}

func storedDrops(c *cameraCapture) uint64 {
	if c.result.Drops == nil {
		return 0
	}
	return *c.result.Drops
}

// Another preset is another encoder run, with parameter sets of its own, so
// the video it makes starts a new segment file at its first frame decoding can
// start on. Frames of the new run that arrive before that one cannot be
// decoded from either file: they are not stored, and count as drops.
func TestRecordingOpensANewSegmentWhereTheEncoderChangesPreset(t *testing.T) {
	const veryfast, ultrafast, medium = appconfig.EncodePresetVeryfast, appconfig.EncodePresetUltrafast, appconfig.EncodePresetMedium
	const notStored = ""
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreVideo, Preset: "veryfast"})
	frames := []struct {
		sequence uint64
		preset   appconfig.EncodePreset
		payload  []byte
		segment  string
	}{
		{1, veryfast, testRAUFrame, "segment-000001.h264"},
		{2, veryfast, testInterFrame, "segment-000001.h264"},
		{3, ultrafast, testRAUFrame, "segment-000002.h264"},
		{4, ultrafast, testInterFrame, "segment-000002.h264"},
		{5, ultrafast, testRAUFrame, "segment-000002.h264"},
		// The hub dropped frame 6, the first of the next run, for this recording.
		{7, medium, testInterFrame, notStored},
		{8, medium, testInterFrame, notStored},
		{9, medium, testRAUFrame, "segment-000003.h264"},
		{10, medium, testInterFrame, "segment-000003.h264"},
	}
	var want []string
	for _, frame := range frames {
		err := c.handleFrame(encodedAt(frame.preset, frame.sequence, time.Duration(frame.sequence)*100*time.Millisecond, frame.payload))
		if frame.segment == notStored {
			if !errors.Is(err, errAwaitCameraRandomAccess) {
				t.Fatalf("frame %d of the new run, before its first key frame: err = %v, want it left out", frame.sequence, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, frame.segment)
	}
	recording := storeRecording{dir: c.dir, result: c.result}
	index := recording.index(t)
	if len(index) != len(want) {
		t.Fatalf("%d frames indexed, want %d", len(index), len(want))
	}
	for i, record := range index {
		if got := filepath.Base(record.Segment); got != want[i] {
			t.Errorf("stored frame %d (sample %d) is in %s, want %s", i+1, record.SampleID, got, want[i])
		}
	}
	assertFileNames(t, recording, "segment-000001.h264", "segment-000002.h264", "segment-000003.h264")
	if got := storedDrops(c); got != 3 {
		t.Errorf("drops = %d, want 3: the frame the hub dropped and the two left out", got)
	}
}

// A stream without sequence numbers has only the count of what was left out.
func TestRecordingCountsFramesLeftOutBeforeTheNewEncodersKeyFrameWithoutSequenceNumbers(t *testing.T) {
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreVideo})
	chunk := func(preset appconfig.EncodePreset, payload []byte) *videoFrame {
		frame := testChunkedH264Frame(payload)
		frame.encodedAt = preset
		return frame
	}
	for i, frame := range []*videoFrame{
		chunk(appconfig.EncodePresetMedium, testRAUFrame),
		chunk(appconfig.EncodePresetUltrafast, testInterFrame),
		chunk(appconfig.EncodePresetUltrafast, testInterFrame),
		chunk(appconfig.EncodePresetUltrafast, testRAUFrame),
	} {
		if err := c.handleFrame(frame); err != nil && !errors.Is(err, errAwaitCameraRandomAccess) {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if c.result.Count != 2 || c.carriedDrops != 2 || c.result.Drops != nil {
		t.Errorf("stored %d frames, %d left out and counted, sequence drops %v; want 2, 2 and none", c.result.Count, c.carriedDrops, c.result.Drops)
	}
}

// The sequence a derived frame carries is the camera's own, which runs on
// while the encoder is replaced for another preset: the camera frames that
// became no access unit meanwhile are a gap in it, counted as any gap is.
func TestRecordingCountsFramesLostWhileTheEncoderIsReplacedAsDrops(t *testing.T) {
	for name, second := range map[string]appconfig.EncodePreset{
		"the same encoder":             appconfig.EncodePresetVeryfast,
		"an encoder at another preset": appconfig.EncodePresetUltrafast,
	} {
		t.Run(name, func(t *testing.T) {
			c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreVideo, Preset: "veryfast"})
			for _, frame := range []struct {
				sequence uint64
				preset   appconfig.EncodePreset
				payload  []byte
			}{
				{1, appconfig.EncodePresetVeryfast, testRAUFrame},
				{2, appconfig.EncodePresetVeryfast, testInterFrame},
				{10, second, testRAUFrame},
				{11, second, testInterFrame},
			} {
				if err := c.handleFrame(encodedAt(frame.preset, frame.sequence, time.Duration(frame.sequence)*33*time.Millisecond, frame.payload)); err != nil {
					t.Fatal(err)
				}
			}
			if got := storedDrops(c); got != 7 {
				t.Errorf("camera frames 3 to 9 never reached the recording, and it counts %d drop(s)", got)
			}
		})
	}
}

// A producer that ended took its numbering with it: the one that replaces it
// counts from where it likes, and the difference is not a loss.
func TestRecordingStartsSequenceTrackingAgainForANewProducer(t *testing.T) {
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreVideo})
	write := func(sequence uint64, payload []byte) {
		t.Helper()
		if err := c.handleFrame(encodedAt(appconfig.EncodePresetUltrafast, sequence, time.Duration(sequence)*33*time.Millisecond, payload)); err != nil {
			t.Fatal(err)
		}
	}
	write(5, testRAUFrame)
	write(6, testInterFrame)
	if err := c.endProducerStreams(); err != nil {
		t.Fatal(err)
	}
	write(50, testRAUFrame)
	write(51, testInterFrame)
	if got := storedDrops(c); got != 0 {
		t.Errorf("drops = %d across a producer's end, want none", got)
	}
	assertFileNames(t, storeRecording{dir: c.dir, result: c.result}, "segment-000001.h264", "segment-000002.h264")
}

// Frames that carry no preset come from one run as far as the recording can
// tell, so they share a segment as before.
func TestRecordingKeepsOneSegmentWhileThePresetDoesNotChange(t *testing.T) {
	for name, preset := range map[string]appconfig.EncodePreset{"no preset": noPreset, "one preset": appconfig.EncodePresetFast} {
		c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreVideo})
		for i, payload := range [][]byte{testRAUFrame, testInterFrame, testRAUFrame, testInterFrame} {
			if err := c.handleFrame(encodedAt(preset, uint64(i+1), time.Duration(i+1)*100*time.Millisecond, payload)); err != nil {
				t.Fatal(err)
			}
		}
		recording := storeRecording{dir: c.dir, result: c.result}
		if got := recording.fileNames(t); !strings.Contains(strings.Join(got, " "), "segment-000001.h264") || strings.Contains(strings.Join(got, " "), "segment-000002") {
			t.Errorf("%s: files %v, want one segment", name, got)
		}
	}
}

// The recording and the pre-roll ring are the campaign's consumers of the
// camera's encoded video, so each states the preset of its source's capture
// block for as long as it is subscribed.
func TestRecordingStatesItsSourcesPresetWhileItRecords(t *testing.T) {
	cam := newStoreTestCamera(t)
	stated := noPreset
	cam.record(t, &data.SourceCapture{Store: data.CaptureStoreVideo, Preset: "fast"}, 2, func() {
		stated = cam.hub.statedPreset()
		cam.deliver(t, h264At(1, time.Second, testRAUFrame))
	}, 1)
	if stated != appconfig.EncodePresetFast {
		t.Errorf("while the recording ran the hub stated %q, want fast", stated)
	}
	if got := cam.hub.statedPreset(); got != noPreset {
		t.Errorf("after the recording stopped the hub still states %q", got)
	}
}

func TestArmedRingStatesItsSourcesPreset(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop(), nil)
	hub, _ := newDefaultHub(t, video, "/dev/video0")
	adapter := &cameraDataAdapter{video: video}
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Capture: &data.SourceCapture{Preset: "veryfast"}}
	adapter.Arm("cam", time.Second, []data.Source{source})
	if got := hub.statedPreset(); got != appconfig.EncodePresetVeryfast {
		t.Errorf("with the ring armed the hub states %q, want veryfast", got)
	}
	adapter.Disarm("cam")
	waitUntil(t, "the disarmed ring has left the hub", func() bool { return hub.statedPreset() == noPreset })
}

// A recording that reattaches to the hub that replaced its camera's states
// its preset there too, whether it began at the trigger or from an armed ring.
func TestRecordingStatesItsPresetAgainWhenItReattaches(t *testing.T) {
	for name, armed := range map[string]bool{"begun at the trigger": false, "begun from an armed ring": true} {
		t.Run(name, func(t *testing.T) {
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
			source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Detail: "USB Cam", Capture: &data.SourceCapture{Preset: "fast"}}
			session := data.CaptureSession{Directory: t.TempDir()}
			if armed {
				session.CampaignKey = "cam"
				adapter.Arm("cam", time.Second, []data.Source{source})
				defer adapter.Disarm("cam")
				waitUntil(t, "the ring has buffered a frame", func() bool { return fp.count() == 1 && hubSubscriberCount(fp.hub(0)) == 1 })
				time.Sleep(20 * time.Millisecond)
			}
			_, origin, _, err := data.CaptureReceipt()
			if err != nil {
				t.Fatal(err)
			}
			session.RequestBootNanos = origin
			capture, err := adapter.Start(ctx, session, []data.Source{source})
			if err != nil || capture == nil {
				t.Fatalf("the recording did not start: %v", err)
			}
			if got := fp.hub(0).statedPreset(); got != appconfig.EncodePresetFast {
				t.Fatalf("the hub the recording began on states %q, want fast", got)
			}

			explicit := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
			newHub, otherID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", explicit, encodedFrames, noPreset)
			if err != nil || !restarted {
				t.Fatalf("takeover failed: restarted=%v err=%v", restarted, err)
			}
			defer newHub.unsubscribe(otherID)
			waitUntil(t, "the recording has reattached to the replacement hub", func() bool { return hubSubscriberCount(newHub) == 2 })
			if got := newHub.statedPreset(); got != appconfig.EncodePresetFast {
				t.Errorf("after the recording reattached the replacement hub states %q, want fast", got)
			}
			if _, err := capture.Stop(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
