//go:build !linux && !darwin

package commands

import "os/exec"

func detachMeshWorker(cmd *exec.Cmd) {}
