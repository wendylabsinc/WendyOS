package data

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEpisodeMediaPreservesManifestValues(t *testing.T) {
	files := []File{{Path: "clip.mp4", MediaType: "video/mp4"}, {Path: "telemetry.jsonl", MediaType: "application/x-ndjson"}, {Path: "events.jsonl", MediaType: "application/x-ndjson"}, {Path: "legacy.wav"}}
	want := []EpisodeFileMedia{{Path: "clip.mp4", MediaType: "video/mp4"}, {Path: "telemetry.jsonl", MediaType: "application/x-ndjson"}, {Path: "events.jsonl", MediaType: "application/x-ndjson"}, {Path: "legacy.wav"}}
	if got := episodeMediaFiles(files); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := episodeMediaFiles(nil); got == nil || len(got) != 0 {
		t.Fatalf("known empty must be [], got %v", got)
	}
}

func TestListSummarizesExistingManifestsWithoutRewriting(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.root, "existing")
	if err = os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	mf := Manifest{ID: "existing", State: "complete", Files: []File{{Path: "clip.mp4", Size: 12}, {Path: "telemetry.jsonl", Size: 5}}}
	if err = writeManifest(dir, mf); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SizeBytes != 17 || !reflect.DeepEqual(rows[0].MediaFiles, []EpisodeFileMedia{{Path: "clip.mp4"}, {Path: "telemetry.jsonl"}}) {
		t.Fatalf("unexpected list: %+v", rows)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if string(before) != string(after) {
		t.Fatal("listing rewrote existing manifest")
	}
}
