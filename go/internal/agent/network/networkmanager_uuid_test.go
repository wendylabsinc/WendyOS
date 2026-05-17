package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// fakeNmcliManager returns an NMCLINetworkManager whose nmcli is a stub script.
// The stub reports a single saved WiFi profile (wifi-uuid-1) plus an ethernet
// connection (eth-uuid). listKnownProfiles filters to 802-11-wireless, so only
// wifi-uuid-1 should be reachable via the UUID-based mutation paths.
func fakeNmcliManager(t *testing.T) *NMCLINetworkManager {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "nmcli")
	body := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "show" ]; then
    echo "HomeWiFi:wifi-uuid-1:802-11-wireless:10"
    echo "Ethernet:eth-uuid:802-3-ethernet:-999"
    exit 0
  fi
done
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake nmcli: %v", err)
	}
	return &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: script}
}

func TestForgetWiFiNetworkByUUID_RejectsNonWiFiUUID(t *testing.T) {
	n := fakeNmcliManager(t)

	err := n.ForgetWiFiNetworkByUUID(context.Background(), "eth-uuid")
	if err == nil {
		t.Fatal("expected error deleting a non-WiFi UUID, got nil")
	}
	if !strings.Contains(err.Error(), "unknown WiFi network UUID") {
		t.Fatalf("error = %q; want it to mention unknown WiFi network UUID", err)
	}

	if err := n.ForgetWiFiNetworkByUUID(context.Background(), "wifi-uuid-1"); err != nil {
		t.Fatalf("forgetting a known WiFi UUID: %v", err)
	}
}

func TestSetWiFiNetworkPriorityByUUID_RejectsNonWiFiUUID(t *testing.T) {
	n := fakeNmcliManager(t)

	err := n.SetWiFiNetworkPriorityByUUID(context.Background(), "eth-uuid", 5)
	if err == nil {
		t.Fatal("expected error reprioritizing a non-WiFi UUID, got nil")
	}
	if !strings.Contains(err.Error(), "unknown WiFi network UUID") {
		t.Fatalf("error = %q; want it to mention unknown WiFi network UUID", err)
	}

	if err := n.SetWiFiNetworkPriorityByUUID(context.Background(), "wifi-uuid-1", 5); err != nil {
		t.Fatalf("reprioritizing a known WiFi UUID: %v", err)
	}
}

func TestReorderKnownWiFiNetworksByUUID_RejectsNonWiFiUUID(t *testing.T) {
	n := fakeNmcliManager(t)

	err := n.ReorderKnownWiFiNetworksByUUID(context.Background(), []string{"wifi-uuid-1", "eth-uuid"})
	if err == nil {
		t.Fatal("expected error reordering with a non-WiFi UUID, got nil")
	}
	if !strings.Contains(err.Error(), "unknown WiFi network UUID") {
		t.Fatalf("error = %q; want it to mention unknown WiFi network UUID", err)
	}

	if err := n.ReorderKnownWiFiNetworksByUUID(context.Background(), []string{"wifi-uuid-1"}); err != nil {
		t.Fatalf("reordering known WiFi UUIDs: %v", err)
	}
}
