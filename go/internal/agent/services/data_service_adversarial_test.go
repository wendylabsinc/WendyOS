package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestLegacyPersistedCampaignStillTriggersAfterUpgrade simulates an agent
// upgrade: a campaign persisted by the previous release (no version stamping
// at parse time, no capture/upload-policy/retention fields) must keep
// auto-triggering through the full service path without redeployment.
func TestLegacyPersistedCampaignStillTriggersAfterUpgrade(t *testing.T) {
	root := t.TempDir()
	legacy := `{
  "version": 1,
  "name": "legacy-flight",
  "sources": [
    {"telemetry": true}
  ],
  "capture": {
    "buffer": "10ms",
    "after_trigger": "30ms",
    "triggers": [
      {"event": "emergency_stop"}
    ]
  },
  "upload": {
    "when": "wifi",
    "destination": "s3://acme-ml/forklift"
  },
  "export": {"annotation": "cvat"},
  "models": {},
  "privacy": [],
  "state": "armed",
  "revision": "0123456789abcdef0123456789abcdef",
  "deployed_unix_nanos": 12345,
  "warnings": null
}`
	campaignsDir := filepath.Join(root, "campaigns")
	if err := os.MkdirAll(campaignsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(campaignsDir, "legacy-flight.json"), []byte(legacy), 0o640); err != nil {
		t.Fatal(err)
	}
	manager, err := data.NewManager(filepath.Join(root, "episodes"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	listed, err := service.Campaigns(context.Background(), &agentpbv2.DataCampaignsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.GetCampaigns()) != 1 || listed.GetCampaigns()[0].GetRevision() != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("legacy campaign not listed intact: %+v", listed.GetCampaigns())
	}
	if _, err := manager.RecordApplication("test.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "emergency_stop"}); err != nil {
		t.Fatal(err)
	}
	// The fixture deliberately declares no capture.drain, because a campaign
	// persisted by the previous release could not have. It therefore takes the
	// default post-seal drain on top of its 30ms after_trigger, and the wait
	// here has to allow for that.
	deadline := time.Now().Add(data.DefaultSealDrain + 3*time.Second)
	for time.Now().Before(deadline) {
		list, listErr := manager.List()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(list) == 1 {
			manifest, _, inspectErr := manager.Inspect(list[0].ID, false)
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			if manifest.Trigger.CampaignName != "legacy-flight" || manifest.Trigger.CampaignRevision != "0123456789abcdef0123456789abcdef" {
				t.Fatalf("legacy campaign identity lost on triggered episode: %+v", manifest.Trigger)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("legacy persisted campaign no longer triggers after the upgrade")
}

// deployTestCampaign arms a campaign whose episodes seal as soon as
// after_trigger expires. The post-seal drain is switched off because these
// tests exercise the campaign lifecycle rather than late application records,
// and the default two second drain would otherwise dominate every timing here.
func deployTestCampaign(t *testing.T, service *DataService, name, afterTrigger string) {
	t.Helper()
	yaml := fmt.Sprintf(`version: 1
name: %s
sources:
  - telemetry: true
capture:
  buffer: 10ms
  drain: 0s
  after_trigger: %s
  triggers:
    - event: trigger-%s
upload:
  when: manual
export:
  annotation: cvat
`, name, afterTrigger, name)
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(yaml)}); err != nil {
		t.Fatalf("deploying %s: %v", name, err)
	}
}

func TestStopPrefersAdHocThenRefusesAmbiguousCampaigns(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	// The ad-hoc Start RPC carries the default drain; this test is about which
	// episode Stop picks, not about waiting for one.
	service.adHocDrain = 0
	deployTestCampaign(t, service, "stop-a", "10s")
	deployTestCampaign(t, service, "stop-b", "10s")
	first, err := service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "stop-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "stop-b"})
	if err != nil {
		t.Fatal(err)
	}
	adHoc, err := service.Start(context.Background(), &agentpbv2.DataStartRequest{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := service.Stop(context.Background(), &agentpbv2.DataStopRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.GetId() != adHoc.GetId() {
		t.Fatalf("Stop finalized %s, want the ad-hoc episode %s", stopped.GetId(), adHoc.GetId())
	}
	// Two campaign episodes remain and no ad-hoc: Stop must refuse.
	if _, err = service.Stop(context.Background(), &agentpbv2.DataStopRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ambiguous Stop returned %v, want FailedPrecondition", err)
	}
	// A stale auto-stop guard must not finalize a different episode.
	service.stopCaptureIfCurrent(context.Background(), "stop-a", "not-the-active-episode")
	if session, ok := manager.ActiveSession("stop-a"); !ok || session.ID != first.GetId() {
		t.Fatal("stale auto-stop guard finalized the wrong episode")
	}
	// With the right episode it stops exactly that campaign's episode.
	service.stopCaptureIfCurrent(context.Background(), "stop-a", first.GetId())
	if _, ok := manager.ActiveSession("stop-a"); ok {
		t.Fatal("stopCaptureIfCurrent did not finalize the current episode")
	}
	// One campaign episode remains: Stop now finalizes it.
	stopped, err = service.Stop(context.Background(), &agentpbv2.DataStopRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.GetId() != second.GetId() {
		t.Fatalf("Stop finalized %s, want %s", stopped.GetId(), second.GetId())
	}
	if keys := manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("episodes still active: %v", keys)
	}
}

func TestServiceConcurrentTriggerRecordAndStopStress(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetWarnLogger(func(string) {})
	service := NewDataService(manager)
	// This test races the episode lifecycle, and its ad-hoc goroutine starts
	// and stops twenty applications episodes in a row. The default drain would
	// add two seconds to each of them without exercising anything this test is
	// about.
	service.adHocDrain = 0
	campaigns := []string{"stress-x", "stress-y", "stress-z"}
	for _, name := range campaigns {
		deployTestCampaign(t, service, name, "40ms")
	}
	var wg sync.WaitGroup
	// Records that match every campaign trigger, arriving concurrently.
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				for _, name := range campaigns {
					_, _ = manager.RecordApplication("stress.app", data.ApplicationRecord{Version: 1, Type: "event", Name: "trigger-" + name})
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	// Manual triggers racing the automatic ones.
	for _, name := range campaigns {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, _ = service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: name})
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}
	// Ad-hoc episodes and Stop RPCs racing the campaign lifecycle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_, _ = service.Start(context.Background(), &agentpbv2.DataStartRequest{Sources: []string{"applications"}})
			_, _ = service.Stop(context.Background(), &agentpbv2.DataStopRequest{})
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()
	// Quiesce: pending observer goroutines and after_trigger timers drain.
	deadline := time.Now().Add(5 * time.Second)
	stable := 0
	for time.Now().Before(deadline) && stable < 3 {
		if len(manager.ActiveEpisodeKeys()) == 0 {
			stable++
		} else {
			stable = 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	if keys := manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("episodes still active after stress: %v", keys)
	}
	list, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]bool{"": true}
	for _, name := range campaigns {
		valid[name] = true
	}
	seen := map[string]bool{}
	for _, episode := range list {
		if seen[episode.ID] {
			t.Fatalf("episode %s appears twice", episode.ID)
		}
		seen[episode.ID] = true
		if episode.State != "complete" && episode.State != "interrupted" {
			t.Fatalf("episode %s in state %q", episode.ID, episode.State)
		}
		manifest, _, err := manager.Inspect(episode.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if !valid[manifest.Trigger.CampaignName] {
			t.Fatalf("episode %s attributed to unknown campaign %q", episode.ID, manifest.Trigger.CampaignName)
		}
	}
}

func TestCampaignDeployWarnsAboutUnenforcedRetentionQuota(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	yaml := []byte(`version: 1
name: quota-warn
sources:
  - telemetry: true
capture:
  buffer: 10ms
  after_trigger: 30ms
  triggers:
    - event: emergency_stop
upload:
  when: wifi
retention:
  local_quota: 10GiB
export:
  annotation: cvat
`)
	campaign, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml})
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, warning := range campaign.GetWarnings() {
		warned = warned || strings.Contains(warning, "retention.local_quota")
	}
	if !warned {
		t.Fatalf("deploy accepted an unenforced retention.local_quota without warning: %v", campaign.GetWarnings())
	}
}
