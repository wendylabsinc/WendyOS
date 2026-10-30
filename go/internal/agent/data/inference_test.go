package data

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func peopleCampaign(t *testing.T) []byte {
	t.Helper()
	contents, err := os.ReadFile("../../../../Examples/WendyDataPeople/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestYOLOInferenceValidationAndRevision(t *testing.T) {
	c, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	c.Inference.Backend = "yolo_onnx"
	c.Inference.ModelFile = "onnx/model.onnx"
	raw, _ := yaml.Marshal(c)
	parsed, err := ParseCampaign(raw)
	if err != nil {
		t.Fatal(err)
	}
	c.Inference.ModelFile = "onnx/other.onnx"
	raw, _ = yaml.Marshal(c)
	other, err := ParseCampaign(raw)
	if err != nil || parsed.Revision == other.Revision {
		t.Fatal("model file did not change immutable campaign revision", err)
	}
	for _, file := range []string{"", "../model.onnx", "/model.onnx", "onnx/../model.onnx", "model.pt", "onnx\\model.onnx"} {
		c.Inference.ModelFile = file
		raw, _ = yaml.Marshal(c)
		if _, err := ParseCampaign(raw); err == nil {
			t.Fatalf("accepted unsafe model file %q", file)
		}
	}
	c.Inference.Backend = "remote_python"
	c.Inference.ModelFile = "model.onnx"
	raw, _ = yaml.Marshal(c)
	if _, err := ParseCampaign(raw); err == nil {
		t.Fatal("accepted unsupported executable backend")
	}
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

func TestNotificationSettingsChangeCampaignRevision(t *testing.T) {
	raw := strings.ReplaceAll(string(peopleCampaign(t)), "  on: event\n  event: person_detected", "  on: event\n  event: person_detected\n  webhook: https://notify.example/events")
	base, err := ParseCampaign([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{"  event: person_detected\n  webhook:", "  event: door.open\n  webhook:"},
		{"https://notify.example/events", "https://notify.example/alerts"},
	} {
		changed, err := ParseCampaign([]byte(strings.ReplaceAll(raw, change[0], change[1])))
		if err != nil {
			t.Fatal(err)
		}
		if changed.Revision == base.Revision {
			t.Fatal("notification plan change did not change revision")
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

func TestClassificationTaskValidationAndRevision(t *testing.T) {
	c, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	oldRevision := c.Revision
	c.Inference.Task = "image-classification"
	raw, _ := yaml.Marshal(c)
	p, err := ParseCampaign(raw)
	if err != nil || p.Revision == oldRevision {
		t.Fatal("task must change campaign identity", err)
	}
	c.Inference.Backend = "yolo_onnx"
	c.Inference.ModelFile = "model.onnx"
	raw, _ = yaml.Marshal(c)
	if _, err = ParseCampaign(raw); err == nil {
		t.Fatal("YOLO detection backend accepted classification")
	}
	c.Inference.Task = "arbitrary-code"
	raw, _ = yaml.Marshal(c)
	if _, err = ParseCampaign(raw); err == nil {
		t.Fatal("unknown task accepted")
	}
}

func TestInferenceAcceleratorValidation(t *testing.T) {
	raw := string(peopleCampaign(t))
	yolo := strings.Replace(raw, "  model: facebook/detr-resnet-50", "  backend: yolo_onnx\n  model_file: onnx/model.onnx\n  model: facebook/detr-resnet-50", 1)
	for _, test := range []struct {
		yaml, accelerator, want string
		ok                      bool
	}{
		{raw, "", AcceleratorAuto, true},
		{raw, "auto", AcceleratorAuto, true},
		{raw, "cpu", AcceleratorCPU, true},
		{yolo, "tensorrt", AcceleratorTensorRT, true},
		// Only the YOLO ONNX backend has a TensorRT engine.
		{raw, "tensorrt", "", false},
		// Closed: no aliases, no other accelerators yet, no case folding.
		{yolo, "gpu", "", false},
		{yolo, "cuda", "", false},
		{yolo, "qnn", "", false},
		{yolo, "TensorRT", "", false},
	} {
		yaml := test.yaml
		if test.accelerator != "" {
			yaml = strings.Replace(yaml, "  model: facebook/detr-resnet-50", "  accelerator: "+test.accelerator+"\n  model: facebook/detr-resnet-50", 1)
		}
		campaign, err := ParseCampaign([]byte(yaml))
		if (err == nil) != test.ok {
			t.Fatalf("accelerator %q: %v", test.accelerator, err)
		}
		if err == nil && campaign.Inference.EffectiveAccelerator() != test.want {
			t.Fatalf("accelerator %q became %q", test.accelerator, campaign.Inference.EffectiveAccelerator())
		}
	}
}

// Campaigns deployed before accelerators existed must keep their revisions: a
// new revision would re-arm every deployed campaign on agent update. These are
// the example's revisions before the field was added.
func TestInferenceAcceleratorKeepsExistingRevisions(t *testing.T) {
	raw := string(peopleCampaign(t))
	yolo := strings.Replace(raw, "  model: facebook/detr-resnet-50", "  backend: yolo_onnx\n  model_file: onnx/model.onnx\n  model: facebook/detr-resnet-50", 1)
	for yaml, want := range map[string]string{
		raw:  "5bdd06fff93d5871fd29b77c85a8a9f17d28486b06c2a4af4d21fa610f1ca625",
		yolo: "b9c856a7a288e1217372382f1481e0d29abbc7666da832326ee4427687afca2c",
	} {
		campaign, err := ParseCampaign([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		if campaign.Revision != want {
			t.Fatalf("revision changed: %s, want %s", campaign.Revision, want)
		}
	}
	revisions := map[string]bool{}
	for _, accelerator := range []string{"", "auto", "cpu", "tensorrt"} {
		yaml := yolo
		if accelerator != "" {
			yaml = strings.Replace(yolo, "  backend: yolo_onnx", "  backend: yolo_onnx\n  accelerator: "+accelerator, 1)
		}
		campaign, err := ParseCampaign([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		if revisions[campaign.Revision] {
			t.Fatalf("accelerator %q did not change the revision", accelerator)
		}
		revisions[campaign.Revision] = true
	}
}
