package services

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func localResetFixture(t *testing.T) (*ProvisioningService, *agentpbv2.UnprovisionRequest, context.Context) {
	t.Helper()
	svc, req, ctx := cloudResetFixture(t)
	req.LocalOnly = true
	req.RevocationProof = nil
	req.CloudDeletion = nil
	req.CloudResetAuthorization = nil
	// A local reset must work without any Cloud verifier or signed-time source.
	svc.trustedTime = nil
	svc.verifyCloudReset = nil
	return svc, req, ctx
}

func TestLocalResetErasesCredentialsWithoutCloudEvidence(t *testing.T) {
	svc, req, ctx := localResetFixture(t)
	for _, name := range []string{"cloud-relay", "acme-account-key.pem.d"} {
		if err := os.Mkdir(filepath.Join(svc.configPath, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(svc.configPath, name, "credential"), []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	key := svc.keyPEM
	beforeCalled := false
	svc.BeforeUnprovisioned = func() error {
		beforeCalled = true
		if _, _, _, enrolled := svc.ProvisioningInfo(); enrolled {
			t.Error("cleanup hook observed an enrolled device")
		}
		return nil
	}
	restarted := make(chan struct{})
	svc.OnUnprovisioned = func() { close(restarted) }
	v2 := NewProvisioningServiceV2(svc)
	info, err := v2.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if err != nil || !info.GetProvisioned().GetLocalResetSupported() || info.GetProvisioned().GetCloudAuthenticatedResetSupported() {
		t.Fatalf("local reset capability depends on Cloud availability: %v, %v", info, err)
	}
	reply, err := v2.Unprovision(ctx, req)
	if err != nil || !reply.GetLocalResetCompleted() || len(reply.GetUnenrollmentCompletion()) != 0 {
		t.Fatalf("local reset did not complete independently: %v, %v", reply, err)
	}
	if !beforeCalled {
		t.Fatal("dependent credential cleanup hook was skipped")
	}
	for _, b := range key {
		if b != 0 {
			t.Fatal("in-memory private key was not zeroed")
		}
	}
	assertLocalResetClean(t, svc)
	assertLocalResetClean(t, NewProvisioningService(zap.NewNop(), svc.configPath))
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("agent restart callback was skipped")
	}
}

func assertLocalResetClean(t *testing.T, svc *ProvisioningService) {
	t.Helper()
	for _, name := range []string{"device-key.pem", "device.pem", "ca.pem", ".provisioned", "acme-account-key.pem", "cloud-relay", "acme-account-key.pem.d"} {
		if _, err := os.Stat(filepath.Join(svc.configPath, name)); !os.IsNotExist(err) {
			t.Fatalf("credential %s retained: %v", name, err)
		}
	}
	state, err := svc.readProvisioningState()
	if err != nil || state.Enrolled || state.Unenrollment != nil || state.PrincipalURI != "" || state.CloudHost != "" || state.CertPEM != "" || state.ChainPEM != "" {
		t.Fatalf("reset retained enrollment state: %#v, %v", state, err)
	}
	if state.LocalReset == nil || *state.LocalReset != (localResetState{Version: 1, Status: unenrollmentCompleted}) || !svc.LocalResetCompleted() {
		t.Fatalf("local reset left no identity-free completion marker for startup cleanup: %#v", state.LocalReset)
	}
	info, err := NewProvisioningServiceV2(svc).IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{})
	if err != nil || info.GetNotProvisioned() == nil || len(info.GetNotProvisioned().GetUnenrollmentCompletion()) != 0 {
		t.Fatalf("local reset misreported Cloud completion: %v, %v", info, err)
	}
}

func TestLocalResetRequiresExactIdentityAndOperator(t *testing.T) {
	for _, failure := range []string{"missing-operator", "unfinished-handshake", "other-tenant", "device-caller", "principal", "fingerprint", "empty-fingerprint", "cloud-evidence", "cancelled", "not-local"} {
		t.Run(failure, func(t *testing.T) {
			svc, req, ctx := localResetFixture(t)
			switch failure {
			case "missing-operator":
				ctx = context.Background()
			case "unfinished-handshake", "other-tenant", "device-caller":
				p, _ := peer.FromContext(ctx)
				tlsState := p.AuthInfo.(credentials.TLSInfo).State
				actor := *tlsState.PeerCertificates[0]
				if failure == "unfinished-handshake" {
					tlsState.HandshakeComplete = false
				} else {
					principal := req.ExpectedPrincipalUri
					if failure == "other-tenant" {
						principal = "spiffe://wendy.sh/tenant/44444444-4444-4444-8444-444444444444/operator/op"
					}
					uri, _ := url.Parse(principal)
					actor.URIs = []*url.URL{uri}
				}
				tlsState.PeerCertificates = []*x509.Certificate{&actor}
				ctx = peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: tlsState}})
			case "principal":
				req.ExpectedPrincipalUri = "another device"
			case "fingerprint":
				req.ExpectedCertificateSha256 = "wrong"
			case "empty-fingerprint":
				req.ExpectedCertificateSha256 = ""
			case "cloud-evidence":
				req.CloudDeletion = []byte("not local authority")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "not-local":
				req.LocalOnly = false
			}
			before, err := os.ReadFile(svc.statePath())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); err == nil {
				t.Fatal("invalid local reset accepted")
			}
			after, err := os.ReadFile(svc.statePath())
			if err != nil || string(before) != string(after) {
				t.Fatalf("rejected request changed durable state: %v", err)
			}
			if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); err != nil || !svc.enrolled {
				t.Fatal("rejected request removed enrollment", err)
			}
		})
	}
}

