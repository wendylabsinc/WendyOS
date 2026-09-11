//go:build !linux && !darwin

package inference

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
