package services

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func withArduinoRouterDir(t *testing.T, dir string) {
	t.Helper()
	orig := oci.ArduinoRouterDir
	t.Cleanup(func() { oci.ArduinoRouterDir = orig })
	oci.ArduinoRouterDir = dir
}

func agentEntitlementTypes(t *testing.T) []string {
	t.Helper()
	client, cleanup := startAgentServer(t, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{})
	defer cleanup()
	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
	return resp.GetEntitlementTypes()
}

func TestGetAgentVersionHidesArduinoBridgeWithoutRouter(t *testing.T) {
	withArduinoRouterDir(t, filepath.Join(t.TempDir(), "absent"))
	if types := agentEntitlementTypes(t); slices.Contains(types, "arduino-bridge") {
		t.Errorf("entitlement types %v list arduino-bridge without a router", types)
	}
}

func TestGetAgentVersionShowsArduinoBridgeWithRouter(t *testing.T) {
	withArduinoRouterDir(t, t.TempDir())
	if types := agentEntitlementTypes(t); !slices.Contains(types, "arduino-bridge") {
		t.Errorf("entitlement types %v lack arduino-bridge with a router", types)
	}
}
