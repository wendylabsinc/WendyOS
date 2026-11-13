package services

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"go.uber.org/zap"
	"golang.org/x/crypto/ocsp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const resetTenant = "11111111-1111-4111-8111-111111111111"
const resetDevice = "22222222-2222-4222-8222-222222222222"

func cloudResetFixture(t *testing.T) (*ProvisioningService, *agentpbv2.UnprovisionRequest, context.Context) {
	t.Helper()
	return cloudResetFixtureAt(t, time.Now())
}

func cloudResetFixtureAt(t *testing.T, now time.Time) (*ProvisioningService, *agentpbv2.UnprovisionRequest, context.Context) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if e != nil {
		t.Fatal(e)
	}
	ca, e = x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	leafKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	principal := "spiffe://wendy.sh/tenant/" + resetTenant + "/device/" + resetDevice
	uri, _ := url.Parse(principal)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, e = x509.CreateCertificate(rand.Reader, leaf, ca, leafKey.Public(), key)
	if e != nil {
		t.Fatal(e)
	}
	leaf, e = x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
	rawKey, e := x509.MarshalPKCS8PrivateKey(leafKey)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	state := provisioningState{Enrolled: true, CloudHost: "api.example:443", PrincipalURI: principal, CertPEM: certPEM, ChainPEM: chain}
	raw, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "provisioning.json"), raw, 0600)
	os.WriteFile(filepath.Join(dir, "device-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey}), 0400)
	os.WriteFile(filepath.Join(dir, "acme-account-key.pem"), []byte("account-key"), 0400)
	evidence, e := ocsp.CreateResponse(ca, ca, ocsp.Response{Status: ocsp.Revoked, SerialNumber: leaf.SerialNumber, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour), RevokedAt: now.Add(-time.Minute)}, key)
	if e != nil {
		t.Fatal(e)
	}
	deleted := &cloudpbv2.DeletedAsset{Id: "33333333-3333-4333-8333-333333333333", OrganizationId: resetTenant, DeviceId: resetDevice, DeletedAt: timestamppb.New(now.Add(-time.Minute))}
	binding, e := proto.Marshal(deleted)
	if e != nil {
		t.Fatal(e)
	}
	actorURI, _ := url.Parse("spiffe://wendy.sh/tenant/" + resetTenant + "/operator/op")
	actorKey, e := mldsa.GenerateKey(mldsa.MLDSA65())
	if e != nil {
		t.Fatal(e)
	}
	actor := &x509.Certificate{SerialNumber: big.NewInt(99), URIs: []*url.URL{actorURI}, NotBefore: now.Add(-4 * time.Hour), NotAfter: now.Add(4 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	actorDER, e := x509.CreateCertificate(rand.Reader, actor, ca, actorKey.Public(), key)
	if e != nil {
		t.Fatal(e)
	}
	actor, e = x509.ParseCertificate(actorDER)
	if e != nil {
		t.Fatal(e)
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{actor}, VerifiedChains: [][]*x509.Certificate{{actor}}}}})
	svc := NewProvisioningService(zap.NewNop(), dir)
	svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
		instant := time.Now()
		return anchored(timesync.TimeWindow{Earliest: instant, Latest: instant}), nil
	}
	ctx = context.WithValue(ctx, resetActorKey{}, actorKey)
	req := &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: principal, ExpectedCertificateSha256: unenrollproof.Fingerprint(leaf), RevocationProof: evidence, CloudDeletion: binding}
	renewFixtureAuthorization(t, svc, req, ctx, time.Now())
	return svc, req, ctx
}