func TestLocalResetRecoversAfterPartialCredentialCleanup(t *testing.T) {
	svc, req, ctx := localResetFixture(t)
	unblock := blockLocalResetCleanup(t, svc)
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.Internal {
		t.Fatalf("partial credential cleanup reported success: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); !os.IsNotExist(err) {
		t.Fatalf("test did not reach partial key deletion: %v", err)
	}
	state, err := svc.readProvisioningState()
	if err != nil || validateLocalResetState(state) != nil {
		t.Fatalf("pending authorization lost after partial cleanup: %#v, %v", state, err)
	}
	blockedSvc := NewProvisioningService(zap.NewNop(), svc.configPath)
	if _, err := NewProvisioningServiceV2(blockedSvc).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("blocked recovery exposed clean state: %v", err)
	}
	if _, err := NewProvisioningServiceV2(blockedSvc).StartProvisioning(ctx, &agentpbv2.StartProvisioningRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("blocked recovery permitted new enrollment: %v", err)
	}
	unblock()
	assertLocalResetClean(t, NewProvisioningService(zap.NewNop(), svc.configPath))
}

func TestLocalResetRetriesPartialCleanupWithoutRestart(t *testing.T) {
	svc, req, ctx := localResetFixture(t)
	v2 := NewProvisioningServiceV2(svc)
	unblock := blockLocalResetCleanup(t, svc)
	key := append([]byte(nil), svc.keyPEM...)
	beforeCalled := false
	svc.BeforeUnprovisioned = func() error { beforeCalled = true; return nil }
	reply, err := v2.Unprovision(ctx, req)
	if status.Code(err) != codes.Internal || reply.GetLocalResetCompleted() {
		t.Fatalf("partial cleanup acknowledged a reset: %v, %v", reply, err)
	}
	info, err := v2.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if err != nil || info.GetProvisioned().GetPrincipalUri() != req.ExpectedPrincipalUri || !bytes.Equal(key, svc.keyPEM) || svc.certificateFingerprintLocked() != req.ExpectedCertificateSha256 {
		t.Fatalf("partial cleanup lost the live identity needed for authenticated retry: %v, %v", info, err)
	}
	if beforeCalled {
		t.Fatal("partial cleanup fired the unprovisioned callback")
	}
	if _, err := v2.StartProvisioning(ctx, &agentpbv2.StartProvisioningRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("partial cleanup permitted new enrollment: %v", err)
	}
	unblock()
	reply, err = v2.Unprovision(ctx, req)
	if err != nil || !reply.GetLocalResetCompleted() || !beforeCalled {
		t.Fatalf("same-process retry did not finish reset: %v, %v", reply, err)
	}
	assertLocalResetClean(t, svc)
	if len(svc.keyPEM) != 0 {
		t.Fatal("successful retry retained the in-memory key")
	}
}

func blockLocalResetCleanup(t *testing.T, svc *ProvisioningService) func() {
	t.Helper()
	blocked := filepath.Join(svc.configPath, "device.pem")
	if err := os.Remove(blocked); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(blocked, "blocker")
	if err := os.WriteFile(blocker, []byte("cannot remove nonempty directory"), 0600); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalResetRecoveryRejectsUntrustedOrChangedState(t *testing.T) {
	for _, failure := range []string{"version", "principal", "fingerprint", "embedded-key", "cloud-receipt", "file-permissions", "directory-permissions"} {
		t.Run(failure, func(t *testing.T) {
			svc, req, _ := localResetFixture(t)
			state, err := svc.readProvisioningState()
			if err != nil {
				t.Fatal(err)
			}
			state.LocalReset = &localResetState{Version: 1, Status: unenrollmentPending, Principal: req.ExpectedPrincipalUri, Fingerprint: req.ExpectedCertificateSha256}
			switch failure {
			case "version":
				state.LocalReset.Version = 2
			case "principal":
				state.PrincipalURI = "changed"
			case "fingerprint":
				state.LocalReset.Fingerprint = "changed"
			case "embedded-key":
				state.KeyPEM = "must not be persisted"
			case "cloud-receipt":
				state.Unenrollment = &unenrollmentState{Status: unenrollmentPending}
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(svc.statePath(), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if failure == "file-permissions" {
				if err := os.Chmod(svc.statePath(), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "directory-permissions" {
				if err := os.Chmod(svc.configPath, 0777); err != nil {
					t.Fatal(err)
				}
			}
			recovered := NewProvisioningService(zap.NewNop(), svc.configPath)
			if recovered.resetRecoveryErr == nil {
				t.Fatal("invalid local reset was recovered")
			}
			if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); err != nil {
				t.Fatal("untrusted reset erased the device key", err)
			}
		})
	}
}

func TestLocalResetRefusesUnprotectedStorageWithoutBlockingRestart(t *testing.T) {
	svc, req, ctx := localResetFixture(t)
	if err := os.Chmod(svc.configPath, 0770); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(svc.configPath, 0700) })
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unprotected storage accepted a local reset: %v", err)
	}
	state, err := svc.readProvisioningState()
	if err != nil || state.LocalReset != nil || !state.Enrolled {
		t.Fatalf("refused reset left a pending record or lost enrollment: %#v, %v", state, err)
	}
	restarted := NewProvisioningService(zap.NewNop(), svc.configPath)
	if restarted.resetRecoveryErr != nil || !restarted.enrolled {
		t.Fatalf("refused reset blocked the next start: %v", restarted.resetRecoveryErr)
	}
	if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); err != nil {
		t.Fatal("refused reset removed the device key", err)
	}
}
