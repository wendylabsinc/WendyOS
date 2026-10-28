package data

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataRootMigrationPreservesStoreAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	oldRoot := filepath.Join(dir, "os", "data", "episodes")
	newRoot := filepath.Join(dir, "persistent", "data", "episodes")
	files := map[string]string{
		"episodes/episode/manifest.json":      "episode manifest",
		"episodes/.recordings/stream/records": "durable records",
		"campaigns/person.json":               "campaign configuration",
		"inference/model/config.json":         "model configuration",
	}
	for name, contents := range files {
		path := filepath.Join(filepath.Dir(oldRoot), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := migrateDataRoot(oldRoot, newRoot)
	if err != nil || got != newRoot {
		t.Fatalf("migration = %q, %v", got, err)
	}
	for name, want := range files {
		for _, root := range []string{oldRoot, newRoot} {
			path := filepath.Join(filepath.Dir(root), name)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("%s: %q, %v", path, data, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("private permissions lost: %s", info.Mode())
			}
		}
	}
	updated := filepath.Join(filepath.Dir(newRoot), "campaigns/person.json")
	if err := os.WriteFile(updated, []byte("updated campaign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateDataRoot(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(updated)
	if err != nil || string(data) != "updated campaign" {
		t.Fatalf("restart overwrote new state: %q, %v", data, err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(newRoot)), ".data-migration-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("staging directories left behind: %v, %v", stages, err)
	}
}

func TestDataRootMigrationRejectsNonDirectoryDestination(t *testing.T) {
	dir := t.TempDir()
	newDir := filepath.Join(dir, "data")
	if err := os.WriteFile(newDir, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateDataRoot(filepath.Join(dir, "old", "episodes"), filepath.Join(newDir, "episodes")); err == nil {
		t.Fatal("accepted a file as the data directory")
	}
	data, err := os.ReadFile(newDir)
	if err != nil || string(data) != "keep" {
		t.Fatalf("destination changed: %q, %v", data, err)
	}
}

func TestExplicitDataRootIsUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-episodes")
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if manager.root != root {
		t.Fatalf("explicit root changed to %s", manager.root)
	}
}

func TestDataRootMigrationDoesNotHideConflictingStores(t *testing.T) {
	dir := t.TempDir()
	oldRoot, newRoot := filepath.Join(dir, "old", "episodes"), filepath.Join(dir, "new", "episodes")
	for _, root := range []string{oldRoot, newRoot} {
		if err := os.MkdirAll(root, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := migrateDataRoot(oldRoot, newRoot); err == nil {
		t.Fatal("silently selected one of two existing stores")
	}
}

func TestStorageErrorReportsFilesystemAndFreeSpace(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetQuota(DefaultMaxQuotaBytes, 1<<62)
	_, err = m.Start(StartOptions{Name: "quota-check", Sources: []string{"applications"}})
	if err == nil || !strings.Contains(err.Error(), m.root) || !strings.Contains(err.Error(), "GiB free") {
		t.Fatalf("not an actionable storage error: %v", err)
	}
}
