package data

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForInProgress polls until want reports true for the in-progress list, so
// the tests follow an episode through its states without sleeping for a guess.
func waitForInProgress(t *testing.T, m *Manager, what string, want func([]InProgressEpisode) bool) []InProgressEpisode {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := m.InProgress()
		if want(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; in progress: %+v", what, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestInProgressFollowsAnEpisodeUntilItIsPublished is the bug report: List
// reads only published episodes, so `wendy data episodes` showed nothing from
// the moment a recording started until its seal had finished. InProgress must
// cover that whole span, recording, draining and sealing, and hand over to
// List without a gap or an overlap.
func TestInProgressFollowsAnEpisodeUntilItIsPublished(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{
		Name:              "door",
		Sources:           []string{"applications", "telemetry"},
		DrainDuration:     150 * time.Millisecond,
		Trigger:           EpisodeTrigger{Reason: "event:person_detected", CampaignName: "door"},
		UnresolvedSources: []Source{{ID: "ros2:/scan", Kind: "ros2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	adHoc, err := m.Start(StartOptions{Name: "manual-test", Sources: []string{"telemetry"}})
	if err != nil {
		t.Fatal(err)
	}

	open := m.InProgress()
	if len(open) != 2 || open[0].ID != adHoc.ID || open[1].ID != started.ID {
		t.Fatalf("want both episodes, newest first: %+v", open)
	}
	manual, campaign := open[0], open[1]
	if manual.State != "recording" || manual.TriggerReason != "manual" || manual.CampaignName != "" || manual.Name != "manual-test" {
		t.Fatalf("ad-hoc episode: %+v", manual)
	}
	if campaign.TriggerReason != "event:person_detected" || campaign.CampaignName != "door" || campaign.BootID == "" || campaign.StartedUnixNanos != started.StartedUnixNanos {
		t.Fatalf("campaign episode: %+v", campaign)
	}
	// The ROS 2 topic that did not resolve is in the manifest but is not being
	// recorded, so it is not reported as a source.
	if strings.Join(campaign.Sources, ",") != "applications,telemetry" {
		t.Fatalf("campaign sources = %v", campaign.Sources)
	}
	if campaign.SizeBytes <= 0 {
		t.Fatalf("an open episode already holds its manifest on disk, got size %d", campaign.SizeBytes)
	}
	first := campaign.Duration
	time.Sleep(20 * time.Millisecond)
	if again := waitForInProgress(t, m, "the campaign episode", func(got []InProgressEpisode) bool { return len(got) == 2 }); again[1].Duration <= first {
		t.Fatalf("a recording episode's duration did not grow: %s then %s", first, again[1].Duration)
	}

	// Hold the seal open so the sealing state can be observed.
	muxing, release := make(chan struct{}), make(chan struct{})
	originalMux := sealMux
	defer func() { sealMux = originalMux }()
	sealMux = func(string) []string {
		close(muxing)
		<-release
		return nil
	}
	stopped := make(chan error, 1)
	go func() {
		_, stopErr := m.Stop("door")
		stopped <- stopErr
	}()

	draining := waitForInProgress(t, m, "the draining state", func(got []InProgressEpisode) bool {
		return len(got) == 2 && got[1].State == EpisodeStateDraining
	})[1]
	if draining.Duration <= 0 {
		t.Fatalf("a draining episode must report how long it captured: %+v", draining)
	}

	<-muxing
	sealing := waitForInProgress(t, m, "the sealing state", func(got []InProgressEpisode) bool {
		return len(got) == 2 && got[1].State == EpisodeStateSealing
	})[1]
	if sealing.Duration != draining.Duration {
		t.Fatalf("capture had stopped, so the duration is frozen: draining %s, sealing %s", draining.Duration, sealing.Duration)
	}
	if sealing.TriggerReason != "event:person_detected" || sealing.Name != "door" || len(sealing.Sources) != 2 {
		t.Fatalf("sealing episode lost its description: %+v", sealing)
	}
	// Status, which older clients read, still does not report a sealing
	// episode beside a recording one.
	if status := m.Status(); status == nil || status.ID != adHoc.ID {
		t.Fatalf("Status = %+v, want the ad-hoc episode", status)
	}
	if status, both := m.StatusAndInProgress(); status == nil || status.ID != adHoc.ID || len(both) != 2 || both[1].State != EpisodeStateSealing {
		t.Fatalf("StatusAndInProgress = %+v, %+v", status, both)
	}

	close(release)
	if err = <-stopped; err != nil {
		t.Fatal(err)
	}
	sealMux = originalMux
	open = m.InProgress()
	if len(open) != 1 || open[0].ID != adHoc.ID {
		t.Fatalf("the published episode is still in progress: %+v", open)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != started.ID || list[0].State != "complete" {
		t.Fatalf("List = %+v, want the sealed campaign episode", list)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	if open = m.InProgress(); len(open) != 0 {
		t.Fatalf("nothing is open any more, got %+v", open)
	}
}

// TestInProgressOrdersByBootTime: every in-progress episode is from this boot,
// so "newest first" is decided on CLOCK_BOOTTIME. A wall clock that NTP stepped
// back between two triggers must not list the newer episode below the older.
func TestInProgressOrdersByBootTime(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	older, err := m.Start(StartOptions{Name: "door", Sources: []string{"telemetry"}, Trigger: EpisodeTrigger{CampaignName: "door"}})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := m.Start(StartOptions{Name: "manual-test", Sources: []string{"telemetry"}})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.active[AdHocEpisodeKey].manifest.StartedUnixNanos = older.StartedUnixNanos - time.Hour.Nanoseconds()
	m.mu.Unlock()

	if open := m.InProgress(); len(open) != 2 || open[0].ID != newer.ID || open[1].ID != older.ID {
		t.Fatalf("want the later-started episode first despite its earlier wall clock: %+v", open)
	}
	for _, key := range []string{AdHocEpisodeKey, "door"} {
		if _, err = m.Stop(key); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInProgressSizeFollowsThePublishRename: sizes are measured without m.mu,
// so a seal can publish the episode between the snapshot and the walk. Its
// bytes are then under the published name, and a sealing episode must not
// report 0 bytes for them.
func TestInProgressSizeFollowsThePublishRename(t *testing.T) {
	root := t.TempDir()
	partial := filepath.Join(root, "ep.partial")
	if err := os.Mkdir(partial, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "telemetry.wdr"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := inProgressBytes(partial); got != 100 {
		t.Fatalf("size before the rename = %d, want 100", got)
	}
	if err := os.Rename(partial, filepath.Join(root, "ep")); err != nil {
		t.Fatal(err)
	}
	if got := inProgressBytes(partial); got != 100 {
		t.Fatalf("size after the publish rename = %d, want 100", got)
	}
}

// TestInProgressForgetsAnEpisodeWhoseSealFailed: a failed seal leaves the
// directory for recovery on the next start, and the episode is no longer in
// progress. Reporting it as "sealing" forever would be the same mistake as the
// parked episode TestFailedSealAbandonsTheEpisodeInsteadOfParkingIt pins.
func TestInProgressForgetsAnEpisodeWhoseSealFailed(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(root, started.ID+".partial", "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err == nil {
		t.Fatal("Stop reported success although the episode could not be sealed")
	}
	if open := m.InProgress(); len(open) != 0 {
		t.Fatalf("an abandoned seal is still reported in progress: %+v", open)
	}
}
