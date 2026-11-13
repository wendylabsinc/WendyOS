//go:build !darwin && !linux

package commands

// digestCacheFSEligible reports false: this platform has no file identity
// (fileidentity_other.go), so it has no digest cache either.
func digestCacheFSEligible(string) bool { return false }
