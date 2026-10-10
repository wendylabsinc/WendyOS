package commands

import (
	"context"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

// TestMain sandboxes HOME/USERPROFILE to a throwaway directory for the whole
// package. Several commands (notably `wendy tour`) derive real filesystem paths
// from the user's home directory; without this guard a test that drives those
// code paths would scaffold into the developer's real ~/Documents or mutate
// ~/.wendy. Individual tests may still override HOME via t.Setenv. It also
// clears WENDY_DEVICE, which the root command would otherwise apply.
func TestMain(m *testing.M) {
	// UI initialization starts LAN discovery immediately, even if its returned
	// tea.Cmd is never run. Default to a closed, empty stream so UI tests cannot
	// leave live probes racing with another test's global stubs (or use the
	// developer's LAN). Stream-specific tests install their own event fixtures;
	// the discovery package tests the real engine with controlled backends.
	lanStreamFn = func(context.Context, discovery.StreamOptions) <-chan discovery.LANEvent {
		events := make(chan discovery.LANEvent)
		close(events)
		return events
	}

	// A developer who exported WENDY_DEVICE (as the docs suggest) must not
	// have it copied into deviceFlag by every test that executes the root
	// command, and leak from there into later tests.
	os.Unsetenv("WENDY_DEVICE")
	if tmp, err := os.MkdirTemp("", "wendy-commands-test-home-"); err == nil {
		os.Setenv("HOME", tmp)
		os.Setenv("USERPROFILE", tmp) // Windows: os.UserHomeDir consults this
		code := m.Run()
		os.RemoveAll(tmp)
		os.Exit(code)
	}
	os.Exit(m.Run())
}
