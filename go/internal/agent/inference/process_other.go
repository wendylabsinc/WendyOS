//go:build !linux && !darwin

package inference

import "os/exec"

// ConfigureProcess is a no-op where process groups are unavailable.
func ConfigureProcess(cmd *exec.Cmd) {}
