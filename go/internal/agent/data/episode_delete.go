package data

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// trashDirName is where DeleteEpisode puts an episode it has taken out of the
// store and is still removing. A hidden directory of the store itself, so the
// move into it is a rename on one filesystem, and so every reader of the store
// already passes over it: List, the upload backlog and the failed-upload
// requeue skip a directory without a manifest, and countEpisodes skips hidden
// entries. scanEpisodeStore counts its bytes, because they are still on disk,
// but never offers it for eviction.
const trashDirName = ".trash"

var (
	// ErrEpisodeInProgress is DeleteEpisode refusing an episode that has not
	// been finalized: it is still recording, draining or sealing, and its
	// directory belongs to the capture that is writing it.
	ErrEpisodeInProgress = errors.New("episode is still in progress")
	// ErrEpisodeBeingRead is DeleteEpisode refusing an episode a Download or
	// the upload worker is reading right now (see BeginDownload).
	ErrEpisodeBeingRead = errors.New("episode is being read")
)

// DeleteEpisode permanently removes one finalized episode and every file it
// holds, and returns its manifest as it was just before. It cannot be undone,
// and an episode that had not been uploaded is lost for good; confirming that
// with the operator is the caller's job.
//
// Only a finalized episode can be deleted. One that is recording, draining or
// sealing is refused with ErrEpisodeInProgress, because its directory is still
// being written; one that a download or upload is reading is refused with
// ErrEpisodeBeingRead, for the reason eviction passes it over. An ID with no
// finalized episode behind it is os.ErrNotExist.
//
// The episode leaves the store in one rename under m.mu: into trashDirName,
// then its files are removed with the lock released, so a large episode does
// not hold up recording while it is unlinked. Every reader of the store either
// sees the whole episode or none of it. A removal that a crash or power cut
// interrupts is finished by emptyTrash when the agent next starts.
func (m *Manager) DeleteEpisode(id string) (Manifest, error) {
	// episodeDir already refuses an empty ID, "." and "..", and anything that
	// is not a plain name. A deletion must refuse more than a read does: a name
	// beginning with a dot is the store's own bookkeeping (".recordings" holds
	// every app's durable streams, ".trash" is this function's), and a
	// ".partial" or ".unrecoverable" suffix names a directory that is not a
	// finalized episode. Generated IDs never look like any of these.
	if strings.HasPrefix(id, ".") || strings.HasSuffix(id, ".partial") || strings.HasSuffix(id, unrecoverableSuffix) {
		return Manifest{}, ErrInvalidEpisodeID
	}
	m.mu.Lock()
	if state, ok := m.inProgressStateLocked(id); ok {
		m.mu.Unlock()
		return Manifest{}, fmt.Errorf("%w: episode %s is %s; stop it with `wendy data stop` or wait for it to be sealed, then delete it", ErrEpisodeInProgress, id, state)
	}
	if m.downloads[id] > 0 {
		m.mu.Unlock()
		return Manifest{}, fmt.Errorf("%w: episode %s is being downloaded or uploaded right now; delete it when that has finished", ErrEpisodeBeingRead, id)
	}
	dir, err := m.episodeDir(id)
	if err != nil {
		m.mu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, fmt.Errorf("no finalized episode %s on this device: %w", id, os.ErrNotExist)
		}
		return Manifest{}, err
	}
	mf, err := readManifest(dir)
	if err != nil || mf.ID != id {
		// Without a manifest that names it, the directory is not an episode
		// List would ever have shown, and deleting it would be guessing.
		m.mu.Unlock()
		return Manifest{}, fmt.Errorf("no finalized episode %s on this device (its manifest is missing or names another episode): %w", id, os.ErrNotExist)
	}
	trash := filepath.Join(m.root, trashDirName)
	if err := os.MkdirAll(trash, 0o750); err != nil {
		m.mu.Unlock()
		return Manifest{}, fmt.Errorf("deleting episode %s: %w", id, err)
	}
	// The suffix keeps a second deletion of the same ID apart from a first one
	// that has not finished being removed.
	doomed := filepath.Join(trash, fmt.Sprintf("%s.%d", id, time.Now().UnixNano()))
	if err := os.Rename(dir, doomed); err != nil {
		m.mu.Unlock()
		return Manifest{}, fmt.Errorf("deleting episode %s: %w", id, err)
	}
	m.mu.Unlock()
	if err := os.RemoveAll(doomed); err != nil {
		// The episode is already out of the store; only its bytes linger, and
		// the next agent start removes them.
		m.warnf("episode %s was deleted, but removing its files failed (%v); they are removed when the agent next starts", id, err)
	}
	return mf, nil
}

// inProgressStateLocked reports the state of an episode that is still
// capturing, draining or sealing, which are the three places an episode lives
// before its directory is published under its ID. Callers hold m.mu.
func (m *Manager) inProgressStateLocked(id string) (string, bool) {
	for _, a := range m.active {
		if a.manifest.ID == id {
			return a.manifest.State, true
		}
	}
	for _, a := range m.sealing {
		if a.manifest.ID == id {
			return EpisodeStateDraining, true
		}
	}
	for _, v := range m.finalizing {
		if v.ID == id {
			return v.State, true
		}
	}
	return "", false
}

// emptyTrash finishes every deletion an earlier agent started and did not
// complete. It runs once, from NewManager, before anything can delete again.
func (m *Manager) emptyTrash() {
	trash := filepath.Join(m.root, trashDirName)
	entries, err := os.ReadDir(trash)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.warnf("reading deleted episodes in %s failed: %v", trash, err)
		}
		return
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(trash, e.Name())); err != nil {
			m.warnf("finishing the deletion of episode %s failed: %v", e.Name(), err)
		}
	}
}
