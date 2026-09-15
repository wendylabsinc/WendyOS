package data

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

const exampleCampaignYAML = `version: 1
name: forklift-failures
fleet: warehouse-west
sources:
  - camera: front
    capture:
      mode: snapshot
      interval: 2s
      max_resolution: 1280x720
  - ros2: /lidar/points
  - ros2: /vehicle/odometry
capture:
  buffer: 10s
  after_trigger: 20s
  triggers:
    - event: emergency_stop
    - model.uncertainty: "> 0.65"
upload:
  when: wifi
  destination: forklift-episodes
  max_rate: 5MB/s
retention:
  local_quota: 10GiB
export:
  annotation: cvat
models:
  detector: 4.2.0
privacy:
  - name: blur-faces
    revision: "3"
`

func TestParseCampaignExampleAndMatchTriggers(t *testing.T) {
	campaign, err := ParseCampaign([]byte(exampleCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Name != "forklift-failures" || campaign.State != "armed" || len(campaign.Revision) != 64 {
		t.Fatalf("bad campaign: %+v", campaign)
	}
	if reason, _, matched := campaign.Match(ApplicationRecord{Type: "event", Name: "emergency_stop"}); !matched || reason != "event:emergency_stop" {
		t.Fatalf("event did not match: %q %v", reason, matched)
	}
	if reason, _, matched := campaign.Match(ApplicationRecord{Type: "prediction", Model: "detector", Attributes: map[string]any{"uncertainty": 0.7}}); !matched || reason == "" {
		t.Fatalf("uncertainty did not match: %q %v", reason, matched)
	}
	if _, _, matched := campaign.Match(ApplicationRecord{Type: "prediction", Model: "detector", Value: 0.4}); matched {
		t.Fatal("low uncertainty unexpectedly matched")
	}
}

func TestParseCampaignRejectsUnknownFieldsAndBadThresholds(t *testing.T) {
	badField := exampleCampaignYAML + "mystery: true\n"
	if _, err := ParseCampaign([]byte(badField)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	badThreshold := []byte(`version: 1
name: bad
sources: [{telemetry: true}]
capture:
  buffer: 1s
  after_trigger: 1s
  triggers: [{model.uncertainty: "> 2"}]
upload: {when: wifi}
export: {annotation: cvat}
`)
	if _, err := ParseCampaign(badThreshold); err == nil {
		t.Fatal("out-of-range threshold was accepted")
	}
}

const minimalCampaignFormat = `version: %d
name: minimal
sources:
  - %s
capture:
  buffer: 1s
  after_trigger: 1s
  triggers: [{event: go}]
upload: {when: %s}
export: {annotation: cvat}
`

func minimalCampaign(version int, source, when string) []byte {
	return []byte(fmt.Sprintf(minimalCampaignFormat, version, source, when))
}

func TestParseCampaignRequiresAuthorDeclaredVersion(t *testing.T) {
	missing := []byte(strings.Replace(string(minimalCampaign(1, "telemetry: true", "wifi")), "version: 1\n", "", 1))
	if _, err := ParseCampaign(missing); err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatalf("missing version accepted: %v", err)
	}
	if _, err := ParseCampaign(minimalCampaign(2, "telemetry: true", "wifi")); err == nil || !strings.Contains(err.Error(), "supports up to version 1") {
		t.Fatalf("future version accepted: %v", err)
	}
	if _, err := ParseCampaign(minimalCampaign(1, "telemetry: true", "wifi")); err != nil {
		t.Fatalf("supported version rejected: %v", err)
	}
}

func TestUploadPolicyValidation(t *testing.T) {
	if _, err := ParseCampaign(minimalCampaign(1, "telemetry: true", "sometimes")); err == nil || !strings.Contains(err.Error(), "always, wifi, or manual") {
		t.Fatalf("invalid upload.when accepted: %v", err)
	}
	// destination is optional: minimalCampaign has none.
	campaign, err := ParseCampaign(minimalCampaign(1, "telemetry: true", "manual"))
	if err != nil {
		t.Fatalf("campaign without destination rejected: %v", err)
	}
	if campaign.Upload.Destination != "" {
		t.Fatalf("unexpected destination: %q", campaign.Upload.Destination)
	}
	rated := []byte(strings.Replace(string(minimalCampaign(1, "telemetry: true", "wifi")), "upload: {when: wifi}", "upload: {when: wifi, max_rate: 5MB/s}\nretention: {local_quota: 10GiB}", 1))
	campaign, err = ParseCampaign(rated)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.UploadMaxRateBytes() != 5_000_000 || campaign.LocalQuotaBytes() != 10<<30 {
		t.Fatalf("rate=%d quota=%d", campaign.UploadMaxRateBytes(), campaign.LocalQuotaBytes())
	}
	badRate := []byte(strings.Replace(string(minimalCampaign(1, "telemetry: true", "wifi")), "upload: {when: wifi}", "upload: {when: wifi, max_rate: fast}", 1))
	if _, err = ParseCampaign(badRate); err == nil || !strings.Contains(err.Error(), "max_rate") {
		t.Fatalf("invalid max_rate accepted: %v", err)
	}
	badQuota := []byte(string(minimalCampaign(1, "telemetry: true", "wifi")) + "retention: {local_quota: \"-5\"}\n")
	if _, err = ParseCampaign(badQuota); err == nil || !strings.Contains(err.Error(), "local_quota") {
		t.Fatalf("invalid local_quota accepted: %v", err)
	}
}

func TestSourceCaptureValidation(t *testing.T) {
	reject := map[string]string{
		"snapshot without interval":    "camera: front\n    capture: {mode: snapshot}",
		"threshold without trigger":    "camera: front\n    capture: {mode: threshold}",
		"unknown mode":                 "camera: front\n    capture: {mode: burst}",
		"field from another mode":      "camera: front\n    capture: {mode: snapshot, interval: 2s, rate: 5}",
		"trigger outside threshold":    "camera: front\n    capture: {mode: continuous, trigger: \"level_db > -20\"}",
		"max_resolution on non-camera": "telemetry: true\n    capture: {mode: continuous, max_resolution: 1280x720}",
		"malformed max_resolution":     "camera: front\n    capture: {mode: continuous, max_resolution: huge}",
		"uncertainty above range":      "camera: front\n    capture: {mode: threshold, trigger: \"model.uncertainty > 2\"}",
		"fragment without pre or post": "camera: front\n    capture: {mode: fragment}",
	}
	for name, source := range reject {
		if _, err := ParseCampaign(minimalCampaign(1, source, "wifi")); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	accept := map[string]string{
		"default continuous":         "camera: front\n    capture: {rate: 5}",
		"snapshot with interval":     "camera: front\n    capture: {mode: snapshot, interval: 30s, max_resolution: 1920x1080}",
		"fragment with pre and post": "ros2: /lidar/points\n    capture: {mode: fragment, pre: 5s, post: 10s}",
		"negative decibel threshold": "camera: front\n    capture: {mode: threshold, trigger: \"level_db > -20\", fragment: 10s}",
		"uncertainty threshold":      "camera: front\n    capture: {mode: threshold, trigger: \"model.uncertainty > 0.9\"}",
	}
	for name, source := range accept {
		if _, err := ParseCampaign(minimalCampaign(1, source, "wifi")); err != nil {
			t.Errorf("%s was rejected: %v", name, err)
		}
	}
}

func TestRevisionHashCoversCaptureUploadAndRetention(t *testing.T) {
	base, err := ParseCampaign(minimalCampaign(1, "telemetry: true", "wifi"))
	if err != nil {
		t.Fatal(err)
	}
	variants := []string{
		strings.Replace(string(minimalCampaign(1, "telemetry: true", "wifi")), "telemetry: true", "telemetry: true\n    capture: {mode: snapshot, interval: 2s}", 1),
		strings.Replace(string(minimalCampaign(1, "telemetry: true", "wifi")), "upload: {when: wifi}", "upload: {when: wifi, max_rate: 5MB/s}", 1),
		string(minimalCampaign(1, "telemetry: true", "wifi")) + "retention: {local_quota: 10GiB}\n",
	}
	for i, variant := range variants {
		changed, err := ParseCampaign([]byte(variant))
		if err != nil {
			t.Fatal(err)
		}
		if changed.Revision == base.Revision {
			t.Errorf("variant %d did not change the revision hash", i)
		}
	}
}

func TestCampaignPersistsAndResolvesSemanticSources(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []Source {
		return []Source{
			{ID: "v4l2:/dev/video2", Kind: "camera", ClockDomain: "V4L2", Healthy: true, Detail: "Logitech Brio"},
			{ID: "ros2:cyclone:domain-0", Kind: "ros2", ClockDomain: "ROS", Healthy: true},
		}
	})
	campaign, err := manager.DeployCampaign([]byte(exampleCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range campaign.Warnings {
		if strings.Contains(warning, "not implemented yet") && strings.Contains(warning, "camera:front") {
			t.Fatalf("camera snapshot mode still deploy-warns: %v", campaign.Warnings)
		}
	}
	stored, err := manager.Campaign(campaign.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != campaign.Revision || stored.DeployedUnixNanos == 0 {
		t.Fatalf("campaign was not durably stored: %+v", stored)
	}
	sources, topics, captures, err := manager.ResolveCampaignSources(stored)
	if err != nil {
		t.Fatal(err)
	}
	wantSources := map[string]bool{"applications": true, "ros2:cyclone:domain-0": true, "v4l2:/dev/video2": true}
	for _, source := range sources {
		delete(wantSources, source)
	}
	if len(wantSources) != 0 || len(topics) != 2 {
		t.Fatalf("sources=%v topics=%v missing=%v", sources, topics, wantSources)
	}
	capture := captures["v4l2:/dev/video2"]
	if capture == nil || capture.EffectiveMode() != "snapshot" || capture.IntervalDuration() != 2*time.Second {
		t.Fatalf("camera capture policy was not resolved: %+v", capture)
	}
	if w, h, ok := capture.MaxResolutionPixels(); !ok || w != 1280 || h != 720 {
		t.Fatalf("max_resolution not resolved: %dx%d %v", w, h, ok)
	}
}

func TestDeployWarnsUnimplementedModesPerSourceKind(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	warned := func(source string) []string {
		campaign, err := manager.DeployCampaign(minimalCampaign(1, source, "wifi"))
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		var modeWarnings []string
		for _, warning := range campaign.Warnings {
			if strings.Contains(warning, "not implemented yet") {
				modeWarnings = append(modeWarnings, warning)
			}
		}
		return modeWarnings
	}
	if warnings := warned("camera: front\n    capture: {mode: snapshot, interval: 2s}"); len(warnings) != 0 {
		t.Fatalf("camera snapshot mode deploy-warns although the camera adapter implements it: %v", warnings)
	}
	if warnings := warned("ros2: /lidar/points\n    capture: {mode: snapshot, interval: 2s}"); len(warnings) != 1 || !strings.Contains(warnings[0], "ros2:/lidar/points") {
		t.Fatalf("ros2 snapshot mode must still deploy-warn: %v", warnings)
	}
	if warnings := warned("camera: front\n    capture: {mode: threshold, trigger: \"model.uncertainty > 0.9\"}"); len(warnings) != 1 {
		t.Fatalf("camera threshold mode must still deploy-warn: %v", warnings)
	}
}
