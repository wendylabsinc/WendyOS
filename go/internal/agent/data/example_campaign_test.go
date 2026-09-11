package data

import (
	"os"
	"testing"
)

// TestShippedModelAppCampaignParses guards the real
// Examples/WendyDataModelApp/campaign.yaml against the campaign schema this
// agent actually parses. It fails when the shipped example and the schema
// drift apart, and asserts that the records the reference app emits fire the
// campaign's triggers.
func TestShippedModelAppCampaignParses(t *testing.T) {
	raw, err := os.ReadFile("../../../../Examples/WendyDataModelApp/campaign.yaml")
	if err != nil {
		t.Fatalf("reading model app campaign example: %v", err)
	}
	campaign, err := ParseCampaign(raw)
	if err != nil {
		t.Fatalf("model app campaign.yaml does not parse: %v", err)
	}

	if len(campaign.Sources) != 1 || campaign.Sources[0].Camera == "" {
		t.Fatalf("expected exactly one camera source, got %+v", campaign.Sources)
	}
	// The example captures the camera continuously and uncapped, so the
	// episode holds the very frames the model consumed through the harness.
	// A rate cap or snapshot interval here would be correct too, but the
	// episode would then hold payload bytes for only a subset of the samples
	// the model saw, which is a weaker demonstration of the contract.
	capture := campaign.Sources[0].Capture
	if capture.EffectiveMode() != "continuous" {
		t.Errorf("camera capture mode = %q, want continuous", capture.EffectiveMode())
	}
	if capture != nil && capture.Rate != 0 {
		t.Errorf("camera capture rate cap = %v, want none", capture.Rate)
	}
	if campaign.Upload.When != "always" {
		t.Errorf("upload.when = %q, want always", campaign.Upload.When)
	}
	if campaign.LocalQuotaBytes() <= 0 {
		t.Error("retention.local_quota did not parse to a positive quota")
	}
	if version, ok := campaign.Models["yolov8n"]; !ok || version == "" {
		t.Errorf("campaign must pin the yolov8n model version, got %v", campaign.Models)
	}

	// The event record the app sends when a person appears.
	if _, _, matched := campaign.Match(ApplicationRecord{Version: 1, Type: "event", Name: "person_detected"}); !matched {
		t.Error("person_detected event did not fire the campaign")
	}
	// A prediction record above the uncertainty threshold, in the shape the
	// app emits (uncertainty rides in attributes).
	uncertain := ApplicationRecord{
		Version:    1,
		Type:       "prediction",
		Model:      "yolov8n",
		Attributes: map[string]any{"uncertainty": 0.9, "model_version": "8.3.63"},
	}
	if _, _, matched := campaign.Match(uncertain); !matched {
		t.Error("high-uncertainty prediction did not fire the campaign")
	}
	confident := ApplicationRecord{
		Version:    1,
		Type:       "prediction",
		Model:      "yolov8n",
		Attributes: map[string]any{"uncertainty": 0.1},
	}
	if reason, _, matched := campaign.Match(confident); matched {
		t.Errorf("confident prediction unexpectedly fired the campaign (%s)", reason)
	}
}
