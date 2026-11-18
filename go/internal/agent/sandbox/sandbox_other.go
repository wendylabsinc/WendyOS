//go:build !linux

// Package sandbox starts helper subprocesses of the root agent as a non-root
// user. Only the Linux agent runs as root, so elsewhere it changes nothing.
package sandbox

import "os/exec"

// Identity is who helpers are started as. Off Linux there is none, and helpers
// start unchanged.
type Identity struct{}

func (*Identity) String() string { return "" }

// Isolated returns no Identity: there is no privilege to drop off Linux.
func Isolated(...string) (*Identity, error) { return nil, nil }

// Lookup returns no Identity: there is no privilege to drop off Linux.
func Lookup(string, ...string) (*Identity, error) { return nil, nil }

// HideMemory does nothing off Linux.
func HideMemory() error { return nil }

// Start is cmd.Start.
func (*Identity) Start(cmd *exec.Cmd) error { return cmd.Start() }
