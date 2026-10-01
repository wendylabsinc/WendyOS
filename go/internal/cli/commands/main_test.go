package commands

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// TestMain sandboxes HOME/USERPROFILE to a throwaway directory for the whole
// package. Several commands (notably `wendy tour`) derive real filesystem paths
// from the user's home directory; without this guard a test that drives those
// code paths would scaffold into the developer's real ~/Documents or mutate
// ~/.wendy. Individual tests may still override HOME via t.Setenv. It also
// clears WENDY_DEVICE, which the root command would otherwise apply.
func TestMain(m *testing.M) {
	// Model-only tests can create discovery sessions without running their
	// event loops. Keep their default stream off the developer's live LAN;
	// stream behavior tests supply explicit events through this same seam.
	lanStreamFn = func(context.Context, discovery.StreamOptions) <-chan discovery.LANEvent {
		events := make(chan discovery.LANEvent)
		close(events)
		return events
	}
	// Connection helpers also launch an eager provisioning browse outside
	// the model stream. Use an empty default fixture so that browse cannot
	// outlive a test and probe real devices through another test's dial seam.
	// Tests for provisioning advertisements replace it with explicit rows.
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		return nil, nil
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
