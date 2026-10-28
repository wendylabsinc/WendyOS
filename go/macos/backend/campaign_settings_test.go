package backend

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"strings"
	"testing"
)

func campaignSettingsFixture(t *testing.T) *pb.DataCampaign {
	t.Helper()
	plan, err := data.ParseCampaign([]byte(`version: 1
name: test
fleet: factory
sources:
  - camera: '*'
    capture:
      mode: continuous
capture:
  buffer: 10s
  after_trigger: 10s
  drain: 3s
  triggers:
    - event: door_opened
upload:
  when: wifi
  destination: training
  max_rate: 5MB/s
retention:
  local_quota: 10GiB
export:
  annotation: none
notify:
  on: event
  event: door_opened
`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return &pb.DataCampaign{Name: plan.Name, Revision: plan.Revision, PlanJson: raw}
}
func TestCampaignSettingsPreservesUneditedConfiguration(t *testing.T) {
	current := campaignSettingsFixture(t)
	input, err := campaignSettings(current)
	if err != nil {
		t.Fatal(err)
	}
	input.Before = "20s"
	input.After = "30s"
	input.Upload = "manual"
	raw, err := patchCampaignSettings(current, input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := data.ParseCampaign(raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.Capture.Buffer != "20s" || result.Capture.AfterTrigger != "30s" || result.Upload.When != "manual" {
		t.Fatal("changes not applied")
	}
	if result.Fleet != "factory" || result.Capture.Drain != "3s" || result.Capture.Triggers[0].Event != "door_opened" || result.Upload.Destination != "training" || result.Upload.MaxRate != "5MB/s" || result.Retention.LocalQuota != "10GiB" || result.Notify.Event != "door_opened" || result.Sources[0].Camera != "*" {
		t.Fatalf("lost configuration: %+v", result)
	}
}
func TestCampaignSettingsRejectsStaleOrInvalidEdits(t *testing.T) {
	current := campaignSettingsFixture(t)
	input, _ := campaignSettings(current)
	stale := input
	stale.Revision = "old"
	if _, err := patchCampaignSettings(current, stale); err == nil {
		t.Fatal("stale edit accepted")
	}
	invalid := input
	invalid.After = "-1s"
	if _, err := patchCampaignSettings(current, invalid); err == nil {
		t.Fatal("invalid duration accepted")
	}
	current.PlanJson = []byte(strings.Replace(string(current.PlanJson), `"version":1`, `"future_setting":true,"version":1`, 1))
	if _, err := patchCampaignSettings(current, input); err == nil {
		t.Fatal("unknown future field silently lost")
	}
	b, _ := settingsBackend(t)
	for _, session := range []uint64{0, 6} {
		if err := b.UpdateCampaignSettings(session, input); err == nil {
			t.Fatal("invalid session accepted")
		}
	}
}
