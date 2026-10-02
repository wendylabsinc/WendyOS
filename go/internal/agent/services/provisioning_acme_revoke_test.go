package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const revokeTenant = "11111111-1111-4111-8111-111111111111"

func revokeTestCert(t *testing.T, principal string, serial int64) (string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), cert
}
func revokeTestContext(t *testing.T, principal string) context.Context {
	_, cert := revokeTestCert(t, principal, 90)
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}, HandshakeComplete: true}}})
}
func revokeTestService(t *testing.T) (*ProvisioningService, *agentpbv2.RevokeACMECertificateRequest) {
	t.Helper()
	dir := t.TempDir()
	principal := "spiffe://wendy.sh/tenant/" + revokeTenant + "/device/box"
	certificate, _ := revokeTestCert(t, principal, 42)
	state := provisioningState{Enrolled: true, CloudHost: "api.example:443", PrincipalURI: principal, CertPEM: certificate, ACMEDirectoryURL: "https://acme.example/" + revokeTenant + "/acme/directory"}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "provisioning.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"device-key.pem", "acme-account-key.pem"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("recovery-key"), 0400); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewProvisioningService(zap.NewNop(), dir)
	return svc, &agentpbv2.RevokeACMECertificateRequest{ExpectedPrincipalUri: principal, ExpectedCertificateSha256: svc.certificateFingerprintLocked()}
}
func stubRevoke(t *testing.T, fn func(context.Context, acmeenroll.Config, string, string) error) {
	t.Helper()
	old := revokeACMECertificate
	revokeACMECertificate = fn
	t.Cleanup(func() { revokeACMECertificate = old })
}
func assertRevokeKeysRetained(t *testing.T, svc *ProvisioningService) {
	t.Helper()
	for _, name := range []string{"device-key.pem", "acme-account-key.pem", "provisioning.json"} {
		if _, err := os.Stat(filepath.Join(svc.configPath, name)); err != nil {
			t.Fatalf("recovery material removed %s: %v", name, err)
		}
	}
}

func TestACMERevokeRequiresSameTenantOperator(t *testing.T) {
	for _, principal := range []string{"", "spiffe://wendy.sh/tenant/22222222-2222-4222-8222-222222222222/operator/op", "spiffe://wendy.sh/tenant/" + revokeTenant + "/device/other", "spiffe://wendy.sh/tenant/" + revokeTenant + "/service/svc"} {
		t.Run(principal, func(t *testing.T) {
			svc, req := revokeTestService(t)
			called := false
			stubRevoke(t, func(context.Context, acmeenroll.Config, string, string) error { called = true; return nil })
			ctx := context.Background()
			if principal != "" {
				ctx = revokeTestContext(t, principal)
			}
			if _, err := NewProvisioningServiceV2(svc).RevokeACMECertificate(ctx, req); err == nil || called {
				t.Fatalf("unauthorized revocation: %v", err)
			}
			assertRevokeKeysRetained(t, svc)
		})
	}
}
func TestACMERevokeBindingAndFailureRetainRecoveryKeys(t *testing.T) {
	for _, failure := range []string{"principal", "fingerprint", "directory", "network"} {
		t.Run(failure, func(t *testing.T) {
			svc, req := revokeTestService(t)
			calls := 0
			stubRevoke(t, func(context.Context, acmeenroll.Config, string, string) error {
				calls++
				return errors.New("uncertain")
			})
			switch failure {
			case "principal":
				req.ExpectedPrincipalUri += "-other"
			case "fingerprint":
				req.ExpectedCertificateSha256 = strings.Repeat("ab", 32)
			case "directory":
				raw, _ := os.ReadFile(svc.statePath())
				var state provisioningState
				_ = json.Unmarshal(raw, &state)
				state.ACMEDirectoryURL = "https://acme.example/22222222-2222-4222-8222-222222222222/acme/directory"
				raw, _ = json.Marshal(state)
				_ = os.WriteFile(svc.statePath(), raw, 0600)
			}
			ctx := revokeTestContext(t, "spiffe://wendy.sh/tenant/"+revokeTenant+"/operator/op")
			if _, err := NewProvisioningServiceV2(svc).RevokeACMECertificate(ctx, req); err == nil {
				t.Fatal("failure accepted")
			}
			if (calls > 0) != (failure == "network") {
				t.Fatalf("unexpected account use: %d", calls)
			}
			assertRevokeKeysRetained(t, svc)
			if svc.revocationConfirmedLocked() {
				t.Fatal("uncertain revocation acknowledged")
			}
		})
	}
}
func TestACMERevokeDurableAckAndGuardedReset(t *testing.T) {
	svc, req := revokeTestService(t)
	calls := 0
	stubRevoke(t, func(_ context.Context, cfg acmeenroll.Config, path, certificate string) error {
		calls++
		if cfg.DeviceID != "box" || path != filepath.Join(svc.configPath, "acme-account-key.pem") || certificate != svc.certPEM {
			t.Fatal("wrong account/certificate")
		}
		return nil
	})
	ctx := revokeTestContext(t, "spiffe://wendy.sh/tenant/"+revokeTenant+"/operator/op")
	v2 := NewProvisioningServiceV2(svc)
	if _, err := v2.Unprovision(ctx, &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: req.ExpectedPrincipalUri, ExpectedCertificateSha256: req.ExpectedCertificateSha256}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reset before revocation: %v", err)
	}
	ack, err := v2.RevokeACMECertificate(ctx, req)
	if err != nil || ack.GetCertificateSerial() != "2a" {
		t.Fatalf("revoke: %v", err)
	}
	assertRevokeKeysRetained(t, svc)
	reloaded := NewProvisioningService(zap.NewNop(), svc.configPath)
	if !reloaded.revocationConfirmedLocked() {
		t.Fatal("ack lost on restart")
	}
	if _, err := NewProvisioningServiceV2(reloaded).RevokeACMECertificate(ctx, req); err != nil || calls != 1 {
		t.Fatalf("non-idempotent retry: %v, %d", err, calls)
	}
	if _, err := reloaded.Unprovision(ctx, &agentpb.UnprovisionRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("legacy bypass: %v", err)
	}
	reset := &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: req.ExpectedPrincipalUri, ExpectedCertificateSha256: req.ExpectedCertificateSha256}
	if _, err := NewProvisioningServiceV2(reloaded).Unprovision(ctx, reset); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"device-key.pem", "acme-account-key.pem", acmeRevocationFile, "provisioning.json"} {
		if _, err := os.Stat(filepath.Join(svc.configPath, name)); !os.IsNotExist(err) {
			t.Fatalf("reset retained %s", name)
		}
	}
}
func TestACMERevocationAckDoesNotCoverRotatedCertificate(t *testing.T) {
	svc, req := revokeTestService(t)
	if err := writeACMERevocationRecord(svc.configPath, acmeRevocationRecord{Principal: req.ExpectedPrincipalUri, Fingerprint: req.ExpectedCertificateSha256, Serial: "2a"}); err != nil {
		t.Fatal(err)
	}
	svc.certPEM, _ = revokeTestCert(t, svc.principalURI, 43)
	if svc.revocationConfirmedLocked() {
		t.Fatal("old acknowledgement covers a new leaf")
	}
	if _, err := svc.unprovision(req.ExpectedPrincipalUri, req.ExpectedCertificateSha256); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("rotated leaf reset: %v", err)
	}
	assertRevokeKeysRetained(t, svc)
}
