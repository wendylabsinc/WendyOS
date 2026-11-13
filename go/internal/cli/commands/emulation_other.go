//go:build !darwin

package commands

// rosettaTranslated is false off macOS: Rosetta 2 exists only there.
func rosettaTranslated() bool { return false }
