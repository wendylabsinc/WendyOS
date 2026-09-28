package commands

import (
	"os"
	"testing"
)

// TestMain sandboxes HOME/USERPROFILE to a throwaway directory for the whole
// package. Several commands (notably `wendy tour`) derive real filesystem paths
// from the user's home directory; without this guard a test that drives those
// code paths would scaffold into the developer's real ~/Documents or mutate
// ~/.wendy. Individual tests may still override HOME via t.Setenv.
//
// It also lets postStart openURL hooks reach browserOpen, which tests swap for
// a recorder: `go test` has no interactive terminal, so the production gate
// (defaultPostStartBrowserAllowed, tested directly) would otherwise suppress
// every hook those tests observe.
func TestMain(m *testing.M) {
	postStartBrowserAllowed = func() bool { return true }
	if tmp, err := os.MkdirTemp("", "wendy-commands-test-home-"); err == nil {
		os.Setenv("HOME", tmp)
		os.Setenv("USERPROFILE", tmp) // Windows: os.UserHomeDir consults this
		code := m.Run()
		os.RemoveAll(tmp)
		os.Exit(code)
	}
	os.Exit(m.Run())
}
