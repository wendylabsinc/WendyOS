package data

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestCapturePresetParsesFromYAMLAndJSON(t *testing.T) {
	for _, name := range appconfig.EncodePresets {
		for _, capture := range []string{"preset: " + name, "store: video, preset: " + name, "store: both, rate: 5, preset: " + name} {
			campaign, err := ParseCampaign(cameraStoreCampaign(capture))
			if err != nil {
				t.Errorf("capture {%s} was rejected: %v", capture, err)
				continue
			}
			if got := campaign.Sources[0].Capture.EncodePreset(); got.String() != name {
				t.Errorf("capture {%s} parsed preset %q, want %q", capture, got, name)
			}
		}
		var capture SourceCapture
		if err := json.Unmarshal([]byte(`{"store":"video","preset":"`+name+`"}`), &capture); err != nil {
			t.Fatal(err)
		}
		if capture.Preset != name {
			t.Errorf("JSON preset %q parsed as %q", name, capture.Preset)
		}
		if err := validateSourceCapture(CampaignSource{Camera: "front", Capture: &capture}); err != nil {
			t.Errorf("JSON preset %q was rejected: %v", name, err)
		}
	}
}

func TestCapturePresetIsUnsetWithoutTheKey(t *testing.T) {
	var absent *SourceCapture
	if got := absent.EncodePreset(); got != appconfig.EncodePresetUnset {
		t.Errorf("a source without a capture block states %q", got)
	}
	campaign, err := ParseCampaign(cameraStoreCampaign("store: video"))
	if err != nil {
		t.Fatal(err)
	}
	if got := campaign.Sources[0].Capture.EncodePreset(); got != appconfig.EncodePresetUnset {
		t.Errorf("a capture block without preset states %q", got)
	}
}

func TestCapturePresetRejections(t *testing.T) {
	for name, tc := range map[string]struct{ source, want string }{
		"unknown value": {
			"camera: front\n    capture: {preset: slow}",
			"sources[0].capture: preset must be one of ultrafast, superfast, veryfast, faster, fast, medium",
		},
		"a value for offline encoding": {
			"camera: front\n    capture: {store: video, preset: veryslow}",
			"sources[0].capture: preset must be one of ultrafast, superfast, veryfast, faster, fast, medium",
		},
		"audio source": {
			"audio: C920\n    capture: {preset: veryfast}",
			"sources[0].capture: preset applies only to camera sources",
		},
		"ros2 source": {
			"ros2: /lidar/points\n    capture: {preset: veryfast}",
			"sources[0].capture: preset applies only to camera sources",
		},
		"telemetry source": {
			"telemetry: true\n    capture: {preset: veryfast}",
			"sources[0].capture: preset applies only to camera sources",
		},
		"store: frames": {
			"camera: front\n    capture: {store: frames, preset: veryfast}",
			"sources[0].capture: preset applies only to a source that stores video; store: frames stores the camera's own frames",
		},
		"snapshot mode": {
			"camera: front\n    capture: {mode: snapshot, interval: 2s, preset: veryfast}",
			"sources[0].capture: preset applies only to continuous capture, not snapshot mode",
		},
	} {
		_, err := ParseCampaign(minimalCampaign(1, tc.source, "wifi"))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if err.Error() != tc.want {
			t.Errorf("%s: error %q, want %q", name, err, tc.want)
		}
	}
}

// An agent from before presets refuses a plan that names one, as it refuses
// any key it does not know, so a plan without one must not serialise the key.
func TestCapturePresetOmittedWhenUnsetAndAnUnknownCaptureKeyIsRefused(t *testing.T) {
	campaign, err := ParseCampaign(cameraStoreCampaign("store: video"))
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
	if strings.Contains(string(planJSON), "preset") || strings.Contains(string(planYAML), "preset") {
		t.Fatalf("a plan without preset serialises the key, which an older agent rejects:\n%s\n%s", planJSON, planYAML)
	}
	if _, err := ParseCampaign(cameraStoreCampaign("store: video, a_key_this_agent_does_not_know: veryfast")); err == nil {
		t.Fatal("a capture block with an unknown key was accepted")
	}
}

func TestCapturePresetSurvivesReMarshalling(t *testing.T) {
	campaign, err := ParseCampaign(cameraStoreCampaign("store: both, preset: veryfast"))
	if err != nil {
		t.Fatal(err)
	}
	planYAML, err := yaml.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := ParseCampaign(planYAML)
	if err != nil {
		t.Fatalf("the re-marshalled plan no longer parses: %v\n%s", err, planYAML)
	}
	if got := reparsed.Sources[0].Capture.EncodePreset(); got != appconfig.EncodePresetVeryfast {
		t.Fatalf("the re-marshalled plan holds preset %q", got)
	}
	if reparsed.Revision != campaign.Revision {
		t.Fatalf("the re-marshalled plan hashed to %s, want %s", reparsed.Revision, campaign.Revision)
	}
}

func TestCapturePresetChangesTheRevisionOnlyWhenDeclared(t *testing.T) {
	revision := func(capture string) string {
		t.Helper()
		campaign, err := ParseCampaign(cameraStoreCampaign(capture))
		if err != nil {
			t.Fatal(err)
		}
		return campaign.Revision
	}
	if got := revision("mode: snapshot, interval: 2s, max_resolution: 1280x720"); got != captureBlockDigestPinRevision {
		t.Fatalf("a plan without preset now hashes to %s, want the pinned %s", got, captureBlockDigestPinRevision)
	}
	const continuous = "max_resolution: 1280x720"
	unset, veryfast, medium := revision(continuous), revision(continuous+", preset: veryfast"), revision(continuous+", preset: medium")
	if veryfast == unset || medium == unset || veryfast == medium {
		t.Fatalf("preset did not reach the revision: unset=%s veryfast=%s medium=%s", unset, veryfast, medium)
	}
}
