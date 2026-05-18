package services

import (
	"context"
	"os"
	"testing"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// TestStartProvisioning_SaveStateFailure verifies that when saveState fails,
// the in-memory provisioning state is NOT mutated. Previously, s.enrolled and
// other fields were set before saveState was called, leaving the agent
// permanently stuck as "already provisioned" even though nothing was persisted.
func TestStartProvisioning_SaveStateFailure(t *testing.T) {
	// Create a temp dir for config.
	tmpDir, err := os.MkdirTemp("", "wendy-prov-savestate-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := zap.NewNop()
	svc := NewProvisioningService(logger, tmpDir)

	// Block saveState by creating a directory at the state file path. When the
	// code tries to open that path for writing it gets EISDIR, which fails
	// deterministically on all platforms and regardless of the running user.
	if err := os.Mkdir(svc.statePath(), 0o755); err != nil {
		t.Fatalf("Mkdir(statePath): %v", err)
	}

	// Attach a fake cloud dialer so the network call succeeds.
	dialer, cleanup := startFakeCloudServer(t, "cert-pem", "chain-pem")
	t.Cleanup(cleanup)
	svc.CloudDialer = dialer

	// First provisioning attempt — saveState will fail because the state path is a directory.
	_, err = svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 7,
		CloudHost:      "fail.wendy.io",
		AssetId:        77,
	})
	if err == nil {
		t.Fatal("expected StartProvisioning to return an error when saveState fails")
	}

	// Remove the blocking directory so the second attempt can write the state file.
	if err := os.Remove(svc.statePath()); err != nil {
		t.Fatalf("Remove(statePath): %v", err)
	}

	// Second provisioning attempt — must NOT be rejected as "already provisioned".
	_, err = svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 7,
		CloudHost:      "fail.wendy.io",
		AssetId:        77,
	})
	if err != nil {
		t.Fatalf("second StartProvisioning after saveState-failure should succeed, got: %v", err)
	}

	// Confirm the agent is now genuinely provisioned.
	resp, err := svc.IsProvisioned(context.Background(), &agentpb.IsProvisionedRequest{})
	if err != nil {
		t.Fatalf("IsProvisioned: %v", err)
	}
	if resp.GetProvisioned() == nil {
		t.Fatal("expected agent to be provisioned after successful second attempt")
	}
}
