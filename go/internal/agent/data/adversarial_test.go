package data

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// legacyCampaignJSON is the exact persisted shape written by the previous
// agent release (before per-source capture, upload policy, and retention
// existed). Upgraded agents must keep loading and triggering these plans.
const legacyCampaignJSON = `{
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

func writeCampaignFile(t *testing.T, m *Manager, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(m.campaignDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.campaignDir(), name+".json"), []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestPersistedCampaignsFromOlderAgentsSurviveUpgrade(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(filepath.Join(root, "episodes"))
	if err != nil {
		t.Fatal(err)
	}
	writeCampaignFile(t, m, "legacy-flight", legacyCampaignJSON)
	// A plan persisted without any version field (or hand-recovered state) must
	// also keep loading: reload is trusted durable state, not author input.
	noVersion := strings.Replace(legacyCampaignJSON, "\"version\": 1,\n  ", "", 1)
	noVersion = strings.Replace(noVersion, "legacy-flight", "legacy-zero", 1)
	writeCampaignFile(t, m, "legacy-zero", noVersion)

	campaigns, err := m.Campaigns()
	if err != nil {
		t.Fatal(err)
	}
	if len(campaigns) != 2 {
		t.Fatalf("campaigns = %d, want 2 (old persisted plans must reload): %+v", len(campaigns), campaigns)
	}
	for _, campaign := range campaigns {
		if campaign.State != "armed" {
			t.Fatalf("campaign %s state = %q, want armed", campaign.Name, campaign.State)
		}
		if campaign.Revision != "0123456789abcdef0123456789abcdef" {
			t.Fatalf("campaign %s revision was not preserved verbatim: %q", campaign.Name, campaign.Revision)
		}
		reason, expression, matched := campaign.Match(ApplicationRecord{Version: 1, Type: "event", Name: "emergency_stop"})
		if !matched || reason == "" || expression == "" {
			t.Fatalf("campaign %s no longer matches its trigger after upgrade", campaign.Name)
		}
		if _, _, _, err := m.ResolveCampaignSources(campaign); err != nil {
			t.Fatalf("campaign %s sources no longer resolve: %v", campaign.Name, err)
		}
	}
	// The full trigger path still works for a legacy plan.
	legacy, err := m.Campaign("legacy-flight")
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications", "telemetry"}, Trigger: EpisodeTrigger{Reason: "event:emergency_stop", CampaignName: legacy.Name, CampaignRevision: legacy.Revision}})
	if err != nil {
		t.Fatalf("legacy campaign can no longer start an episode: %v", err)
	}
	stopped, err := m.Stop(legacy.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.ID != started.ID || stopped.Trigger.CampaignRevision != legacy.Revision {
		t.Fatalf("legacy campaign episode lost identity: %+v", stopped.Trigger)
	}
}

func TestCampaignRevisionDeterministicAndPreservedOnReload(t *testing.T) {
	yaml := []byte(`version: 1
name: rev-check
sources:
  - telemetry: true
capture:
  buffer: 10ms
  after_trigger: 30ms
  triggers:
    - event: emergency_stop
upload:
  when: wifi
  max_rate: 5MB/s
retention:
  local_quota: 10GiB
export:
  annotation: cvat
models:
  detector: v4
  planner: v9
