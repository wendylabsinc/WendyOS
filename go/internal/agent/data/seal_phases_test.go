package data

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFailedSealAbandonsTheEpisodeInsteadOfParkingIt pins what happens when a
// seal fails on an episode that has already given up its campaign key.
//
// beginSeal moves a draining episode from m.active to m.sealing. Every error
// return in the finalize path used to precede the removal from m.sealing, so a
// seal that failed (no space left for the manifest, a symlink in the episode
// directory, a rename that could not complete) left the episode there
// permanently. Nothing could reach it afterwards: Stop and Interrupt look only
// in m.active, so it could never be retried, while RecordApplication walks the
// draining episodes too and kept appending records to it, answering "recorded"
// to the application for data that would never be sealed or uploaded.
//
// The failure is injected with a symlink, which sealFiles refuses by design.
func TestFailedSealAbandonsTheEpisodeInsteadOfParkingIt(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{
		Sources:       []string{"applications"},
		DrainDuration: 20 * time.Millisecond,
		Trigger:       EpisodeTrigger{Reason: "event:seal-failure", CampaignName: "camp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, started.ID+".partial")
	if err = os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}

	if _, err = m.Stop("camp"); err == nil {
		t.Fatal("Stop reported success although the episode could not be sealed")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Stop failed with %v, want the symlink refusal from sealFiles", err)
	}

	m.mu.Lock()
	stillActive, stillSealing := len(m.active), len(m.sealing)
	m.mu.Unlock()
	if stillActive != 0 || stillSealing != 0 {
		t.Fatalf("after a failed seal the manager still holds %d active and %d sealing episode(s), want none", stillActive, stillSealing)
	}

	// The application must be told the truth: nothing is open, so the record
	// is buffered for the next episode rather than acknowledged into one that
	// will never be sealed.
	state, err := m.RecordApplication("com.example.scorer", ApplicationRecord{Version: 1, Type: "event", Name: "after-failed-seal"})
	if err != nil {
		t.Fatal(err)
	}
	if state != "buffered" {
		t.Fatalf("record after a failed seal: state=%q, want %q", state, "buffered")
	}
	if containsName(recordNames(t, readEvents(t, dir)), "after-failed-seal") {
		t.Fatal("a record reached an episode the manager had already failed to seal")
	}

	// The directory keeps its ".partial" suffix, which is what recoverPartials
	// picks up on the next start: the episode has an owner again.
	if _, err = os.Stat(dir); err != nil {
		t.Fatalf("failed seal did not leave the episode for recovery: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, started.ID)); err == nil {
		t.Fatal("failed seal published the episode anyway")
	}

	// The campaign key is free, so the campaign records again immediately
	// rather than being wedged behind an episode nobody can finalize.
	if _, err = m.Start(StartOptions{
		Sources: []string{"applications"},
		Trigger: EpisodeTrigger{Reason: "event:next", CampaignName: "camp"},
	}); err != nil {
		t.Fatalf("the campaign could not start its next episode: %v", err)
	}
}

// TestSealDoesNotBlockOtherEpisodesOrDelayReceipts pins that the expensive part
// of a seal runs with m.mu released.
//
// The playable remux rewrites every camera byte and the checksum pass reads
// every byte again, which is seconds of input and output on a real episode.
// Run under the lock they blocked every other manager call, and the damage was
// not only latency: RecordApplication took its agent receipt after acquiring
// the lock, so a record that arrived during a seal was stamped with the time
// the manager got round to it, minutes off on a large episode, and the
// pre-roll window and episode offsets derived from that stamp inherited the
// error.
func TestSealDoesNotBlockOtherEpisodesOrDelayReceipts(t *testing.T) {
	const mux = 750 * time.Millisecond

	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The episode being sealed, and a second, unrelated episode that must keep
	// serving throughout.
	if _, err = m.Start(StartOptions{
		Sources: []string{"applications"},
		Trigger: EpisodeTrigger{Reason: "event:slow", CampaignName: "slow"},
	}); err != nil {
		t.Fatal(err)
	}
	other, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}

	muxing := make(chan struct{})
	defer func(original func(string) []string) { sealMux = original }(sealMux)
	sealMux = func(string) []string {
		close(muxing)
		time.Sleep(mux)
		return nil
	}

	sealed := make(chan error, 1)
	go func() {
		_, stopErr := m.Stop("slow")
		sealed <- stopErr
	}()
	<-muxing

	// A record for the other episode, written while the seal is muxing.
	before, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	state, err := m.RecordApplication("com.example.scorer", ApplicationRecord{Version: 1, Type: "event", Name: "during-seal"})
	elapsed := time.Since(begin)
	if err != nil {
		t.Fatal(err)
	}
	if state != "recorded" {
		t.Fatalf("record written during another episode's seal: state=%q, want %q", state, "recorded")
	}
	if elapsed > mux/3 {
		t.Fatalf("RecordApplication took %s during a %s seal: it is waiting for the seal to finish", elapsed, mux)
	}

	// Also check the other manager calls a device makes constantly.
	begin = time.Now()
	m.ActiveEpisodeKeys()
	m.Status()
	if _, err = m.Start(StartOptions{
		Sources: []string{"applications"},
		Trigger: EpisodeTrigger{Reason: "event:concurrent", CampaignName: "concurrent"},
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed = time.Since(begin); elapsed > mux/3 {
		t.Fatalf("status and start calls took %s during a %s seal", elapsed, mux)
	}

	if err = <-sealed; err != nil {
		t.Fatal(err)
	}

	// The receipt is the agent's statement of when the record arrived, so it
	// must sit at the moment of the call and not at the end of the seal.
	dir, err := m.episodeDir(other.ID)
	if err != nil {
		// The other episode is still open, so its directory is the partial one.
		dir = filepath.Join(m.root, other.ID+".partial")
	}
	var receipt int64
	for _, stored := range decodeRecords(t, readEvents(t, dir)) {
		if stored.Name == "during-seal" {
			receipt = stored.AgentReceiptBootNanos
		}
	}
	if receipt == 0 {
		t.Fatal("the record written during the seal did not reach the other episode")
	}
	if late := time.Duration(receipt - before); late > mux/3 {
		t.Fatalf("agent receipt is %s after the record was written, which is the seal duration leaking into the timestamp", late)
	}
}
