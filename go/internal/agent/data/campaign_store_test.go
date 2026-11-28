package data

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func cameraStoreCampaign(capture string) []byte {
	return minimalCampaign(1, "camera: front\n    capture: {"+capture+"}", "wifi")
}

func TestCaptureStoreParsesFromYAML(t *testing.T) {
	for capture, want := range map[string]string{
		"rate: 5":       "",
		"store: video":  CaptureStoreVideo,
		"store: frames": CaptureStoreFrames,
		"store: both":   CaptureStoreBoth,
	} {
		campaign, err := ParseCampaign(cameraStoreCampaign(capture))
		if err != nil {
			t.Errorf("capture {%s} was rejected: %v", capture, err)
			continue
		}
		if got := campaign.Sources[0].Capture.Store; got != want {
			t.Errorf("capture {%s} parsed store %q, want %q", capture, got, want)
		}
	}
}

func TestCaptureStoreParsesFromJSON(t *testing.T) {
	for raw, want := range map[string]string{
		`{"mode":"continuous"}`: "",
		`{"store":"video"}`:     CaptureStoreVideo,
		`{"store":"frames"}`:    CaptureStoreFrames,
		`{"store":"both"}`:      CaptureStoreBoth,
	} {
		var capture SourceCapture
		if err := json.Unmarshal([]byte(raw), &capture); err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if capture.Store != want {
			t.Errorf("%s parsed store %q, want %q", raw, capture.Store, want)
		}
		if err := validateSourceCapture(CampaignSource{Camera: "front", Capture: &capture}); err != nil {
			t.Errorf("%s was rejected: %v", raw, err)
		}
	}
	plan := `{"version":1,"name":"json-plan","sources":[{"camera":"front","capture":{"store":"both"}}],` +
		`"capture":{"buffer":"1s","after_trigger":"1s","triggers":[{"event":"go"}]},"upload":{"when":"wifi"},"export":{"annotation":"cvat"}}`
	campaign, err := ParseCampaign([]byte(plan))
	if err != nil {
		t.Fatalf("a JSON plan with store was rejected: %v", err)
	}
	if got := campaign.Sources[0].Capture.Store; got != CaptureStoreBoth {
		t.Fatalf("a JSON plan parsed store %q, want %q", got, CaptureStoreBoth)
	}
}

func TestCaptureStoreRejectsUnknownValue(t *testing.T) {
	_, err := ParseCampaign(cameraStoreCampaign("store: stills"))
	if err == nil {
		t.Fatal("an unknown store value was accepted")
	}
	if want := "sources[0].capture: store must be one of video, frames, both"; err.Error() != want {
		t.Fatalf("error %q, want %q", err, want)
	}
}

func TestCaptureStoreRejectedOnNonCameraSources(t *testing.T) {
	for name, source := range map[string]string{
		"audio":     "audio: C920\n    capture: {store: frames}",
		"ros2":      "ros2: /lidar/points\n    capture: {store: both}",
		"telemetry": "telemetry: true\n    capture: {store: video}",
	} {
		_, err := ParseCampaign(minimalCampaign(1, source, "wifi"))
		if err == nil {
			t.Errorf("store on a %s source was accepted", name)
			continue
		}
		if want := "sources[0].capture: store applies only to camera sources"; err.Error() != want {
			t.Errorf("%s: error %q, want %q", name, err, want)
		}
	}
}

func TestCaptureStoreRejectedInSnapshotMode(t *testing.T) {
	for _, store := range []string{CaptureStoreVideo, CaptureStoreFrames, CaptureStoreBoth} {
		_, err := ParseCampaign(cameraStoreCampaign("mode: snapshot, interval: 2s, store: " + store))
		if err == nil {
			t.Errorf("store: %s was accepted in snapshot mode", store)
			continue
		}
		if want := "sources[0].capture: store applies only to continuous capture; snapshot mode always stores single frames"; err.Error() != want {
			t.Errorf("store: %s: error %q, want %q", store, err, want)
		}
	}
	if _, err := ParseCampaign(cameraStoreCampaign("mode: snapshot, interval: 2s")); err != nil {
		t.Fatalf("snapshot mode without store was rejected: %v", err)
	}
}

// A camera mode the adapter does not implement yet deploys with the existing
// not-implemented warning and records continuously, so store is accepted with
// those modes rather than rejected.
func TestCaptureStoreFollowsTheCaptureModeDeploymentRule(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pendingModeWarnings := func(capture string) []string {
		t.Helper()
		campaign, err := manager.DeployCampaign(cameraStoreCampaign(capture))
		if err != nil {
			t.Fatalf("capture {%s} did not deploy: %v", capture, err)
		}
		var warnings []string
		for _, warning := range campaign.Warnings {
			if strings.Contains(warning, "not implemented yet") {
				warnings = append(warnings, warning)
			}
		}
		return warnings
	}
	for _, capture := range []string{
		"store: frames",
		"mode: continuous, rate: 5, store: both",
	} {
		if warnings := pendingModeWarnings(capture); len(warnings) != 0 {
			t.Errorf("capture {%s} warned although the camera adapter implements its mode: %v", capture, warnings)
		}
	}
	for _, capture := range []string{
		"mode: fragment, pre: 1s, store: frames",
		`mode: threshold, trigger: "model.uncertainty > 0.9", store: both`,
	} {
		warnings := pendingModeWarnings(capture)
		if len(warnings) != 1 || !strings.Contains(warnings[0], "camera:front") {
			t.Errorf("capture {%s} must deploy with the not-implemented mode warning: %v", capture, warnings)
		}
	}
}

