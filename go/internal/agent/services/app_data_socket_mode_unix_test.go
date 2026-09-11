//go:build unix

package services

import (
	"os"
	"syscall"
	"testing"
)

// TestAppDataSocketDirectoryModeIsStated covers the umask dependency: MkdirAll
// applies the process umask to the requested mode, so without an explicit
// Chmod the directory guarding an app's private socket was as tight, or as
// loose, as the agent's umask happened to be.
func TestAppDataSocketDirectoryModeIsStated(t *testing.T) {
	// A restrictive umask is the case that actually bit: 0o027 would have left
	// the directory at 0o750 by luck, so use one that clears a bit the mode
	// asks for.
	previous := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(previous) })

	manager := newTestDataSocketManager(t)
	dir, err := manager.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("socket directory mode = %o, want 750; the umask decided it", got)
	}
}
