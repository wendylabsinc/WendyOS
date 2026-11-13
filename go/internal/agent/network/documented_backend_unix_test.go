//go:build linux || darwin

package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// The corrected DEVELOPMENT.md and docs/development/debugging.md claim is
// about backend selection, not radio behavior: the Go Agent ignores the old
// Swift/Linux WENDY_NETWORK_MANAGER selector and invokes nmcli. Exercise the
// actual constructor and process boundary with an explicitly synthetic status
// provider; never invoke the host's NetworkManager or ConnMan.
func TestDocumentedGoAgentIgnoresLegacyNetworkBackend(t *testing.T) {
	directory := t.TempDir()
	trace := filepath.Join(directory, "commands")
	state := filepath.Join(directory, "status")
	connman := filepath.Join(directory, "connman-used")
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	// All operations are shell builtins. With this private PATH even an
	// unexpected new command fails instead of reaching a system network tool.
	write(filepath.Join(directory, "nmcli"), `#!/bin/sh
[ "$*" = '-t -f TYPE,STATE,CONNECTION device status' ] || exit 81
printf '%s\n' "$*" >> "$DOC_BACKEND_TRACE"
IFS= read -r state < "$DOC_BACKEND_STATE" || exit 82
printf '%s\n' "$state"
`, 0o755)
	write(filepath.Join(directory, "connmanctl"), `#!/bin/sh
printf 'unexpected ConnMan invocation\n' > "$DOC_CONNMAN_TRACE"
exit 83
`, 0o755)
	t.Setenv("PATH", directory)
	t.Setenv("WENDY_NETWORK_MANAGER", "connman")
	t.Setenv("DOC_BACKEND_TRACE", trace)
	t.Setenv("DOC_BACKEND_STATE", state)
	t.Setenv("DOC_CONNMAN_TRACE", connman)
	manager := NewNMCLINetworkManager(zap.NewNop())
	if manager == nil || manager.nmcliPath != filepath.Join(directory, "nmcli") {
		t.Fatal("DOCUMENTED_NMCLI_BACKEND: legacy selector displaced the owned nmcli executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, item := range []struct {
		wire, ssid string
		connected  bool
	}{
		{`wifi:connected:documented\:network`, "documented:network", true},
		{"wifi:disconnected:", "", false},
	} {
		write(state, item.wire+"\n", 0o600)
		connected, ssid, err := manager.GetWiFiStatus(ctx)
		if err != nil || connected != item.connected || ssid != item.ssid {
			t.Fatalf("DOCUMENTED_NMCLI_STATUS: got connected=%v ssid=%q err=%v", connected, ssid, err)
		}
	}
	commands, err := os.ReadFile(trace)
	if err != nil || string(commands) != strings.Repeat("-t -f TYPE,STATE,CONNECTION device status\n", 2) {
		t.Fatalf("actual status process receipts %q: %v", commands, err)
	}
	if _, err := os.Stat(connman); !os.IsNotExist(err) {
		t.Fatalf("legacy backend was invoked or trace could not be checked: %v", err)
	}
	t.Log("DOCUMENTED_GO_BACKEND_VERIFIED: actual nmcli subprocesses; legacy ConnMan selector ignored; no radio")
}
