//go:build !linux

package localmesh

import "os"

// Non-Linux callers only parse configuration; host routing is Linux-only.
func openTCPConfig(path string) (*os.File, error) { return os.Open(path) }
