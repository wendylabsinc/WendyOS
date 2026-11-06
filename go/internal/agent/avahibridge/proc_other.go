//go:build !linux

package avahibridge

import "os/exec"

func setParentDeathSignal(cmd *exec.Cmd) {}
