//go:build windows

package commands

import (
	"fmt"
	"os/exec"
)

// preAuthElevation checks that the current process is running with
// Administrator privileges, which are required for raw disk access on Windows.
func preAuthElevation() error {
	// "net session" succeeds only when running as Administrator.
	if err := exec.Command("net", "session").Run(); err != nil {
		return fmt.Errorf("administrator privileges required — please re-run this command from an elevated (Administrator) terminal")
	}
	return nil
}

// elevationHint returns a user-facing message about privilege requirements.
func elevationHint() string {
	return "Administrator privileges are required for disk writing."
}
