//go:build !darwin && !linux

package commands

// HandBackSudoFiles is a no-op off macOS and Linux, the only platforms where
// wendy runs itself under sudo.
func HandBackSudoFiles() {}
