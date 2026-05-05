//go:build darwin || linux

package commands

import (
	"fmt"
	"os/exec"
)

// preAuthElevation pre-authenticates sudo so the password prompt appears
// on the raw terminal before any TUI takes over.
func preAuthElevation() error {
	fmt.Println("You may be prompted for your password (sudo is required).")
	if err := exec.Command("sudo", "-v").Run(); err != nil {
		return fmt.Errorf("sudo authentication failed: %w", err)
	}
	return nil
}

// elevationHint returns a message about what to expect for privilege elevation.
func elevationHint() string {
	return "You may be prompted for your password (sudo is required)."
}

