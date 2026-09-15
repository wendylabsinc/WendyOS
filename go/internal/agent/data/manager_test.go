package data

import (
	"context"

	"bytes"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEpisodeLifecycleUsesBoottimeAndSealsFiles(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Name: "trial", Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if started.CanonicalClock != "CLOCK_BOOTTIME" || started.BootID == "" {
		t.Fatalf("bad clock identity: %+v", started)
	}
	if len(started.UTCObservations) != 1 || started.UTCObservations[0].UncertaintyNanos < 0 {
		t.Fatalf("bad UTC interval: %+v", started.UTCObservations)
	}
	stopped, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "complete" || stopped.StoppedEpisodeNS < 0 {
		t.Fatalf("bad stopped manifest: %+v", stopped)
	}
	if stopped.Device.ID == "" || stopped.Trigger.Reason != "manual" || stopped.Upload.State != "local" || stopped.Labeling.State != "unlabeled" {
		t.Fatalf("missing episode identity/workflow metadata: %+v", stopped)
	}
	if len(stopped.Files) != 1 || stopped.Files[0].Format != "jsonl" || stopped.Files[0].MediaType != "application/x-ndjson" || stopped.Files[0].SourceID != "applications" {
		t.Fatalf("payload format metadata missing: %+v", stopped.Files)
	}
	_, failures, err := m.Inspect(stopped.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("verification failures: %v", failures)
	}
}

func TestApplicationPreRollHasNegativeCanonicalOffsets(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.RecordApplication("com.example.app", ApplicationRecord{Version: 1, Type: "event", Name: "ready", ClientBootNanos: now, ClientBootID: bootID()})
	if err != nil {
		t.Fatal(err)
	}
	if state != "buffered" {
		t.Fatalf("state=%s", state)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	dir, err := m.episodeDir(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var stored storedApplicationRecord
	if err = json.Unmarshal(bytes.TrimSpace(b), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.EpisodeNanos >= 0 {
		t.Fatalf("pre-roll offset=%d, want negative", stored.EpisodeNanos)
	}
	manifest, _, err := m.Inspect(started.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Sources) != 1 || manifest.Sources[0].Count != 1 || manifest.Sources[0].ActualOffset >= 0 {
		t.Fatalf("pre-roll stats not represented in manifest: %+v", manifest.Sources)
	}
}

func TestRecoveryFinalizesPartialAsInterrupted(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, started.ID+".partial", "events.jsonl")
	if err = os.WriteFile(events, []byte("{\"ok\":true}\n{broken"), 0o640); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	mf, failures, err := m2.Inspect(started.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if mf.State != "interrupted" || mf.Interruption != "agent_restart" {
		t.Fatalf("bad recovery: %+v", mf)
	}
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	b, err := os.ReadFile(filepath.Join(root, started.ID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{\"ok\":true}\n" {
		t.Fatalf("tail not truncated: %q", b)
	}
}

func TestInspectDetectsChecksumCorruption(t *testing.T) {
	m, e := NewManager(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	start, e := m.Start(StartOptions{Sources: []string{"applications"}})
	if e != nil {
		t.Fatal(e)
	}
	done, e := m.Stop(AdHocEpisodeKey)
	if e != nil {
		t.Fatal(e)
	}
	if start.ID != done.ID {
		t.Fatal("id changed")
	}
	dir, _ := m.episodeDir(done.ID)
	if e = os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("tampered"), 0o640); e != nil {
		t.Fatal(e)
	}
	_, fail, e := m.Inspect(done.ID, true)
	if e != nil {
		t.Fatal(e)
	}
	if len(fail) == 0 {
		t.Fatal("expected corruption failure")
	}
}

func setUploadState(t *testing.T, m *Manager, id, uploadState, campaignName string) {
	t.Helper()
	dir, err := m.episodeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	mf.Upload.State = uploadState
	mf.Trigger.CampaignName = campaignName
	if err := writeManifest(dir, mf); err != nil {
		t.Fatal(err)
	}
}

func episodeSize(t *testing.T, m *Manager, id string) int64 {
	t.Helper()
	dir, err := m.episodeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	err = filepath.WalkDir(dir, func(_ string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return size
}

func recordEpisode(t *testing.T, m *Manager, opts StartOptions) Manifest {
	t.Helper()
	started, err := m.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := m.Stop(opts.Trigger.CampaignName)
	if err != nil {
		t.Fatal(err)
	}
	if started.ID != stopped.ID {
		t.Fatal("id changed across stop")
	}
	return stopped
}

func TestEnforceQuotaEvictsUploadedBeforePendingUpload(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	m.SetWarnLogger(func(msg string) { warnings = append(warnings, msg) })
	pending := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:test", CampaignName: "forklift"}})
	time.Sleep(2 * time.Millisecond)
	uploaded := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	setUploadState(t, m, pending.ID, "pending", "forklift")
	setUploadState(t, m, uploaded.ID, "uploaded", "")
	// The quota can hold the pending episode alone. The pending episode is the
	// OLDER of the two, so the previous strictly oldest-first order would have
	// evicted it; upload-state ordering must sacrifice the newer uploaded
	// episode instead.
	m.SetQuota(episodeSize(t, m, pending.ID)+1, 0)
	final := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	if _, err = m.episodeDir(uploaded.ID); err == nil {
		t.Fatal("uploaded episode was not evicted")
	}
	if _, err = m.episodeDir(pending.ID); err != nil {
		t.Fatal("pending episode did not survive eviction")
	}
	if _, err = m.episodeDir(final.ID); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	// Force the pending episode out as well: the quota can no longer hold it,
	// and the eviction must warn, naming the episode and campaign.
	m.SetQuota(1, 0)
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err == nil {
		if _, stopErr := m.Stop(AdHocEpisodeKey); stopErr != nil {
			t.Fatal(stopErr)
		}
	}
	if _, err = m.episodeDir(pending.ID); err == nil {
		t.Fatal("pending episode survived a quota that cannot hold it")
	}
	warned := false
	for _, warning := range warnings {
		warned = warned || strings.Contains(warning, pending.ID) && strings.Contains(warning, "forklift")
	}
	if !warned {
		t.Fatalf("missing eviction warning naming episode and campaign: %v", warnings)
	}
}

func TestPerCampaignEpisodeConcurrency(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Start(StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:a", CampaignName: "campaign-a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:b", CampaignName: "campaign-b"}})
	if err != nil {
		t.Fatalf("second campaign could not capture concurrently: %v", err)
	}
	adHoc, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatalf("ad-hoc episode could not run beside campaigns: %v", err)
	}
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:a", CampaignName: "campaign-a"}}); err == nil || !strings.Contains(err.Error(), "campaign-a") {
		t.Fatalf("second episode for the same campaign was accepted: %v", err)
	}
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err == nil || err.Error() != "an episode is already active" {
		t.Fatalf("second ad-hoc episode was accepted: %v", err)
	}
	// A record lands in every concurrently active episode.
	if _, err = m.RecordApplication("test.app", ApplicationRecord{Version: 1, Type: "event", Name: "shared"}); err != nil {
		t.Fatal(err)
	}
	keys := m.ActiveEpisodeKeys()
	if len(keys) != 3 || keys[0] != AdHocEpisodeKey || keys[1] != "campaign-a" || keys[2] != "campaign-b" {
		t.Fatalf("active keys: %v", keys)
	}
	for key, id := range map[string]string{"campaign-a": first.ID, "campaign-b": second.ID, AdHocEpisodeKey: adHoc.ID} {
		session, ok := m.ActiveSession(key)
		if !ok || session.ID != id {
			t.Fatalf("session for %q = %+v, want %s", key, session, id)
		}
		manifest, err := m.Stop(key)
		if err != nil {
			t.Fatal(err)
		}
		counted := false
		for _, source := range manifest.Sources {
			if source.Source.ID == "applications" && source.Count == 1 {
				counted = true
			}
		}
		if !counted {
			t.Fatalf("episode %s did not record the shared application record: %+v", id, manifest.Sources)
		}
	}
	// Quota accounting spans the episodes of every campaign and the ad-hoc one.
	episodes, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3", len(episodes))
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	for _, p := range []string{"../x", "/tmp/x", "a/../../x", ""} {
		if _, e := safeJoin("/safe", p); e == nil {
			t.Errorf("accepted %q", p)
		}
	}
}

// TestStartDoesNotBlockOnRoughtimeConsensus pins the property that made the
// trigger moment fall out of episode video: a Roughtime server that never
// answers used to hold Start (and therefore every capture adapter) for the
// whole query timeout, so the camera began recording seconds after the
// trigger. The consensus is still recorded, just not on the start path.
func TestStartDoesNotBlockOnRoughtimeConsensus(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	queried := make(chan struct{}, 4)
	m.SetConsensusProvider(func(ctx context.Context) (timesync.Consensus, error) {
		queried <- struct{}{}
		select {
		case <-released:
		case <-ctx.Done():
			return timesync.Consensus{}, ctx.Err()
		}
		return timesync.Consensus{Confidence: "degraded", Quorum: 2, LowerOffsetNanos: 1, UpperOffsetNanos: 2}, nil
	})

	start := time.Now()
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Start blocked %s on an unresponsive consensus provider; it must not wait", elapsed)
	}

	// The query still happens, and its result still reaches the manifest.
	select {
	case <-queried:
	case <-time.After(5 * time.Second):
		t.Fatal("consensus was never queried in the background")
	}
	close(released)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var manifest Manifest
		b, readErr := os.ReadFile(filepath.Join(m.root, started.ID+".partial", "manifest.json"))
		if readErr == nil && json.Unmarshal(b, &manifest) == nil && len(manifest.Roughtime) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background consensus never landed in the manifest")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
}
