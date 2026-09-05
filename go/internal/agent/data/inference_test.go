package data

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func peopleCampaign(t *testing.T) []byte {
	t.Helper()
	contents, err := os.ReadFile("../../../../Examples/WendyDataPeople/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestInferenceExampleAndModelURL(t *testing.T) {
	raw := peopleCampaign(t)
	campaign, err := ParseCampaign(raw)
	if err != nil {
		t.Fatal(err)
	}
	fromURL, err := ParseCampaign([]byte(strings.ReplaceAll(string(raw), "model: facebook/", "model: https://huggingface.co/facebook/")))
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Revision != fromURL.Revision {
		t.Fatal("equivalent model URL changed campaign revision")
	}
	if !campaign.Inference.IsEnabled() || campaign.Notify.On != NotifyOnEpisodeCommitted {
		t.Fatalf("bad example: %+v", campaign)
	}
	campaign.InferenceStatus = &InferenceStatus{State: "error", Error: "transient runtime error"}
	plan, err := json.Marshal(campaign.planDigestInput())
	if err != nil || strings.Contains(string(plan), "inference_status") {
		t.Fatalf("live status leaked into plan: %s: %v", plan, err)
	}
}

func TestInferenceRejectsInvalidConfiguration(t *testing.T) {
	raw := string(peopleCampaign(t))
	changes := [][2]string{
		{"model: facebook/detr-resnet-50", "model: https://example.com/model"},
		{"model: facebook/detr-resnet-50", "model: ../model"},
		{"revision: 1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b", "revision: main"},
		{"labels: [person]", "labels: []"},
		{"labels: [person]", "labels: [person, person]"},
		{"threshold: 0.9", "threshold: .nan"},
		{"threshold: 0.9", "threshold: 2"},
		{"rate: 1", "rate: 0"},
		{"rate: 1", "rate: .inf"},
		{"clear_after: 5s", "clear_after: -1s"},
		{"cooldown: 30s", "cooldown: 48h"},
		{"event: person_detected\n  clear_after", "event: another_event\n  clear_after"},
		{"  on: episode_committed", "  on: detection"},
	}
	for _, change := range changes {
		t.Run(change[1], func(t *testing.T) {
			if _, err := ParseCampaign([]byte(strings.ReplaceAll(raw, change[0], change[1]))); err == nil {
				t.Fatalf("accepted %s", change[1])
			}
		})
	}
}

func TestWildcardCamerasResolveAllHealthyAndRefresh(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sources := []Source{
		{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true},
		{ID: "v4l2:/dev/video2", Kind: "camera", Healthy: true},
		{ID: "ipcamera:1000000", Kind: "camera", Healthy: true},
		{ID: "ipcamera:1000001", Kind: "camera", Healthy: false},
		{ID: "audio:default", Kind: "audio", Healthy: true},
	}
	manager.SetSourceProvider(func(context.Context) []Source { return sources })
	campaign, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	ids, _, captures, err := manager.ResolveCampaignSources(campaign)
	want := []string{"applications", "ipcamera:1000000", "v4l2:/dev/video0", "v4l2:/dev/video2"}
	if err != nil || !reflect.DeepEqual(ids, want) || len(captures) != 3 {
		t.Fatalf("resolved %v, policies %v, err %v", ids, captures, err)
	}
	for _, policy := range captures {
		if policy.EffectiveMode() != "continuous" {
			t.Fatal("lost capture policy")
		}
	}
	cameras, err := manager.ResolveCampaignCameraSources(campaign)
	if err != nil || len(cameras) != 3 {
		t.Fatalf("pre-roll cameras = %v, err %v", cameras, err)
	}
	for _, camera := range cameras {
		if camera.Capture == nil || camera.Capture.EffectiveMode() != "continuous" {
			t.Fatal("pre-roll camera lost capture policy")
		}
	}
	sources[3].Healthy = true
	ids, _, _, err = manager.ResolveCampaignSources(campaign)
	if err != nil || len(ids) != 5 {
		t.Fatalf("recovered camera was not selected: %v, %v", ids, err)
	}
	cameras, err = manager.ResolveCampaignCameraSources(campaign)
	if err != nil || len(cameras) != 4 {
		t.Fatalf("recovered pre-roll camera was not selected: %v, %v", cameras, err)
	}
	sources = nil
	if _, err = manager.ResolveCampaignCameraSources(campaign); err == nil {
		t.Fatal("empty camera inventory accepted for pre-roll")
	}
	if _, _, _, err = manager.ResolveCampaignSources(campaign); err == nil {
		t.Fatal("empty camera inventory would produce an applications-only episode")
	}
}

func TestInferenceDisabledPersistsWithoutChangingOtherFields(t *testing.T) {
	raw := strings.ReplaceAll(string(peopleCampaign(t)), "enabled: true", "enabled: false")
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := manager.DeployCampaign([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := manager.Campaign(campaign.Name)
	if err != nil || restored.Inference.IsEnabled() || restored.Inference.Model != campaign.Inference.Model {
		t.Fatalf("disabled plan lost: %+v, %v", restored, err)
	}
}

func TestDetectionNotificationRequiresValidWebhook(t *testing.T) {
	raw := string(peopleCampaign(t))
	for _, endpoint := range []string{"https://notify.example/person", "http://localhost:8080/person"} {
		configured := strings.ReplaceAll(raw, "  on: episode_committed", "  on: detection\n  webhook: "+endpoint)
		if _, err := ParseCampaign([]byte(configured)); err != nil {
			t.Fatal(err)
		}
	}
	for _, endpoint := range []string{"file:///tmp/notify", "https://user:password@notify.example", "https://notify.example/#token", "relative/path", "https://"} {
		configured := strings.ReplaceAll(raw, "  on: episode_committed", "  on: detection\n  webhook: "+endpoint)
		if _, err := ParseCampaign([]byte(configured)); err == nil {
			t.Fatalf("accepted invalid webhook %q", endpoint)
		}
	}
}

func TestInferenceSettingsChangeCampaignRevision(t *testing.T) {
	raw := string(peopleCampaign(t))
	base, err := ParseCampaign([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{"model: facebook/detr-resnet-50", "model: example/detector"},
		{"revision: 1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b", "revision: 0000000000000000000000000000000000000000"},
		{"labels: [person]", "labels: [car]"},
		{"threshold: 0.9", "threshold: 0.8"},
		{"rate: 1", "rate: 2"},
		{"person_detected", "vehicle_detected"},
		{"clear_after: 5s", "clear_after: 6s"},
		{"cooldown: 30s", "cooldown: 60s"},
		{"enabled: true", "enabled: false"},
	} {
		t.Run(change[0], func(t *testing.T) {
			changed, err := ParseCampaign([]byte(strings.ReplaceAll(raw, change[0], change[1])))
			if err != nil {
				t.Fatal(err)
			}
			if changed.Revision == base.Revision {
				t.Fatal("inference plan change did not change revision")
			}
		})
	}
}
