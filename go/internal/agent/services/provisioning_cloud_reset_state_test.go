package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCloudResetInvalidExistingStateBlocksRecovery(t *testing.T) {
	for _, failure := range []string{"signature", "status", "principal", "certificate", "endpoint", "private-key", "premature-completion"} {
		t.Run(failure, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			obstruction := filepath.Join(svc.configPath, ".provisioned")
			if err := os.Remove(obstruction); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Mkdir(obstruction, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(obstruction, "obstruction"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); err == nil {
				t.Fatal("expected partial cleanup")
			}
			state, err := svc.readProvisioningState()
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "signature":
				state.Unenrollment.Receipt = []byte("invalid receipt")
			case "status":
				state.Unenrollment.Status = "unknown"
			case "principal":
				state.PrincipalURI = "different device"
			case "certificate":
				state.CertPEM = ""
			case "endpoint":
				state.CloudHost = "another.example:443"
			case "private-key":
				state.KeyPEM = "must not exist"
			case "premature-completion":
				state.Unenrollment.Status = unenrollmentCompleted
			}
			// Directly exercise the validation for the private-key case: saveState
			// intentionally refuses to serialize that legacy-only field.
			if failure == "private-key" {
				if _, err := validateUnenrollmentState(state); err == nil {
					t.Fatal("private key accepted in recovery state")
				}
				return
			}
			if err := svc.saveState(state); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(obstruction); err != nil {
				t.Fatal(err)
			}
			recovered := NewProvisioningService(zap.NewNop(), svc.configPath)
			if _, err := NewProvisioningServiceV2(recovered).IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{}); status.Code(err) != codes.Unavailable {
				t.Fatalf("invalid recovery exposed as clean: %v", err)
			}
			if _, err := NewProvisioningServiceV2(recovered).StartProvisioning(context.Background(), &agentpbv2.StartProvisioningRequest{}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("enrollment allowed during invalid recovery: %v", err)
			}
			if _, err := os.Stat(filepath.Join(svc.configPath, "acme-account-key.pem")); err != nil {
				t.Fatal("recovery erased credentials with invalid evidence", err)
			}
		})
	}
}

func TestCloudResetCompletionSupersededInExistingState(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); err != nil {
		t.Fatal(err)
	}
	if receipt, err := svc.cloudCompletion(); err != nil || len(receipt) == 0 {
		t.Fatal("completion unavailable", err)
	}
	// Normal enrollment replaces provisioning.json, rather than needing to
	// remove a separate receipt or importing a historical development file.
	if err := svc.saveState(&provisioningState{}); err != nil {
		t.Fatal(err)
	}
	if receipt, err := svc.cloudCompletion(); err != nil || len(receipt) != 0 {
		t.Fatal("old completion survived replacement", err)
	}
}
