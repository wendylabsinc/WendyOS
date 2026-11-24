package data

import (
	"context"
	"reflect"
	"testing"
)

func camerasAmongTestSources() []Source {
	return []Source{
		{ID: "v4l2:/dev/video2", Kind: "camera", Healthy: true, Detail: "Logi H264 USB"},
		{ID: "v4l2:/dev/video4", Kind: "camera", Healthy: true, Detail: "C920 USB"},
		{ID: "ipcamera:203", Kind: "camera", Healthy: false, Detail: "yard (offline)"},
		{ID: "audio:usb-mic", Kind: "audio", Healthy: true, Detail: "USB microphone"},
	}
}

// CamerasAmong answers for a deploy what a trigger and arming resolve for
// themselves, so it is held to them: for each plan the cameras it says are
// recorded carry the capture blocks ResolveCampaignSources returns, and its
// listings are ResolveCampaignCameraSources'.
func TestCampaignCamerasAmong_AgreesWithWhatATriggerAndArmingResolve(t *testing.T) {
	frames, snapshot, both := &SourceCapture{Store: CaptureStoreFrames}, &SourceCapture{Mode: "snapshot", Interval: "1s"}, &SourceCapture{Store: CaptureStoreBoth}
	for name, sources := range map[string][]CampaignSource{
		"a camera listed again without a capture block": {{Camera: "/dev/video4", Capture: frames}, {Camera: "/dev/video4"}},
		"every camera with a block, then one bare":      {{Camera: "*", Capture: frames}, {Camera: "/dev/video4"}},
		"a camera with a block, then every camera bare": {{Camera: "/dev/video4", Capture: snapshot}, {Camera: "*"}},
		"two blocks for one camera":                     {{Camera: "C920", Capture: frames}, {Camera: "v4l2:/dev/video4", Capture: both}},
		"a bare camera, then a block":                   {{Camera: "/dev/video2"}, {Camera: "Logi", Capture: snapshot}},
		"cameras beside other sources":                  {{Telemetry: true}, {Camera: "/dev/video2", Capture: both}, {Audio: "audio:usb-mic"}},
	} {
		t.Run(name, func(t *testing.T) {
			manager, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager.SetSourceProvider(func(context.Context) []Source { return camerasAmongTestSources() })
			campaign := Campaign{Name: "among", Sources: sources}

			recorded, listed := campaign.CamerasAmong(camerasAmongTestSources())
			_, _, captures, err := manager.ResolveCampaignSources(campaign)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range recorded {
				if source.Capture != captures[source.ID] {
					t.Errorf("%s is recorded with %+v, and a trigger records it with %+v", source.ID, source.Capture, captures[source.ID])
				}
			}
			armed, err := manager.ResolveCampaignCameraSources(campaign)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(listed, armed) {
				t.Errorf("listings = %+v\narming is handed %+v", listed, armed)
			}
			selected := map[string]bool{}
			for _, source := range armed {
				selected[source.ID] = true
			}
			if len(recorded) != len(selected) {
				t.Errorf("%d cameras are recorded, and the plan selects %d: %+v", len(recorded), len(selected), recorded)
			}
		})
	}
}

func TestCampaignCamerasAmong_ACameraListedAgainWithoutABlockKeepsTheBlockItHas(t *testing.T) {
	frames := &SourceCapture{Store: CaptureStoreFrames}
	campaign := Campaign{Sources: []CampaignSource{{Camera: "/dev/video4", Capture: frames}, {Camera: "/dev/video4"}}}
	recorded, listed := campaign.CamerasAmong(camerasAmongTestSources())
	if want := []Source{{ID: "v4l2:/dev/video4", Kind: "camera", Capture: frames}}; !reflect.DeepEqual(recorded, want) {
		t.Errorf("recorded = %+v, want %+v", recorded, want)
	}
	if want := []Source{{ID: "v4l2:/dev/video4", Kind: "camera", Capture: frames}, {ID: "v4l2:/dev/video4", Kind: "camera"}}; !reflect.DeepEqual(listed, want) {
		t.Errorf("listed = %+v, want %+v", listed, want)
	}
}

// A deploy speaks of the cameras that are there: a selector that names none
// costs the other selectors nothing, where a trigger refuses the whole plan.
func TestCampaignCamerasAmong_ASelectorThatNamesNoCameraIsPassedOver(t *testing.T) {
	campaign := Campaign{Sources: []CampaignSource{{Camera: "/dev/video9"}, {Camera: "yard"}, {Camera: "C920"}}}
	recorded, listed := campaign.CamerasAmong(camerasAmongTestSources())
	want := []Source{{ID: "v4l2:/dev/video4", Kind: "camera"}}
	if !reflect.DeepEqual(recorded, want) || !reflect.DeepEqual(listed, want) {
		t.Errorf("recorded = %+v, listed = %+v, want both %+v", recorded, listed, want)
	}
	if recorded, listed := campaign.CamerasAmong(nil); len(recorded) != 0 || len(listed) != 0 {
		t.Errorf("with no camera present: recorded = %+v, listed = %+v", recorded, listed)
	}
}
