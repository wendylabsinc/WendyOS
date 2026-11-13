package data

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteEpisodeRemovesItFromEveryView(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	doomed := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:test", CampaignName: "forklift"}})
	kept := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	setUploadState(t, m, doomed.ID, "pending", "forklift")

	deleted, err := m.DeleteEpisode(doomed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.ID != doomed.ID || deleted.Upload.State != "pending" || deleted.Trigger.CampaignName != "forklift" {
		t.Fatalf("returned manifest is not the deleted episode as it was: %+v", deleted)
	}
	if _, err := m.episodeDir(doomed.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("episode directory still resolves after delete: %v", err)
	}
	if _, _, err := m.Inspect(doomed.ID, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Inspect of a deleted episode = %v, want os.ErrNotExist", err)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != kept.ID {
		t.Fatalf("List after delete = %+v, want only %s", list, kept.ID)
	}
	backlog, err := m.EpisodesAwaitingUpload()
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog) != 0 {
		t.Fatalf("a deleted pending episode is still in the upload backlog: %+v", backlog)
	}
	// The files are gone, not just moved aside.
	entries, err := os.ReadDir(filepath.Join(m.root, trashDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("deleted episode left files in %s: %v", trashDirName, entries)
	}
	if _, _, err := m.Inspect(kept.ID, true); err != nil {
		t.Fatalf("the other episode was disturbed: %v", err)
	}

	// Deleting it again names it as absent.
	if _, err := m.DeleteEpisode(doomed.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second delete = %v, want os.ErrNotExist", err)
	}
}

func TestDeleteEpisodeRefusesARecordingInProgress(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.DeleteEpisode(started.ID)
	if !errors.Is(err, ErrEpisodeInProgress) {
		t.Fatalf("delete while recording = %v, want ErrEpisodeInProgress", err)
	}
	if !strings.Contains(err.Error(), started.ID) || !strings.Contains(err.Error(), "wendy data stop") {
		t.Fatalf("refusal does not name the episode and the way out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.root, started.ID+".partial")); err != nil {
		t.Fatalf("the recording's directory was touched: %v", err)
	}
	if _, err := m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeleteEpisode(started.ID); err != nil {
		t.Fatalf("delete after the episode was sealed: %v", err)
	}
}

func TestDeleteEpisodeRefusesAnEpisodeStillDrainingOrSealing(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A finalized directory under the same ID would be published by the very
	// seal that is still running; the in-progress state wins.
	ep := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	for _, state := range []string{EpisodeStateDraining, EpisodeStateSealing} {
		m.mu.Lock()
		if state == EpisodeStateDraining {
			m.sealing = []*activeEpisode{{manifest: Manifest{ID: ep.ID}}}
		} else {
			m.sealing = nil
			m.finalizing = []InProgressEpisode{{ID: ep.ID, State: EpisodeStateSealing}}
		}
		m.mu.Unlock()
		_, err := m.DeleteEpisode(ep.ID)
		if !errors.Is(err, ErrEpisodeInProgress) || !strings.Contains(err.Error(), state) {
			t.Fatalf("delete while %s = %v, want ErrEpisodeInProgress naming %q", state, err, state)
		}
	}
	m.mu.Lock()
	m.finalizing = nil
	m.mu.Unlock()
	if _, err := m.DeleteEpisode(ep.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteEpisodeRefusesAnEpisodeBeingRead(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	m.BeginDownload(ep.ID)
	if _, err := m.DeleteEpisode(ep.ID); !errors.Is(err, ErrEpisodeBeingRead) {
		t.Fatalf("delete during a download = %v, want ErrEpisodeBeingRead", err)
	}
	if _, err := m.episodeDir(ep.ID); err != nil {
		t.Fatalf("episode being read was deleted: %v", err)
	}
	m.EndDownload(ep.ID)
	if _, err := m.DeleteEpisode(ep.ID); err != nil {
		t.Fatalf("delete after the download finished: %v", err)
	}
}

func TestDeleteEpisodeRefusesWhatIsNotAFinalizedEpisode(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The store's own bookkeeping and the directories of unfinished or damaged
	// episodes must never be reachable by an episode ID.
	for _, name := range []string{".recordings", trashDirName, "20261106T000000Z-abc.partial", "20261106T000000Z-abc" + unrecoverableSuffix} {
		if err := os.MkdirAll(filepath.Join(m.root, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"", ".", "..", ".recordings", trashDirName, "20261106T000000Z-abc.partial", "20261106T000000Z-abc" + unrecoverableSuffix, "a/b", "../x"} {
		if _, err := m.DeleteEpisode(id); !errors.Is(err, ErrInvalidEpisodeID) {
			t.Errorf("DeleteEpisode(%q) = %v, want ErrInvalidEpisodeID", id, err)
		}
	}
	for _, name := range []string{".recordings", "20261106T000000Z-abc.partial", "20261106T000000Z-abc" + unrecoverableSuffix} {
		if _, err := os.Stat(filepath.Join(m.root, name)); err != nil {
			t.Errorf("%s was touched: %v", name, err)
		}
	}

	// A directory that looks like an episode but has no manifest naming it is
	// not one List would ever have shown, so it is not found, and left alone.
	stray := filepath.Join(m.root, "20261106T000000Z-stray")
	if err := os.MkdirAll(stray, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeleteEpisode("20261106T000000Z-stray"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete of a directory without a manifest = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("directory without a manifest was removed: %v", err)
	}
	if _, err := m.DeleteEpisode("20261106T000000Z-never"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete of an unknown ID = %v, want os.ErrNotExist", err)
	}
}

func TestNewManagerFinishesAnInterruptedDelete(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	ep := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	// What a crash between the rename and the removal leaves behind.
	dir, err := m.episodeDir(ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, trashDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(root, trashDirName, ep.ID+".1")); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	m, err = NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetWarnLogger(func(msg string) { warnings = append(warnings, msg) })
	entries, err := os.ReadDir(filepath.Join(root, trashDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("interrupted delete was not finished at start: %v", entries)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("a deleted episode came back: %+v", list)
	}
	// The trash directory itself is not an episode the quota can't read.
	recordEpisode(t, m, StartOptions{Sources: []string{"applications"}})
	for _, w := range warnings {
		if strings.Contains(w, trashDirName) {
			t.Fatalf("the quota scan treated %s as an episode: %v", trashDirName, warnings)
		}
	}
}

func TestStoreScanNeverEvictsTheTrash(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, trashDirName, "20261106T000000Z-abc.1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, trashDirName, "20261106T000000Z-abc.1", "payload"), []byte("12345"), 0o640); err != nil {
		t.Fatal(err)
	}
	scan, err := scanEpisodeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if scan.used != 5 {
		t.Fatalf("used = %d, want the trash's 5 bytes counted", scan.used)
	}
	if len(scan.candidates) != 0 || len(scan.readFailures) != 0 {
		t.Fatalf("the trash was offered for eviction or reported unreadable: %+v", scan)
	}
}
