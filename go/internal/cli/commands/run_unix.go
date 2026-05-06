//go:build darwin || linux

package commands

import "os/exec"

// configurePostStartProcessGroup is a no-op on Unix. The default
// exec.CommandContext behavior — SIGKILL to the direct child — is enough
// because hooks are spawned via `sh -c`, and once the parent shell exits its
// foreground children are reaped through the process group cleanup the OS
// already performs.
//
// Returns a no-op finalizer for API parity with the Windows implementation.
func configurePostStartProcessGroup(_ *exec.Cmd) func() {
	return func() {}
}
