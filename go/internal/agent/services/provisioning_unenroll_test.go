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
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	now := time.Now()
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
	actor := &x509.Certificate{URIs: []*url.URL{actorURI}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{actor}, VerifiedChains: [][]*x509.Certificate{{actor}}}}})
	return NewProvisioningService(zap.NewNop(), dir), &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: principal, ExpectedCertificateSha256: unenrollproof.Fingerprint(leaf), RevocationProof: evidence, CloudDeletion: binding}, ctx
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
func TestCloudResetDurableCompletionAndCrashResume(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply-lost", true: "erasure-interrupted"}[crash], func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			historical := filepath.Join(svc.configPath, "acme-revocation.json")
			os.WriteFile(historical, []byte("historical evidence"), 0600)
			if crash {
				if err := os.Remove(filepath.Join(svc.configPath, ".provisioned")); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(svc.configPath, ".provisioned"), 0700); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(svc.configPath, ".provisioned", "obstruction"), []byte("x"), 0600)
			}
			reply, e := NewProvisioningServiceV2(svc).Unprovision(ctx, req)
			if crash {
				if e == nil {
					t.Fatal("expected interrupted cleanup")
				}
				persisted, err := svc.readProvisioningState()
				if err != nil || persisted.Unenrollment == nil || persisted.Unenrollment.Status != unenrollmentPending {
					t.Fatal("pending authorization lost during partial erasure", err)
				}
				if _, err := svc.cloudCompletion(); err == nil {
					t.Fatal("completion published before erasure")
				}
				os.RemoveAll(filepath.Join(svc.configPath, ".provisioned"))
			} else if e != nil || len(reply.GetUnenrollmentCompletion()) == 0 {
				t.Fatal(e)
			}
			recovered := NewProvisioningService(zap.NewNop(), svc.configPath)
			state, e := NewProvisioningServiceV2(recovered).IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{})
			if e != nil || state.GetProvisioned() != nil {
				t.Fatal("recovery incomplete", e)
			}
			receipt, _, e := unenrollproof.ReadCompletion(state.GetNotProvisioned().GetUnenrollmentCompletion())
			if e != nil || receipt.Principal != req.ExpectedPrincipalUri {
				t.Fatal("missing authenticated completion", e)
			}
			for _, name := range []string{"device-key.pem", "device.pem", "ca.pem", ".provisioned", "acme-account-key.pem"} {
				if _, e := os.Stat(filepath.Join(svc.configPath, name)); !os.IsNotExist(e) {
					t.Fatal("key survived", name, e)
				}
			}
			persisted, err := recovered.readProvisioningState()
			if err != nil || persisted.Unenrollment == nil || persisted.Unenrollment.Status != unenrollmentCompleted {
				t.Fatal("completion not retained in existing provisioning state", err)
			}
			if _, err := validateUnenrollmentState(persisted); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(recovered.statePath())
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("unsafe state permissions", err)
			}
			entries, err := os.ReadDir(svc.configPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "provisioning.json" && entry.Name() != "acme-revocation.json" {
					t.Fatalf("additional persistent file: %s", entry.Name())
				}
			}
			if data, e := os.ReadFile(historical); e != nil || string(data) != "historical evidence" {
				t.Fatal("historical artifact changed")
			}
		})
	}
}