`)
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.DeployCampaign(yaml)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.DeployCampaign(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == "" || first.Revision != second.Revision {
		t.Fatalf("same plan produced different revisions: %q vs %q", first.Revision, second.Revision)
	}
	reloaded, err := m.Campaign("rev-check")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision != first.Revision {
		t.Fatalf("reload recomputed or lost the revision: %q vs %q", reloaded.Revision, first.Revision)
	}
	parsed, err := ParseCampaign(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Revision != first.Revision {
		t.Fatalf("ParseCampaign revision differs from deploy: %q vs %q", parsed.Revision, first.Revision)
	}
}

func TestParseByteSizeAndRateEdgeCases(t *testing.T) {
	sizes := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"500", 500, false},
		{"1.5MB", 1500000, false},
		{"10GiB", 10 << 30, false},
		{"10 GiB", 10 << 30, false},
		{"512KiB", 512 << 10, false},
		{"2TB", 2e12, false},
		{"0", 0, true},
		{"0MB", 0, true},
		{"-1GiB", 0, true},
		{"5MBps", 0, true},
		{"1PB", 0, true},
		{"MB", 0, true},
		{"", 0, true},
		{"1..5MB", 0, true},
		// Values that overflow int64 must be rejected, not silently converted
		// to an implementation-defined (possibly negative) quota.
		{"999999999999999999GiB", 0, true},
		{"16EiB", 0, true},
		{"9300000000000000000", 0, true},
	}
	for _, c := range sizes {
		got, err := parseByteSize(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseByteSize(%q) = %d, want error", c.raw, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseByteSize(%q) = %d, %v, want %d", c.raw, got, err, c.want)
		}
	}
	rates := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"5MB/s", 5e6, false},
		{"5mb/s", 5e6, false},
		{"5MB", 5e6, false},
		{"1GiB/s", 1 << 30, false},
		{"250000", 250000, false},
		{"5MBps", 0, true},
		{"-5MB/s", 0, true},
		{"/s", 0, true},
	}
	for _, c := range rates {
		got, err := parseByteRate(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseByteRate(%q) = %d, want error", c.raw, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseByteRate(%q) = %d, %v, want %d", c.raw, got, err, c.want)
		}
	}
}

func TestValidationErrorsNameTheField(t *testing.T) {
	base := `version: 1
name: field-names
sources:
  - telemetry: true
capture:
  buffer: 10ms
  after_trigger: 30ms
  triggers:
    - event: emergency_stop
upload:
  when: wifi
%s
export:
  annotation: cvat
