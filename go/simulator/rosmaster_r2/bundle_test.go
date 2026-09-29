package rosmaster_r2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleMaterializesCompleteBuildInputsAndRejectsCorruption(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { removeTemporary(root) })
	path, err := Materialize(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"Dockerfile", "wendy.json", "compatibility.json", "entrypoint.sh", "r2_sim/simulation.py", "r2_sim/ros.py", "r2_sim/viewer.js", "r2_sim/robot-model.js", "r2_sim/environment.js", "r2_sim/visuals.js", "r2_sim/vendor/three.module.js", "ros_ws/src/r2_command_ingress/src/command_ingress.cpp"} {
		if _, err := os.Stat(filepath.Join(path, file)); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := digestTree(os.DirFS(path))
	if err != nil || digest != SourceDigest() {
		t.Fatalf("materialized source identity: %q, %v", digest, err)
	}
	if cached, err := Materialize(root); err != nil || cached != path {
		t.Fatalf("cache reuse: %q, %v", cached, err)
	}
	file := filepath.Join(path, "r2_sim", "simulation.py")
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(root); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("corrupt cache accepted: %v", err)
	}
}
