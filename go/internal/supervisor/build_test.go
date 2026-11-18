package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func writeRootfs(t *testing.T, version, commit, agent string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, data string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("usr/lib/wendyos/version.txt", version)
	if commit != "" {
		write("usr/lib/wendyos/commit", commit)
	}
	write("opt/wendyos/bin/wendy-agent", agent)
	return root
}

func TestBuildIDIsStableForTheSameImage(t *testing.T) {
	a := BuildID(writeRootfs(t, "0.20.2", "abc123", "agent-v1"))
	b := BuildID(writeRootfs(t, "0.20.2", "abc123", "agent-v1"))
	if a == "" || a != b {
		t.Fatalf("want the same non-empty id, got %q and %q", a, b)
	}
}

func TestBuildIDChangesWithTheImage(t *testing.T) {
	base := BuildID(writeRootfs(t, "0.20.2", "abc123", "agent-v1"))
	for name, root := range map[string]string{
		"commit":  writeRootfs(t, "0.20.2", "def456", "agent-v1"),
		"version": writeRootfs(t, "0.20.3", "abc123", "agent-v1"),
		"agent":   writeRootfs(t, "0.20.2", "abc123", "agent-v2"),
	} {
		if got := BuildID(root); got == base {
			t.Errorf("a different %s gave the same build id", name)
		}
	}
}

func TestBuildIDIsUnknownWithoutACommit(t *testing.T) {
	if got := BuildID(writeRootfs(t, "0.20.2", "", "agent-v1")); got != "" {
		t.Fatalf("want an unknown build id, got %q", got)
	}
}
