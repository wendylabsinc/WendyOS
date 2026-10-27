package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveCacheTreeReadOnlyDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	nested := filepath.Join(root, "simulator", "g1_sim")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "simulation.py"), []byte("cached"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := removeCacheTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("cache still exists: %v", err)
	}
}

func TestRemoveCacheTreeDoesNotFollowLinks(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(outside, "keep")
	if err := os.WriteFile(file, []byte("keep"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o755) })
	root := filepath.Join(parent, "cache")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := removeCacheTree(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("outside mode changed: %o", info.Mode().Perm())
	}
	if content, err := os.ReadFile(file); err != nil || string(content) != "keep" {
		t.Fatalf("outside file changed: %q, %v", content, err)
	}
	if err := removeCacheTree(filepath.Join(parent, "missing")); err != nil {
		t.Fatal(err)
	}
}
