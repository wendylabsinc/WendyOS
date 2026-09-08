package data

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultRootUsesDataVolume(t *testing.T) {
	volume := t.TempDir()
	root, err := defaultRoot(volume)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(volume, "wendy-agent", "data", "episodes")
	if root != want {
		t.Fatalf("root = %q, want %q", root, want)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.root); err != nil {
		t.Fatalf("episode directory was not created on the data volume: %v", err)
	}
}

func TestDefaultRootWithoutDataVolume(t *testing.T) {
	root, err := defaultRoot(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if root != DefaultRoot {
		t.Fatalf("root = %q, want %q", root, DefaultRoot)
	}
}

func TestDefaultRootRejectsInvalidDataVolume(t *testing.T) {
	volume := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(volume, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultRoot(volume); err == nil {
		t.Fatal("invalid data volume silently fell back to root storage")
	}
}

func TestExplicitEpisodeRootIsPreserved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom", "episodes")
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.root != root {
		t.Fatalf("root = %q, want explicit root %q", m.root, root)
	}
}
