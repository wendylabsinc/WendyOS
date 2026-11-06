package commands

import (
	"math"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

func TestDesktopCampaignHumanPlan(t *testing.T) {
	raw, err := DesktopCampaignYAML(DesktopCampaignInput{Name: "human-detected", Camera: "*", RunInference: true, Notify: true, Threshold: .9, Cooldown: 30, Before: 10, After: 10})
	if err != nil {
		t.Fatal(err)
	}
	p, err := data.ParseCampaign(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sources[0].Camera != "*" || p.Sources[0].Capture.Mode != "continuous" || p.Inference.Labels[0] != "person" || len(p.Inference.Revision) != 40 {
		t.Fatalf("wrong detector: %+v", p)
	}
	if p.Notify.Event != p.Inference.Event || p.Capture.Triggers[0].Event != p.Inference.Event {
		t.Fatal("notification and capture must match the emitted detection event")
	}
	if p.Upload.When != "manual" || p.Capture.Buffer != "10s" || p.Capture.AfterTrigger != "10s" {
		t.Fatal("default clips should stay local with bounded pre/post capture")
	}
}
func TestDesktopCampaignLiteralSourceAndAppEvent(t *testing.T) {
	camera := "sensorlink:camera-1:camera\nnotify: injected"
	raw, err := DesktopCampaignYAML(DesktopCampaignInput{Name: "door", Camera: camera, Event: "door_opened", After: 5})
	if err != nil {
		t.Fatal(err)
	}
	p, err := data.ParseCampaign(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sources[0].Camera != camera || p.Notify != nil || p.Inference != nil || p.Capture.Triggers[0].Event != "door_opened" {
		t.Fatal("input must stay literal and recording-only must not install a detector or send notifications")
	}
}
func TestDesktopCampaignRejectsInvalidOptions(t *testing.T) {
	base := DesktopCampaignInput{Name: "people", Camera: "*", RunInference: true, Notify: true, Threshold: .9, Cooldown: 30, Before: 10, After: 10}
	for _, change := range []func(*DesktopCampaignInput){
		func(p *DesktopCampaignInput) { p.Name = "../bad" }, func(p *DesktopCampaignInput) { p.Camera = "" },
		func(p *DesktopCampaignInput) { p.Before = 301 }, func(p *DesktopCampaignInput) { p.After = 0 },
		func(p *DesktopCampaignInput) { p.Threshold = math.NaN() }, func(p *DesktopCampaignInput) { p.Cooldown = 0 },
		func(p *DesktopCampaignInput) { p.RunInference = false; p.Event = "" },
	} {
		p := base
		change(&p)
		if _, err := DesktopCampaignYAML(p); err == nil {
			t.Fatalf("accepted invalid campaign: %+v", p)
		}
	}
}

func TestDesktopCampaignClassifierAndMultipleLiteralLabels(t *testing.T) {
	raw, err := DesktopCampaignYAML(DesktopCampaignInput{Name: "animals", Camera: "*", RunInference: true, Classification: true, Model: "google/vit-base-patch16-224", ModelRevision: "3f49326eb077187dfe1c2a2bb15fbd74e6ab91e3", Labels: []string{"cat, tabby", "dog"}, Event: "animal_seen", Notify: true, Threshold: .8, Cooldown: 30, After: 10})
	if err != nil {
		t.Fatal(err)
	}
	p, err := data.ParseCampaign(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Inference.Task != "image-classification" || p.Inference.Labels[0] != "cat, tabby" || p.Notify.Event != "animal_seen" || p.Capture.Triggers[0].Event != "animal_seen" {
		t.Fatalf("bad classifier plan: %+v", p)
	}
}

func TestDesktopCampaignSensorSelection(t *testing.T) {
	in := DesktopCampaignInput{Name: "sensors", Event: "capture", After: 10, Audio: []string{"audio:mic"}, ROS2: []string{"ros2:rmw_cyclonedds_cpp:domain-42:/scan"}, Telemetry: true}
	raw, err := DesktopCampaignYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := data.ParseCampaign(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 3 || plan.Sources[0].Audio != "audio:mic" || plan.Sources[1].ROS2 != in.ROS2[0] || !plan.Sources[2].Telemetry {
		t.Fatalf("lost source selection: %+v", plan.Sources)
	}
	in.RunInference = true
	if _, err := DesktopCampaignYAML(in); err == nil {
		t.Fatal("allowed vision inference without a camera")
	}
}
