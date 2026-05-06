//go:build windows

package commands

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestLinkOrCopyDir_PathWithCmdMetachars verifies that a junction can be
// created for a target whose path contains characters that cmd.exe would
// otherwise interpret (`&`). This regresses the previous `cmd.exe /C mklink`
// implementation, where such a path could be reparsed by the shell.
func TestLinkOrCopyDir_PathWithCmdMetachars(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "needs & escape")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "marker.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	dst := filepath.Join(root, "link")
	if err := linkOrCopyDir(srcDir, dst); err != nil {
		t.Fatalf("linkOrCopyDir: %v", err)
	}

	// Whether we landed on Symlink, junction, or copy, the marker must be
	// reachable through the destination.
	got, err := os.ReadFile(filepath.Join(dst, "marker.txt"))
	if err != nil {
		t.Fatalf("read through link: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("marker contents = %q, want %q", got, "ok")
	}
}

// TestMakeJunction_LstatReportsSymlink confirms that a junction created via
// the native API is reported by Go as ModeSymlink, which is what the
// docker.go staleness-refresh logic relies on.
func TestMakeJunction_LstatReportsSymlink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	dst := filepath.Join(root, "junction")
	if err := makeJunction(src, dst); err != nil {
		t.Skipf("junction unsupported in this environment: %v", err)
	}
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("expected junction to be reported as ModeSymlink, got %v", info.Mode())
	}
}
