package data

import (
	"os"
	"testing"
	"time"
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
	if capture == nil {
		t.Fatal("the camera source declares no capture block; the assertions below would pass vacuously")
	}
	if capture.EffectiveMode() != "continuous" {
		t.Errorf("camera capture mode = %q, want continuous", capture.EffectiveMode())
	}
	if capture.Rate != 0 {
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
	// person_detected is deliberately the campaign's ONLY trigger. The app
	// scores uncertainty as 1 minus its best detection confidence, so a scene
	// the detector has no class for sits at exactly 1.0 and a
	// model.uncertainty threshold would fire on every prediction: measured on
	// a Jetson with nobody in frame, a fresh episode every 30 seconds. No
	// prediction may fire this campaign, whatever its uncertainty.
	uncertain := ApplicationRecord{
		Version:    1,
		Type:       "prediction",
		Model:      "yolov8n",
		Attributes: map[string]any{"uncertainty": 0.9, "model_version": "8.3.63"},
	}
	if reason, _, matched := campaign.Match(uncertain); matched {
		t.Errorf("uncertain prediction fired the campaign (%s); the example arms no model.uncertainty trigger", reason)
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

// TestShippedObjectsCampaignParses guards the real
// Examples/WendyDataObjects/campaign.yaml against the campaign schema this
// agent parses: both world view objects, the bottle's composition, the depth
// block, and the two trigger forms the example demonstrates.
func TestShippedObjectsCampaignParses(t *testing.T) {
	raw, err := os.ReadFile("../../../../Examples/WendyDataObjects/campaign.yaml")
	if err != nil {
		t.Fatalf("reading objects campaign example: %v", err)
	}
	campaign, err := ParseCampaign(raw)
	if err != nil {
		t.Fatalf("objects campaign.yaml does not parse: %v", err)
	}

	// Depth pairs only with a campaign that resolves to exactly one camera.
	if len(campaign.Sources) != 1 || campaign.Sources[0].Camera == "" || campaign.Sources[0].Camera == "*" {
		t.Fatalf("expected exactly one named camera source, got %+v", campaign.Sources)
	}
	if len(campaign.Objects) != 2 {
		t.Fatalf("objects = %v, want coke_can and bottle", sortedObjectNames(campaign.Objects))
	}

	can := campaign.Objects["coke_can"]
	if can == nil {
		t.Fatal("objects.coke_can is missing")
	}
	if can.Event != "coke_can_seen" || can.Rate != 4 || can.EveryFrames != 0 {
		t.Errorf("coke_can event, rate, every_frames = %q, %v, %d; want coke_can_seen, 4, 0", can.Event, can.Rate, can.EveryFrames)
	}
	if can.ClearDuration() != 3*time.Second || can.CooldownDuration() != 20*time.Second {
		t.Errorf("coke_can clear_after, cooldown = %s, %s; want 3s, 20s", can.ClearDuration(), can.CooldownDuration())
	}
	if can.Fusion.Threshold != 0.75 || len(can.Fusion.Required) != 1 || can.Fusion.Required[0] != "shape" {
		t.Errorf("coke_can fusion = %+v, want threshold 0.75 and required [shape]", can.Fusion)
	}
	for _, kind := range []string{"shape", "size", "colour"} {
		if can.Attributes[kind] == nil {
			t.Errorf("coke_can declares no %s attribute", kind)
		}
	}
	if colour := can.Attributes["colour"]; colour != nil {
		if palette, _ := colour.Expect["palette"].([]any); len(palette) != 2 {
			t.Errorf("coke_can palette has %d entries, want a red and a white or silver entry", len(palette))
		}
		if got := colour.ColourTolerance(); got != defaultColourTolerance {
			t.Errorf("coke_can colour tolerance = %v, want the default %v", got, defaultColourTolerance)
		}
	}

	bottle := campaign.Objects["bottle"]
	if bottle == nil {
		t.Fatal("objects.bottle is missing")
	}
	if bottle.Event != "bottle_seen" || bottle.EveryFrames != 4 || bottle.Rate != 0 {
		t.Errorf("bottle event, every_frames, rate = %q, %d, %v; want bottle_seen, 4, 0", bottle.Event, bottle.EveryFrames, bottle.Rate)
	}
	// Without a declared shape the composition would score but be unweighted.
	if bottle.Attributes["shape"] == nil || bottle.Attributes["size"] == nil {
		t.Errorf("bottle attributes = %v, want shape and size", bottle.Attributes)
	}
	wantParts := []string{"cylinder", "cone", "cylinder"}
	if len(bottle.Composition) != len(wantParts) {
		t.Fatalf("bottle composition has %d parts, want %d", len(bottle.Composition), len(wantParts))
	}
	for i, part := range bottle.Composition {
		if part.Primitive != wantParts[i] {
			t.Errorf("bottle composition[%d] = %q, want %q", i, part.Primitive, wantParts[i])
		}
	}

	depth := campaign.Depth
	if depth == nil {
		t.Fatal("the example declares no depth block")
	}
	if depth.Source == "" || depth.Source == campaign.Sources[0].Camera {
		t.Errorf("depth.source = %q, want a stream other than the colour camera %q", depth.Source, campaign.Sources[0].Camera)
	}
	if depth.ScaleM != 0.001 {
		t.Errorf("depth.scale_m = %v, want 0.001 for millimetre depth", depth.ScaleM)
	}
	// Placeholder intrinsics for an 848x480 frame: the principal point sits
	// inside it.
	if in := depth.Intrinsics; in.Fx <= 0 || in.Fy <= 0 || in.Cx <= 0 || in.Cx >= 848 || in.Cy <= 0 || in.Cy >= 480 {
		t.Errorf("depth.intrinsics = %+v, want positive values for an 848x480 frame", in)
	}

	prediction := func(object string, confidence float64) ApplicationRecord {
		return ApplicationRecord{Version: 1, Type: "prediction", Model: "worldview",
			Attributes: map[string]any{"object": object, "confidence": confidence}}
	}
	if _, expression, matched := campaign.Match(prediction("bottle", 0.9)); !matched {
		t.Error("a bottle prediction at confidence 0.9 did not fire the campaign")
	} else if expression != `object.bottle.confidence > 0.8` {
		t.Errorf("bottle trigger expression = %q", expression)
	}
	if reason, _, matched := campaign.Match(prediction("bottle", 0.7)); matched {
		t.Errorf("a bottle prediction at confidence 0.7 fired the campaign (%s)", reason)
	}
	// The can starts episodes through its event only, not its confidence.
	if reason, _, matched := campaign.Match(prediction("coke_can", 0.99)); matched {
		t.Errorf("a coke_can prediction fired the campaign (%s); only coke_can_seen should", reason)
	}
	if _, _, matched := campaign.Match(ApplicationRecord{Version: 1, Type: "event", Name: "coke_can_seen", Model: "worldview"}); !matched {
		t.Error("the coke_can_seen event did not fire the campaign")
	}
	if reason, _, matched := campaign.Match(ApplicationRecord{Version: 1, Type: "event", Name: "bottle_seen", Model: "worldview"}); matched {
		t.Errorf("the bottle_seen event fired the campaign (%s); the example arms no trigger for it", reason)
	}

	if campaign.Notify == nil || campaign.Notify.On != NotifyOnEvent || campaign.Notify.Event != "coke_can_seen" {
		t.Errorf("notify = %+v, want on: event for coke_can_seen", campaign.Notify)
	}
}
