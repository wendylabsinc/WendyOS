package services

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEmptyProvisioningStateAllowsEnrollmentWithoutErasingCredentials(t *testing.T) {
	dir := t.TempDir()
	// ACME enrollment keeps an existing ML-DSA-65 key and replaces any other.
	key, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"provisioning.json":    {},
		"device-key.pem":       []byte(key),
		"device.pem":           []byte("previous certificate"),
		"ca.pem":               []byte("previous chain"),
		"acme-account-key.pem": []byte("previous ACME account key"),
		".provisioned":         {},
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	var svc *ProvisioningService
	for range 2 {
		svc = NewProvisioningService(zap.NewNop(), dir)
		v1, err := svc.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
		if err != nil || v1.GetNotProvisioned() == nil {
			t.Fatalf("v1 status: %v, %v", v1, err)
		}
		v2, err := NewProvisioningServiceV2(svc).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
		if err != nil || v2.GetNotProvisioned() == nil {
			t.Fatalf("v2 status: %v, %v", v2, err)
		}
		if v2.GetNotProvisioned().GetUnenrollmentCompletion() != nil {
			t.Fatal("empty state fabricated reset completion")
		}
		for name, want := range files {
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("startup modified %s: %v", name, err)
			}
		}
	}
	stubACMEEnroll(t, func(_ context.Context, _ acmeenroll.Config, _ string, gotKey []byte) (string, string, error) {
		if string(gotKey) != key {
			t.Fatal("enrollment replaced the existing key")
		}
		return "new certificate", "new chain", nil
	})
	resp, err := NewProvisioningServiceV2(svc).StartACMEProvisioning(ctx, acmeProvisionRequest())
	if err != nil {
		t.Fatalf("enrollment from empty state: %v", err)
	}
	restarted := NewProvisioningService(zap.NewNop(), dir)
	status, err := NewProvisioningServiceV2(restarted).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if err != nil || status.GetProvisioned().GetPrincipalUri() != resp.GetPrincipalUri() {
		t.Fatalf("new enrollment did not survive restart: %v, %v", status, err)
	}
}

func TestNonemptyInvalidProvisioningStateStillBlocksEnrollment(t *testing.T) {
	for _, data := range []string{" ", "{", `{"unenrollment":`, `{"enrolled":true,"unenrollment":{"status":"pending","receipt":"invalid"}}`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "provisioning.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			svc := NewProvisioningServiceV2(NewProvisioningService(zap.NewNop(), dir))
			if _, err := svc.IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{}); status.Code(err) != codes.Unavailable {
				t.Fatalf("invalid state exposed as clean: %v", err)
			}
			if _, err := svc.StartACMEProvisioning(context.Background(), acmeProvisionRequest()); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("invalid state allowed enrollment: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("invalid state modified: %v", err)
			}
		})
	}
}

func TestLegacyKeyMigrationReplacesStateAtomically(t *testing.T) {
	dir := t.TempDir()
	state := provisioningState{Enrolled: true, CloudHost: "cloud.example", OrgID: 42, AssetID: 100, KeyPEM: "legacy-private-key", CertPEM: "certificate", ChainPEM: "chain"}
	original, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provisioning.json")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	svc := NewProvisioningService(zap.NewNop(), dir)
	_, _, key := svc.ProvisioningCerts()
	if string(key) != state.KeyPEM {
		t.Fatal("migration lost the device key")
	}
	got, err := io.ReadAll(old)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("migration truncated the original file instead of replacing it: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved provisioningState
	if err := json.Unmarshal(after, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.KeyPEM != "" || !saved.Enrolled || saved.AssetID != state.AssetID {
		t.Fatal("migration did not preserve enrollment without the private key")
	}
}