// Persist a development-version receipt directly to exercise historical recovery.
// New requests only create version 2 receipts with Cloud-authenticated authority.
func persistHistoricalCloudReset(t *testing.T, svc *ProvisioningService, req *agentpbv2.UnprovisionRequest) []byte {
	t.Helper()
	leaves, err := certs.ParseCertsFromPEM([]byte(svc.certPEM))
	if err != nil || len(leaves) == 0 {
		t.Fatal("missing fixture certificate", err)
	}
	pair, err := certs.TLSKeyPair(svc.certPEM, svc.chainPEM, string(svc.keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	var deleted cloudpbv2.DeletedAsset
	if err := proto.Unmarshal(req.CloudDeletion, &deleted); err != nil {
		t.Fatal(err)
	}
	record := unenrollproof.Completion{
		Principal: svc.principalURI, Cloud: svc.cloudHost, AssetID: deleted.Id,
		Fingerprint: req.ExpectedCertificateSha256, AuthorizedAt: time.Now().Unix(),
		Certificate: leaves[0].Raw, Chain: svc.chainPEM,
		Revocation: req.RevocationProof, CloudDeletion: req.CloudDeletion,
	}
	receipt, err := unenrollproof.SignCompletion(record, pair.PrivateKey.(crypto.Signer))
	if err != nil {
		t.Fatal(err)
	}
	state, err := svc.readProvisioningState()
	if err != nil {
		t.Fatal(err)
	}
	state.KeyPEM = ""
	state.Unenrollment = &unenrollmentState{Status: unenrollmentPending, Receipt: receipt}
	if err := svc.saveState(state); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func persistHistoricalCloudCompletion(t *testing.T, svc *ProvisioningService, req *agentpbv2.UnprovisionRequest) []byte {
	t.Helper()
	receipt := persistHistoricalCloudReset(t, svc, req)
	// Simulate a reset that already completed in a development version. The
	// production recovery path cannot turn a pending receipt into completion.
	if err := svc.clearCredentialFiles(); err != nil {
		t.Fatal(err)
	}
	if err := svc.saveState(&provisioningState{Unenrollment: &unenrollmentState{
		Status: unenrollmentCompleted, Receipt: receipt,
	}}); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestCloudResetRetiredACMEPaths(t *testing.T) {
	svc, _, ctx := cloudResetFixture(t)
	v2 := NewProvisioningServiceV2(svc)
	if _, e := v2.RevokeACMECertificate(ctx, &agentpbv2.RevokeACMECertificateRequest{}); status.Code(e) != codes.Unimplemented {
		t.Fatal(e)
	}
	if _, e := v2.CheckACMERevocation(ctx, &agentpbv2.RevokeACMECertificateRequest{}); status.Code(e) != codes.Unimplemented {
		t.Fatal(e)
	}
	if _, e := svc.Unprovision(ctx, &agentpb.UnprovisionRequest{}); e == nil {
		t.Fatal("v1 reset allowed for direct PKI")
	}
}
func TestCloudResetFailsClosed(t *testing.T) {
	for _, failure := range []string{"operator", "fingerprint", "principal", "proof", "deletion"} {
		t.Run(failure, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			switch failure {
			case "operator":
				ctx = context.Background()
			case "fingerprint":
				req.ExpectedCertificateSha256 = "wrong"
			case "principal":
				req.ExpectedPrincipalUri = "wrong"
			case "proof":
				req.RevocationProof = nil
			case "deletion":
				req.CloudDeletion = nil
			}
			if _, e := NewProvisioningServiceV2(svc).Unprovision(ctx, req); e == nil {
				t.Fatal("invalid reset allowed")
			}
			for _, name := range []string{"provisioning.json", "device-key.pem", "acme-account-key.pem"} {
				if _, e := os.Stat(filepath.Join(svc.configPath, name)); e != nil {
					t.Fatal("keys erased", name, e)
				}
			}
		})
	}
}
func TestCloudResetHistoricalPendingRequiresLocalRecovery(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-erasure", true: "partial-erasure"}[partial], func(t *testing.T) {
			svc, req, _ := cloudResetFixture(t)
			persistHistoricalCloudReset(t, svc, req)
			if partial {
				if err := os.Remove(filepath.Join(svc.configPath, "device-key.pem")); err != nil {
					t.Fatal(err)
				}
			}
			original := map[string][]byte{}
			entries, err := os.ReadDir(svc.configPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				data, err := os.ReadFile(filepath.Join(svc.configPath, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				original[entry.Name()] = data
			}
			if _, err := svc.unprovision(req.ExpectedPrincipalUri, req.ExpectedCertificateSha256); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("historical pending record authorized erasure: %v", err)
			}
			recovered := NewProvisioningService(zap.NewNop(), svc.configPath)
			v2 := NewProvisioningServiceV2(recovered)
			if _, err := v2.IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{}); status.Code(err) != codes.Unavailable {
				t.Fatalf("pending reset reported complete: %v", err)
			}
			if _, err := v2.StartProvisioning(context.Background(), &agentpbv2.StartProvisioningRequest{}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("pending reset permitted new enrollment: %v", err)
			}
			for name, before := range original {
				after, err := os.ReadFile(filepath.Join(svc.configPath, name))
				if err != nil || string(after) != string(before) {
					t.Fatalf("pending reset recovery changed %s: %v", name, err)
				}
			}
		})
	}
}

func TestCloudResetHistoricalCompletionRemainsReadable(t *testing.T) {
	svc, req, _ := cloudResetFixture(t)
	expected := persistHistoricalCloudCompletion(t, svc, req)
	recovered := NewProvisioningService(zap.NewNop(), svc.configPath)
	state, err := NewProvisioningServiceV2(recovered).IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{})
	if err != nil || state.GetNotProvisioned() == nil || string(state.GetNotProvisioned().GetUnenrollmentCompletion()) != string(expected) {
		t.Fatalf("historical completion unavailable: %v, %v", state, err)
	}
}
