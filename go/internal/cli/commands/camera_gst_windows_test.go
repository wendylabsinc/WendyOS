//go:build windows

package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGstLaunchFallbackPaths_UsesInstallerEnvRoot(t *testing.T) {
	root := t.TempDir()

	// Isolate from the host: only the MSVC root env var is set.
	for _, env := range gstRootEnvVars {
		t.Setenv(env, "")
	}
	t.Setenv("GSTREAMER_1_0_ROOT_MSVC_X86_64", root)

	prevDefaults := gstDefaultRoots
	gstDefaultRoots = nil
	t.Cleanup(func() { gstDefaultRoots = prevDefaults })
	t.Setenv("ProgramFiles", "")

	want := filepath.Join(root, "bin", gstLaunchName)
	paths := gstLaunchFallbackPaths()
	if len(paths) == 0 || paths[0] != want {
		t.Fatalf("expected first candidate %q, got %v", want, paths)
	}

	// End-to-end: a binary placed at that path resolves without PATH.
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(want, []byte("stub"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PATH", t.TempDir())

	got, err := resolveGSTLaunch()
	if err != nil {
		t.Fatalf("resolveGSTLaunch: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