`
	cases := []struct {
		fragment string
		field    string
	}{
		{"  max_rate: 5MBps\n", "upload.max_rate"},
		{"  max_rate: -1MB/s\n", "upload.max_rate"},
		{"retention:\n  local_quota: banana\n", "retention.local_quota"},
		{"retention:\n  local_quota: \"0\"\n", "retention.local_quota"},
	}
	for _, c := range cases {
		_, err := ParseCampaign([]byte(fmt.Sprintf(base, c.fragment)))
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("fragment %q: error %v does not name field %s", c.fragment, err, c.field)
		}
	}
}

func TestThresholdParserEdgeCases(t *testing.T) {
	cases := []struct {
		expression string
		field      string
		operator   string
		value      float64
		wantErr    bool
	}{
		{"level_db > -20", "level_db", ">", -20, false},
		{"level_db>-20", "level_db", ">", -20, false},
		{"  model.uncertainty   >   0.9  ", "model.uncertainty", ">", 0.9, false},
		{"model.uncertainty > .9", "model.uncertainty", ">", 0.9, false},
		{"model.uncertainty >= 0", "model.uncertainty", ">=", 0, false},
		{"model.uncertainty <= 1", "model.uncertainty", "<=", 1, false},
		{"model.uncertainty == 0.5", "model.uncertainty", "==", 0.5, false},
		{"model.uncertainty > 1.5", "", "", 0, true},
		{"model.uncertainty < -0.1", "", "", 0, true},
		{"level dB > 3", "", "", 0, true},
		{"garbage", "", "", 0, true},
		{"> 5", "", "", 0, true},
		{"level_db >", "", "", 0, true},
		// NaN and infinities are not comparable thresholds; accepting them
		// arms a trigger that can never fire (NaN) or always fires (Inf).
		{"model.uncertainty > NaN", "", "", 0, true},
		{"level_db > NaN", "", "", 0, true},
		{"level_db < +Inf", "", "", 0, true},
		{"level_db > -Inf", "", "", 0, true},
	}
	for _, c := range cases {
		field, operator, value, err := parseFieldThreshold(c.expression)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseFieldThreshold(%q) = %s %s %g, want error", c.expression, field, operator, value)
			}
			continue
		}
		if err != nil || field != c.field || operator != c.operator || value != c.value {
			t.Errorf("parseFieldThreshold(%q) = %s %s %g, %v; want %s %s %g", c.expression, field, operator, value, err, c.field, c.operator, c.value)
		}
	}
	// The campaign-level trigger keeps the original strict 0..1 rejection for
	// model.uncertainty (a behavior the previous release enforced too).
	if _, _, err := parseThreshold("model.uncertainty", "> 1.5"); err == nil {
		t.Error("campaign trigger accepted model.uncertainty > 1.5")
	}
	if _, _, err := parseThreshold("model.uncertainty", "> NaN"); err == nil {
		t.Error("campaign trigger accepted model.uncertainty > NaN")
	}
}

func TestQuotaEvictionEdgeCases(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	m.SetWarnLogger(func(msg string) { warnings = append(warnings, msg) })

	first := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:a", CampaignName: "camp-a"}})
	second := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:b", CampaignName: "camp-b"}})
	third := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:c", CampaignName: "camp-c"}})
	for id, campaign := range map[string]string{first.ID: "camp-a", second.ID: "camp-b", third.ID: "camp-c"} {
		setUploadState(t, m, id, "pending", campaign)
	}
	// Equal start timestamps must not break the ordering.
	for _, id := range []string{first.ID, second.ID} {
		dir, err := m.episodeDir(id)
		if err != nil {
			t.Fatal(err)
		}
		mf, err := readManifest(dir)
		if err != nil {
			t.Fatal(err)
		}
		mf.StartedUnixNanos = 42
		if err := writeManifest(dir, mf); err != nil {
			t.Fatal(err)
		}
	}
	// Every candidate awaits upload and the quota holds roughly one episode:
	// eviction must still make room, warn once per sacrificed episode, and
	// never fail to start the new capture.
	m.SetQuota(episodeSize(t, m, third.ID)+1, 0)
	fourth := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	if _, err := m.episodeDir(fourth.ID); err != nil {
		t.Fatal(err)
	}
	evicted := 0
	for _, id := range []string{first.ID, second.ID, third.ID} {
		if _, err := m.episodeDir(id); err != nil {
			evicted++
		}
	}
	if evicted == 0 {
		t.Fatal("no pending episode was evicted despite an overflowing quota")
	}
	if len(warnings) != evicted {
		t.Fatalf("warnings = %d, want exactly one per evicted episode (%d): %v", len(warnings), evicted, warnings)
	}
	// A quota smaller than any single episode evicts everything sealed and
	// still admits the new episode rather than deadlocking capture.
	warnings = warnings[:0]
	m.SetQuota(1, 0)
	fifth, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatalf("start under minimal quota: %v", err)
	}
	if _, err := m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := m.episodeDir(fourth.ID); err == nil {
		t.Fatal("sealed episode survived a quota that cannot hold it")
	}
	_ = fifth
}

func TestRecordsRouteOnlyToEpisodesCapturingApplications(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	telemetryOnly, err := m.Start(StartOptions{Sources: []string{"telemetry"}})
	if err != nil {
		t.Fatal(err)
	}
	withApps, err := m.Start(StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:x", CampaignName: "apps"}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.RecordApplication("test.app", ApplicationRecord{Version: 1, Type: "event", Name: "shared"})
	if err != nil || state != "recorded" {
		t.Fatalf("record state = %q, %v", state, err)
	}
	telemetryManifest, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	appsManifest, err := m.Stop("apps")
	if err != nil {
		t.Fatal(err)
	}
	if telemetryManifest.ID != telemetryOnly.ID || appsManifest.ID != withApps.ID {
		t.Fatal("episode identity mixed up across stops")
	}
	for _, file := range telemetryManifest.Files {
		if file.Path != "events.jsonl" {
			continue
		}
		if file.Size != 0 {
			t.Fatalf("episode that excluded the applications source captured application records: %+v", file)
		}
	}
	found := false
	for _, file := range appsManifest.Files {
		if file.Path == "events.jsonl" && file.Size > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("episode with the applications source did not capture the record: %+v", appsManifest.Files)
	}
}

func TestRecoverMultiplePartialsAcrossCampaigns(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	episodes := map[string]Manifest{}
	for _, campaign := range []string{"camp-a", "camp-b", ""} {
		upload := WorkflowState{State: "local"}
		if campaign != "" {
			upload = WorkflowState{State: "pending", Destination: "example-episodes"}
		}
		started, err := m.Start(StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:crash", CampaignName: campaign}, Upload: upload})
		if err != nil {
			t.Fatal(err)
		}
		episodes[campaign] = started
	}
	// Simulate an agent crash: a fresh manager over the same root must
	// finalize every partial from every campaign, not just one.
	recovered, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if keys := recovered.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("recovered manager reports active episodes: %v", keys)
	}
	list, err := recovered.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("recovered episodes = %d, want 3", len(list))
	}
	for campaign, started := range episodes {
		mf, _, err := recovered.Inspect(started.ID, true)
		if err != nil {
			t.Fatalf("campaign %q episode not recovered: %v", campaign, err)
		}
		if mf.State != "interrupted" || mf.Interruption != "agent_restart" {
			t.Fatalf("campaign %q episode state = %s/%s", campaign, mf.State, mf.Interruption)
		}
		if mf.Trigger.CampaignName != campaign {
			t.Fatalf("campaign association lost: %q vs %q", mf.Trigger.CampaignName, campaign)
		}
		wantUpload := "local"
		if campaign != "" {
			wantUpload = "pending"
		}
		if mf.Upload.State != wantUpload {
			t.Fatalf("campaign %q upload state = %q, want %q", campaign, mf.Upload.State, wantUpload)
		}
	}
	// Recovered pending-upload episodes keep their eviction protection.
	var warnings []string
	recovered.SetWarnLogger(func(msg string) { warnings = append(warnings, msg) })
	recovered.SetQuota(1, 0)
	if _, err := recovered.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	protectedWarnings := 0
	for _, warning := range warnings {
		if strings.Contains(warning, "camp-a") || strings.Contains(warning, "camp-b") {
			protectedWarnings++
		}
	}
	if protectedWarnings != 2 {
		t.Fatalf("recovered pending episodes were evicted without warnings: %v", warnings)
	}
}

func TestManagerConcurrentLifecycleStress(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetWarnLogger(func(string) {})
	keys := []string{AdHocEpisodeKey, "stress-a", "stress-b", "stress-c"}
	var wg sync.WaitGroup
	for _, key := range keys {
		key := key
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				if _, err := m.Start(StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "stress", CampaignName: key}}); err != nil {
					continue
				}
				if i%3 == 0 {
					_, _ = m.Interrupt(key, "stress")
				} else {
					_, _ = m.Stop(key)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = m.RecordApplication("stress.app", ApplicationRecord{Version: 1, Type: "event", Name: "stress"})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			for _, key := range keys {
				_, _ = m.Stop(key)
			}
			_ = m.Status()
			_ = m.ActiveEpisodeKeys()
			_, _ = m.List()
		}
	}()
	wg.Wait()
	for _, key := range keys {
		_, _ = m.Stop(key)
	}
	if keys := m.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("episodes leaked after stress: %v", keys)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, episode := range list {
		if seen[episode.ID] {
			t.Fatalf("episode %s sealed twice", episode.ID)
		}
		seen[episode.ID] = true
		if episode.State != "complete" && episode.State != "interrupted" {
			t.Fatalf("episode %s in state %q after stress", episode.ID, episode.State)
		}
	}
	entries, err := os.ReadDir(m.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Fatalf("partial episode %s left behind", entry.Name())
		}
	}
}

// TestUploadStateVocabularyMatchesEviction pins the awaitingUpload state set
// against the states the agent actually writes, so a renamed workflow state
// cannot silently revert eviction to oldest-first.
func TestUploadStateVocabularyMatchesEviction(t *testing.T) {
	// States produced by this agent today: "local" (ad-hoc default) and
	// "pending" (campaign episodes). "uploaded" is written by the future
	// transfer worker. Anything unknown must stay protected (fail-safe).
	for state, awaiting := range map[string]bool{
		"":          false,
		"local":     false,
		"uploaded":  false,
		"pending":   true,
		"uploading": true,
		"failed":    true,
	} {
		if got := awaitingUpload(state); got != awaiting {
			t.Errorf("awaitingUpload(%q) = %v, want %v", state, got, awaiting)
		}
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(`{"upload":{"state":"pending"}}`), &manifest); err != nil {
		t.Fatal(err)
	}
	if !awaitingUpload(manifest.Upload.State) {
		t.Fatal("persisted pending state lost eviction protection")
	}
}

func TestCampaignConcurrentDeployAndReload(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	yaml := `version: 1
name: deploy-%d
sources:
  - telemetry: true
capture:
  buffer: 10ms
  after_trigger: 30ms
  triggers:
    - event: emergency_stop
upload:
  when: wifi
export:
  annotation: cvat
`
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := m.DeployCampaign([]byte(fmt.Sprintf(yaml, i))); err != nil {
					t.Errorf("deploy %d: %v", i, err)
					return
				}
				if _, err := m.Campaigns(); err != nil {
					t.Errorf("list during deploy: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	campaigns, err := m.Campaigns()
	if err != nil {
		t.Fatal(err)
	}
	if len(campaigns) != 8 {
		t.Fatalf("campaigns = %d, want 8", len(campaigns))
	}
	_ = time.Now()
}
