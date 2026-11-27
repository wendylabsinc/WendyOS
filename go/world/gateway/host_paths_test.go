package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostStateBoundaryAllowsDedicatedWorldDirectory(t *testing.T) {
	root := t.TempDir()
	options := Options{StateBoundaries: []StateBoundary{{root, "world"}}}
	if err := validateHostStatePaths([]string{filepath.Join(root, "world", "config.json"), filepath.Join(root, "world", "state.json"), filepath.Join(root, "world", "state.json.lock"), filepath.Join(root, "world", "evidence"), filepath.Join(t.TempDir(), "external-state")}, options); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Dir(root), filepath.Join(root, "state.json"), filepath.Join(root, "other", "state.json")} {
		if err := validateHostStatePaths([]string{path}, options); err == nil {
			t.Fatalf("accepted host namespace %s", path)
		}
	}
}

func TestHostStateBoundaryRejectsRoomAliasesAndWorldSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	worldDir := filepath.Join(root, "world")
	if err := os.Mkdir(worldDir, 0700); err != nil {
		t.Fatal(err)
	}
	room := filepath.Join(root, "11111111-1111-4111-8111-111111111111.json")
	if err := os.WriteFile(room, []byte("original room"), 0600); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(outside, "hard-link")
	if err := os.Link(room, hardlink); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(outside, "room-link")
	if err := os.Symlink(room, symlink); err != nil {
		t.Fatal(err)
	}
	insideLink := filepath.Join(worldDir, "room-link")
	if err := os.Symlink(room, insideLink); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(worldDir, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	options := Options{StateBoundaries: []StateBoundary{{root, "world"}}}
	for _, path := range []string{hardlink, symlink, insideLink, filepath.Join(escape, "future-state")} {
		if err := validateHostStatePaths([]string{path}, options); err == nil {
			t.Fatalf("accepted alias %s", path)
		}
	}
}
