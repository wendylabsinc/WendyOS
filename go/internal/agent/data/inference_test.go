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
	if !campaign.Inference.IsEnabled() || campaign.Notify.On != NotifyOnEvent || campaign.Notify.Event != "person_detected" || campaign.Notify.Webhook != "" {
		t.Fatalf("bad example: %+v", campaign)
	}
	campaign.InferenceStatus = &InferenceStatus{State: "error", Error: "transient runtime error"}
	plan, err := json.Marshal(campaign.planOnly())
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
	sources[3].Healthy = true
	ids, _, _, err = manager.ResolveCampaignSources(campaign)
	if err != nil || len(ids) != 5 {
		t.Fatalf("recovered camera was not selected: %v, %v", ids, err)
	}
	sources = nil
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
		configured := strings.ReplaceAll(raw, "  on: event\n  event: person_detected", "  on: detection\n  webhook: "+endpoint)
		if _, err := ParseCampaign([]byte(configured)); err != nil {
			t.Fatal(err)
		}
	}
	for _, endpoint := range []string{"file:///tmp/notify", "https://user:password@notify.example", "https://notify.example/#token", "relative/path", "https://"} {
		configured := strings.ReplaceAll(raw, "  on: event\n  event: person_detected", "  on: detection\n  webhook: "+endpoint)
		if _, err := ParseCampaign([]byte(configured)); err == nil {
			t.Fatalf("accepted invalid webhook %q", endpoint)
		}
	}
}

func TestNamedEventNotificationValidation(t *testing.T) {
	raw, err := os.ReadFile("../../../../Examples/WendyDataPeople/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"person_detected", "door.open", "person-left"} {
		configured := strings.ReplaceAll(string(raw), "  on: event\n  event: person_detected", "  on: event\n  event: "+event+"\n  webhook: https://notify.example/events")
		campaign, err := ParseCampaign([]byte(configured))
		if err != nil {
			t.Fatal(err)
		}
		if campaign.Notify.Event != event {
			t.Fatal("event name was not preserved")
		}
	}
	for _, notify := range []string{
		"  on: event\n  webhook: https://notify.example/events",
		"  on: event\n  event: person detected\n  webhook: https://notify.example/events",
		"  on: episode_committed\n  event: person_detected",
		"  on: detection\n  event: person_detected\n  webhook: https://notify.example/events",
	} {
		if _, err := ParseCampaign([]byte(strings.ReplaceAll(string(raw), "  on: event\n  event: person_detected", notify))); err == nil {
			t.Fatalf("accepted invalid notification: %s", notify)
		}
	}
}

func TestCampaignImmediateNotificationDefaultsToCloud(t *testing.T) {
	for _, notify := range []string{"  on: detection", "  on: event\n  event: person_detected"} {
		raw := strings.ReplaceAll(string(peopleCampaign(t)), "  on: event\n  event: person_detected", notify)
		campaign, err := ParseCampaign([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if campaign.Notify.Webhook != "" {
			t.Fatal("Cloud notification unexpectedly requires a webhook")
		}
	}
}
