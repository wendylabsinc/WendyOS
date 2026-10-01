//go:build !darwin

package t234

// SuppressDiskPrompts is a no-op: only macOS prompts about the flashing disks.
func SuppressDiskPrompts() (release func(), err error) { return func() {}, nil }
