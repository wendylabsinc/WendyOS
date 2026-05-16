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
	// Create a temp dir for config, then make it read-only so saveState fails.
	tmpDir, err := os.MkdirTemp("", "wendy-prov-savestate-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Pre-create the config dir as read-only so os.WriteFile inside saveState fails.
	if err := os.Chmod(tmpDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	logger := zap.NewNop()
	svc := NewProvisioningService(logger, tmpDir)

	// Attach a fake cloud dialer so the network call succeeds.
	dialer, cleanup := startFakeCloudServer(t, "cert-pem", "chain-pem")
	t.Cleanup(cleanup)
	svc.CloudDialer = dialer

	// First provisioning attempt — saveState will fail because the dir is read-only.
	_, err = svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 7,
		CloudHost:      "fail.wendy.io",
		AssetId:        77,
	})
	if err == nil {
		t.Fatal("expected StartProvisioning to return an error when saveState fails")
	}

	// Restore write permission so the second attempt can succeed.
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		t.Fatalf("Chmod restore: %v", err)
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
