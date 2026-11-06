package so101

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializePinnedSourceAndDetectCorruption(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { removeTemporary(root) })
	first, err := Materialize(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, strings.TrimPrefix(SourceDigest(), "sha256:")) {
		t.Fatalf("unaddressed source: %s", first)
	}
	second, err := Materialize(root)
	if err != nil || first != second {
		t.Fatalf("unstable materialization: %s %v", second, err)
	}
	for _, name := range []string{"model/scene.xml", "assets.lock.json", "viewer-assets.lock.json", "licenses/SO-ARM100.LICENSE", "so101_sim/viewer.js", "wendy.json", "Dockerfile"} {
		if _, err := os.Stat(filepath.Join(first, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(first, "model", "assets")); !os.IsNotExist(err) {
		t.Fatal("downloaded models entered embedded source")
	}
	if _, err := os.Stat(filepath.Join(first, "so101_sim", "vendor")); !os.IsNotExist(err) {
		t.Fatal("downloaded viewer dependencies entered embedded source")
	}
	file := filepath.Join(first, "so101_sim", "server.py")
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(root); err == nil {
		t.Fatal("accepted corrupt source cache")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "changed" {
		t.Fatal("silently replaced modified cache")
	}
}
