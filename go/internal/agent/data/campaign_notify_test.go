package data

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// campaignWithNotify appends a top-level notify block to the minimal valid
// campaign document.
func campaignWithNotify(notify string) []byte {
	return append(minimalCampaign(1, "telemetry: true", "wifi"), []byte(notify+"\n")...)
}

// TestNotifyBlockRoundTripsIntoManifestTrigger covers the whole device-side
// life of the notify block: parsed from YAML, persisted with the deployed
// plan, copied onto the episode trigger, and serialized into the manifest
// JSON at trigger.notify.on, which is the path the cloud ingest service reads
// out of attributes_json.
func TestNotifyBlockRoundTripsIntoManifestTrigger(t *testing.T) {
	campaign, err := ParseCampaign(campaignWithNotify("notify: {on: episode_committed}"))
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Notify == nil || campaign.Notify.On != NotifyOnEpisodeCommitted {
		t.Fatalf("notify block was not parsed: %+v", campaign.Notify)
	}

	manifest := Manifest{
		Version: ManifestVersion,
		ID:      "ep-1",
		Trigger: EpisodeTrigger{
			Reason:           "event:go",
			CampaignName:     campaign.Name,
			CampaignRevision: campaign.Revision,
			Notify:           campaign.Notify,
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"notify":{"on":"episode_committed"}`) {
		t.Fatalf("manifest JSON does not carry the notify block verbatim: %s", raw)
	}
	var decoded Manifest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Trigger.Notify == nil || decoded.Trigger.Notify.On != NotifyOnEpisodeCommitted {
		t.Fatalf("notify block did not survive the manifest round trip: %+v", decoded.Trigger.Notify)
	}

	// A campaign without notify must leave the manifest exactly as before:
	// no notify key at all, not an empty one.
	bare, err := json.Marshal(Manifest{Version: ManifestVersion, ID: "ep-2", Trigger: EpisodeTrigger{Reason: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), `"notify"`) {
		t.Fatalf("manifest without a notify block still mentions notify: %s", bare)
	}
}

func TestNotifyRejectsUnsupportedOn(t *testing.T) {
	if _, err := ParseCampaign(campaignWithNotify("notify: {on: episode_sealed}")); !errors.Is(err, ErrUnsupportedNotifyOn) {
		t.Fatalf("unsupported notify.on accepted or misclassified: %v", err)
	}
	if _, err := ParseCampaign(campaignWithNotify("notify: {}")); !errors.Is(err, ErrUnsupportedNotifyOn) {
		t.Fatalf("notify without on accepted or misclassified: %v", err)
	}
	if _, err := ParseCampaign(campaignWithNotify("notify: episode_committed")); err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Fatalf("non-mapping notify accepted or misdescribed: %v", err)
	}
}

func TestDeployWarnsOnUnknownNotifyKeys(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := manager.DeployCampaign(campaignWithNotify("notify: {on: episode_committed, channel: slack}"))
	if err != nil {
		t.Fatalf("unknown notify key must warn, not fail deployment: %v", err)
	}
	found := false
	for _, warning := range campaign.Warnings {
		if strings.Contains(warning, "notify") && strings.Contains(warning, "channel") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning names the unknown notify key: %v", campaign.Warnings)
	}
	stored, err := manager.Campaign(campaign.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Notify == nil || stored.Notify.On != NotifyOnEpisodeCommitted {
		t.Fatalf("notify block was not durably stored with the plan: %+v", stored.Notify)
	}

	clean, err := manager.DeployCampaign(campaignWithNotify("notify: {on: episode_committed}"))
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range clean.Warnings {
		if strings.Contains(warning, "notify") {
			t.Fatalf("a fully known notify block must not warn: %v", clean.Warnings)
		}
	}
}

func TestNotifyChangesRevisionHash(t *testing.T) {
	base, err := ParseCampaign(minimalCampaign(1, "telemetry: true", "wifi"))
	if err != nil {
		t.Fatal(err)
	}
	notified, err := ParseCampaign(campaignWithNotify("notify: {on: episode_committed}"))
	if err != nil {
		t.Fatal(err)
	}
	if base.Revision == notified.Revision {
		t.Fatal("adding a notify block did not change the revision hash")
	}
}
