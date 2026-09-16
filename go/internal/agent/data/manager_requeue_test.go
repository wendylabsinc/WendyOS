package data

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func seedEpisode(t *testing.T, root, id, state string, attempts int) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	mf := Manifest{ID: id}
	mf.Upload.State = state
	mf.Upload.Attempts = attempts
	mf.Upload.LastError = "gave up"
	b, err := json.Marshal(mf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// TestRequeueFailedUploadsRevivesTheBacklog covers the one-way door: before
// this, "failed" was terminal because EpisodesAwaitingUpload only ever returned
// pending and uploading. Fixing whatever broke uploads did nothing for anything
// already given up on, so a device that spent a week failing kept that week's
// data on disk permanently unshipped.
func TestRequeueFailedUploadsRevivesTheBacklog(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	seedEpisode(t, root, "ep-failed", "failed", 5)
	seedEpisode(t, root, "ep-uploaded", "uploaded", 0)
	seedEpisode(t, root, "ep-pending", "pending", 2)

	before, err := m.EpisodesAwaitingUpload()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("before requeue: %d episodes awaiting upload, want 1 (the failed one must be invisible)", len(before))
	}

	moved, err := m.RequeueFailedUploads()
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("requeued %d, want 1", moved)
	}

	after, err := m.EpisodesAwaitingUpload()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("after requeue: %d awaiting upload, want 2", len(after))
	}
	for _, mf := range after {
		if mf.ID == "ep-failed" {
			if mf.Upload.State != "pending" {
				t.Errorf("state = %q, want pending", mf.Upload.State)
			}
			// A revived episode that kept its exhausted counter would fail
			// again on the first attempt and undo the requeue.
			if mf.Upload.Attempts != 0 {
				t.Errorf("attempts = %d, want 0", mf.Upload.Attempts)
			}
		}
	}
}

// An uploaded episode must not be dragged back into the queue.
func TestRequeueFailedUploadsLeavesOtherStatesAlone(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	seedEpisode(t, root, "ep-uploaded", "uploaded", 0)
	seedEpisode(t, root, "ep-local", "local", 0)

	moved, err := m.RequeueFailedUploads()
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Fatalf("requeued %d, want 0", moved)
	}
}