func TestCaptureStoreAccessors(t *testing.T) {
	var absent *SourceCapture
	for name, tc := range map[string]struct {
		capture       *SourceCapture
		video, frames bool
	}{
		"nil":    {absent, true, false},
		"empty":  {&SourceCapture{}, true, false},
		"video":  {&SourceCapture{Store: CaptureStoreVideo}, true, false},
		"frames": {&SourceCapture{Store: CaptureStoreFrames}, false, true},
		"both":   {&SourceCapture{Store: CaptureStoreBoth}, true, true},
	} {
		if got := tc.capture.StoresVideo(); got != tc.video {
			t.Errorf("%s: StoresVideo() = %v, want %v", name, got, tc.video)
		}
		if got := tc.capture.StoresFrames(); got != tc.frames {
			t.Errorf("%s: StoresFrames() = %v, want %v", name, got, tc.frames)
		}
	}
}

func TestCaptureStoreSurvivesDeployAndResolution(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []Source {
		return []Source{{ID: "v4l2:/dev/video2", Kind: "camera", ClockDomain: "V4L2", Healthy: true, Detail: "Logitech Brio"}}
	})
	deployed, err := manager.DeployCampaign(cameraStoreCampaign("max_resolution: 1280x720, store: both"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := manager.Campaign(deployed.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Sources[0].Capture.Store; got != CaptureStoreBoth {
		t.Fatalf("the stored plan holds store %q, want %q", got, CaptureStoreBoth)
	}
	_, _, captures, err := manager.ResolveCampaignSources(stored)
	if err != nil {
		t.Fatal(err)
	}
	if capture := captures["v4l2:/dev/video2"]; !capture.StoresVideo() || !capture.StoresFrames() {
		t.Fatalf("the resolved capture policy lost store: %+v", capture)
	}
	cameras, err := manager.ResolveCampaignCameraSources(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(cameras) != 1 || !cameras[0].Capture.StoresFrames() {
		t.Fatalf("the resolved camera source lost store: %+v", cameras)
	}

	planJSON, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(planJSON), `"store":"both"`) {
		t.Fatalf("the plan JSON shown for a deployed campaign omits store: %s", planJSON)
	}
	planYAML, err := yaml.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := ParseCampaign(planYAML)
	if err != nil {
		t.Fatalf("the re-marshalled plan no longer parses: %v\n%s", err, planYAML)
	}
	if got := reparsed.Sources[0].Capture.Store; got != CaptureStoreBoth {
		t.Fatalf("the re-marshalled plan holds store %q, want %q", got, CaptureStoreBoth)
	}
	if reparsed.Revision != deployed.Revision {
		t.Fatalf("the re-marshalled plan hashed to %s, want %s", reparsed.Revision, deployed.Revision)
	}
}

func TestCaptureStoreOmittedWhenUnset(t *testing.T) {
	campaign, err := ParseCampaign(cameraStoreCampaign("rate: 5"))
	if err != nil {
		t.Fatal(err)
	}
	planJSON, err := json.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	planYAML, err := yaml.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planJSON), "store") || strings.Contains(string(planYAML), "store") {
		t.Fatalf("a plan without store serialises the key, which an older agent rejects:\n%s\n%s", planJSON, planYAML)
	}
}

// captureBlockDigestPinRevision is the revision this plan had before store
// existed. Deployed campaigns that never name store must keep their revision.
const captureBlockDigestPinRevision = "8b89be298fb29d0105eb01b66046518d5c7e2677cc24dd54feac43ed67124a7e"

func TestCaptureStoreChangesTheRevisionOnlyWhenDeclared(t *testing.T) {
	const capture = "mode: snapshot, interval: 2s, max_resolution: 1280x720"
	revision := func(capture string) string {
		t.Helper()
		campaign, err := ParseCampaign(cameraStoreCampaign(capture))
		if err != nil {
			t.Fatal(err)
		}
		return campaign.Revision
	}
	if got := revision(capture); got != captureBlockDigestPinRevision {
		t.Fatalf("a plan without store now hashes to %s, want the pinned %s", got, captureBlockDigestPinRevision)
	}
	const continuous = "max_resolution: 1280x720"
	unset, frames, both := revision(continuous), revision(continuous+", store: frames"), revision(continuous+", store: both")
	if frames == unset || both == unset || frames == both {
		t.Fatalf("store did not reach the revision: unset=%s frames=%s both=%s", unset, frames, both)
	}
}

func TestDeployWarnsWhenACameraSelectorIsListedTwice(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repeatedSelectorWarnings := func(sources string) []string {
		t.Helper()
		campaign, err := manager.DeployCampaign(minimalCampaign(1, sources, "wifi"))
		if err != nil {
			t.Fatal(err)
		}
		var warnings []string
		for _, warning := range campaign.Warnings {
			if strings.Contains(warning, "listed more than once") {
				warnings = append(warnings, warning)
			}
		}
		return warnings
	}
	warnings := repeatedSelectorWarnings("camera: front\n    capture: {store: video}\n  - camera: \" front \"\n    capture: {store: frames}\n  - camera: side")
	if len(warnings) != 1 {
		t.Fatalf("a camera listed twice must warn once: %v", warnings)
	}
	for _, want := range []string{"camera:front", "last capture block", "store: both"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning %q does not name %q", warnings[0], want)
		}
	}
	if strings.Contains(warnings[0], "camera:side") {
		t.Errorf("warning names a camera listed once: %q", warnings[0])
	}
	if warnings := repeatedSelectorWarnings("camera: front\n  - camera: side\n  - telemetry: true"); len(warnings) != 0 {
		t.Fatalf("distinct cameras warned: %v", warnings)
	}
}
