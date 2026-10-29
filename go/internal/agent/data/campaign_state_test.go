package data

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const stateTestCampaignYAML = `version: 1
name: dock-door
fleet: warehouse-west
sources:
  - telemetry: true
capture:
  buffer: 0s
  after_trigger: 5s
  triggers:
    - event: door_open
upload:
  when: manual
  max_rate: 1MB/s
export:
  annotation: cvat
`

// newStateTestManager roots the store one level down, so the campaign
// directory (a sibling of the store) belongs to this test alone.
func newStateTestManager(t *testing.T, root string) *Manager {
	t.Helper()
	m, err := NewManager(filepath.Join(root, "episodes"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDisarmedStatePersistsAcrossARestartAndKeepsTheRevision(t *testing.T) {
	root := t.TempDir()
	m := newStateTestManager(t, root)
	deployed, err := m.DeployCampaign([]byte(stateTestCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	disarmed, changed, err := m.SetCampaignState("dock-door", CampaignStateDisarmed)
	if err != nil || !changed || disarmed.State != CampaignStateDisarmed {
		t.Fatalf("disarm = %+v, %v, %v", disarmed, changed, err)
	}
	if _, changed, err := m.SetCampaignState("dock-door", CampaignStateDisarmed); err != nil || changed {
		t.Fatalf("disarming twice: changed=%v err=%v, want an unchanged success", changed, err)
	}

	// A new manager over the same directory is what the agent builds at boot.
	restarted := newStateTestManager(t, root)
	campaigns, err := restarted.Campaigns()
	if err != nil || len(campaigns) != 1 {
		t.Fatalf("campaigns after restart = %+v, %v", campaigns, err)
	}
	got := campaigns[0]
	if got.State != CampaignStateDisarmed {
		t.Fatalf("state after restart = %q, want disarmed", got.State)
	}
	if got.Revision != deployed.Revision || got.DeployedUnixNanos != deployed.DeployedUnixNanos {
		t.Fatalf("disarming edited the plan: revision %s -> %s", deployed.Revision, got.Revision)
	}

	armed, changed, err := restarted.SetCampaignState("dock-door", CampaignStateArmed)
	if err != nil || !changed || armed.State != CampaignStateArmed || armed.Revision != deployed.Revision {
		t.Fatalf("arm = %+v, %v, %v", armed, changed, err)
	}
}

func TestSetCampaignStateRefusesUnknownStatesAndCampaigns(t *testing.T) {
	m := newStateTestManager(t, t.TempDir())
	if _, err := m.DeployCampaign([]byte(stateTestCampaignYAML)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SetCampaignState("dock-door", "paused"); err == nil {
		t.Fatal("an unknown state was stored")
	}
	if _, _, err := m.SetCampaignState("missing", CampaignStateDisarmed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disarming a campaign that is not deployed: %v, want os.ErrNotExist", err)
	}
	if _, _, err := m.SetCampaignState("../escape", CampaignStateDisarmed); !errors.Is(err, ErrInvalidCampaignName) {
		t.Fatalf("a path as a campaign name: %v, want ErrInvalidCampaignName", err)
	}
}

// TestSetCampaignStateKeepsFieldsThisAgentDoesNotKnow: after a rollback, an
// older agent can find a plan a newer one wrote. Switching it off and on must
// not strip what the older agent cannot read.
func TestSetCampaignStateKeepsFieldsThisAgentDoesNotKnow(t *testing.T) {
	m := newStateTestManager(t, t.TempDir())
	if _, err := m.DeployCampaign([]byte(stateTestCampaignYAML)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.campaignDir(), "dock-door.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "{", `{"rules": [{"area": "dock"}],`, 1))
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SetCampaignState("dock-door", CampaignStateDisarmed); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `"area": "dock"`) || !strings.Contains(string(after), `"state": "disarmed"`) {
		t.Fatalf("stored plan after disarm:\n%s", after)
	}
}

// TestRedeployingADisarmedCampaignKeepsItDisarmed pins the documented rule:
// deploying a plan never switches on a campaign an operator switched off.
func TestRedeployingADisarmedCampaignKeepsItDisarmed(t *testing.T) {
	m := newStateTestManager(t, t.TempDir())
	if _, err := m.DeployCampaign([]byte(stateTestCampaignYAML)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SetCampaignState("dock-door", CampaignStateDisarmed); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(stateTestCampaignYAML, "after_trigger: 5s", "after_trigger: 9s", 1)
	redeployed, err := m.DeployCampaign([]byte(edited))
	if err != nil {
		t.Fatal(err)
	}
	if redeployed.State != CampaignStateDisarmed {
		t.Fatalf("redeploy state = %q, want disarmed", redeployed.State)
	}
	if len(redeployed.Warnings) == 0 || !strings.Contains(redeployed.Warnings[len(redeployed.Warnings)-1], "wendy data campaign arm dock-door") {
		t.Fatalf("redeploy did not say how to arm it: %q", redeployed.Warnings)
	}
	stored, err := m.Campaign("dock-door")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != CampaignStateDisarmed || stored.Capture.AfterTrigger != "9s" {
		t.Fatalf("stored plan = state %q, after_trigger %q; want the new plan, disarmed", stored.State, stored.Capture.AfterTrigger)
	}
	for _, warning := range stored.Warnings {
		if strings.Contains(warning, "disarmed") {
			t.Fatalf("the redeploy note was stored with the plan, where it outlives the next arm: %q", warning)
		}
	}

	// An armed campaign, and a campaign deployed for the first time, are armed.
	if _, _, err := m.SetCampaignState("dock-door", CampaignStateArmed); err != nil {
		t.Fatal(err)
	}
	if again, err := m.DeployCampaign([]byte(stateTestCampaignYAML)); err != nil || again.State != CampaignStateArmed {
		t.Fatalf("redeploying an armed campaign = %q, %v", again.State, err)
	}
}

// TestRemoveCampaignDeletesThePlanAndKeepsItsUploadPolicy: the plan goes, the
// episodes stay, and they keep uploading under the policy their plan named.
func TestRemoveCampaignDeletesThePlanAndKeepsItsUploadPolicy(t *testing.T) {
	root := t.TempDir()
	m := newStateTestManager(t, root)
	deployed, err := m.DeployCampaign([]byte(stateTestCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	episodeDir := filepath.Join(m.root, "20261027T000000Z-kept")
	if err := os.MkdirAll(episodeDir, 0o750); err != nil {
		t.Fatal(err)
	}

	removed, err := m.RemoveCampaign("dock-door", "")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Revision != deployed.Revision || removed.Fleet != "warehouse-west" {
		t.Fatalf("removed = %+v", removed)
	}
	if _, err := m.Campaign("dock-door"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the plan is still readable after remove: %v", err)
	}
	restarted := newStateTestManager(t, root)
	if campaigns, err := restarted.Campaigns(); err != nil || len(campaigns) != 0 {
		t.Fatalf("campaigns after remove and restart = %+v, %v", campaigns, err)
	}
	if _, err := os.Stat(episodeDir); err != nil {
		t.Fatalf("removing the campaign touched the episode store: %v", err)
	}

	policy, err := restarted.CampaignForUpload("dock-door")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Upload.When != "manual" || policy.UploadMaxRateBytes() != 1_000_000 || policy.Revision != deployed.Revision {
		t.Fatalf("upload policy kept for the removed campaign = %+v", policy)
	}
	if _, err := restarted.RemoveCampaign("dock-door", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removing twice: %v, want os.ErrNotExist", err)
	}
	if _, err := restarted.CampaignForUpload("never-deployed"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a campaign never deployed: %v, want os.ErrNotExist", err)
	}

	// Deploying the name again starts a fresh, armed campaign whose own plan
	// decides its upload policy.
	fresh, err := restarted.DeployCampaign([]byte(strings.Replace(stateTestCampaignYAML, "when: manual", "when: always", 1)))
	if err != nil || fresh.State != CampaignStateArmed {
		t.Fatalf("redeploy after remove = %q, %v", fresh.State, err)
	}
	if policy, err := restarted.CampaignForUpload("dock-door"); err != nil || policy.Upload.When != "always" {
		t.Fatalf("upload policy after redeploy = %+v, %v", policy.Upload, err)
	}
}
