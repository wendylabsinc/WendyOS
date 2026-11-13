package services

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
)

// otherOperatorPeer is a same-tenant operator that did NOT sign the reset request.
func otherOperatorPeer(t *testing.T) *peer.Peer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("spiffe://wendy.sh/tenant/" + resetTenant + "/operator/other")
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}}
}

func TestCloudResetGoodAuthorityErases(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); err != nil {
		t.Fatalf("verified Cloud reset authority refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); !os.IsNotExist(err) {
		t.Fatalf("device key retained after authorized reset: %v", err)
	}
}

// Ruling 2026-10-15 12:56: after an authenticated unenroll the device keeps
// nothing tying it to the old org/asset and re-enrolls with the same device ID
// in the same or a different org.
func TestCloudResetThenReenrollSameDeviceID(t *testing.T) {
	for name, tenant := range map[string]string{"same-org": resetTenant, "other-org": "77777777-7777-4777-8777-777777777777"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			oldKey := string(svc.keyPEM)
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); err != nil {
				t.Fatal(err)
			}
			// Restart between reset and re-enroll, as a real device would.
			restarted := NewProvisioningService(zap.NewNop(), svc.configPath)
			if restarted.resetRecoveryErr != nil {
				t.Fatal(restarted.resetRecoveryErr)
			}
			var enrollKey []byte
			stubACMEEnroll(t, func(_ context.Context, cfg acmeenroll.Config, _ string, key []byte) (string, string, error) {
				enrollKey = append([]byte(nil), key...)
				return "leaf", "chain", nil
			})
			enroll := acmeProvisionRequest()
			enroll.DeviceId = resetDevice
			enroll.DirectoryUrl = "https://acme.dev.pki.wendy.sh/" + tenant + "/acme/directory"
			resp, err := NewProvisioningServiceV2(restarted).StartACMEProvisioning(context.Background(), enroll)
			if err != nil {
				t.Fatalf("re-enroll after authenticated reset refused: %v", err)
			}
			if want := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + resetDevice; resp.GetPrincipalUri() != want {
				t.Fatalf("principal = %q, want %q", resp.GetPrincipalUri(), want)
			}
			if string(enrollKey) == oldKey {
				t.Fatal("re-enrollment reused the erased device key")
			}
			raw, err := os.ReadFile(restarted.statePath())
			if err != nil {
				t.Fatal(err)
			}
			for _, stale := range []string{"unenrollment", "33333333-3333-4333-8333-333333333333", "api.example"} {
				if strings.Contains(string(raw), stale) {
					t.Fatalf("re-enrolled state still references the old enrollment (%s)", stale)
				}
			}
		})
	}
}

func TestCloudResetCorrectSignatureCannotSubstituteClaims(t *testing.T) {
	for _, name := range []string{"purpose", "issuer", "audience", "expired", "future", "lifetime", "jti", "fingerprint", "operator", "correlation", "extra-claim", "caller-key-url", "tenant", "device", "asset", "peer-not-signer"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			key := renewFixtureAuthorization(t, svc, req, ctx, time.Now())
			unchanged := checkResetStateUnchanged(t, svc)
			parts := strings.Split(string(req.CloudResetAuthorization), ".")
			var header, body map[string]any
			h, _ := base64.RawURLEncoding.DecodeString(parts[0])
			b, _ := base64.RawURLEncoding.DecodeString(parts[1])
			if json.Unmarshal(h, &header) != nil || json.Unmarshal(b, &body) != nil {
				t.Fatal("fixture")
			}
			switch name {
			case "purpose":
				header["typ"] = "tunnel-session-grant+jws"
			case "issuer":
				body["iss"] = "https://evil.example"
			case "audience":
				body["aud"] = "different-agent"
			case "expired":
				body["iat"] = time.Now().Unix() - 60
				body["exp"] = time.Now().Unix() - 30
			case "future":
				body["iat"] = time.Now().Unix() + 60
				body["exp"] = time.Now().Unix() + 90
			case "lifetime":
				body["exp"] = int64(body["iat"].(float64)) + 31
			case "jti":
				body["jti"] = "reused-noncanonical-identifier"
			case "fingerprint":
				body["request_body"] = []byte{1, 2, 3}
			case "operator":
				body["request_jws"] = "forged.operator.signature"
			case "correlation":
				body["correlation_id"] = "different-operation"
			case "extra-claim":
				body["admin"] = true
			case "caller-key-url":
				header["jku"] = "https://evil.example/keys"
			case "tenant", "device", "asset":
				// Cloud-signed and byte-consistent, but bound to another target.
				var deleted cloudpbv2.DeletedAsset
				if err := proto.Unmarshal(req.CloudDeletion, &deleted); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "tenant":
					deleted.OrganizationId = "44444444-4444-4444-8444-444444444444"
				case "device":
					deleted.DeviceId = "55555555-5555-4555-8555-555555555555"
				case "asset":
					deleted.Id = "66666666-6666-4666-8666-666666666666"
				}
				raw, err := proto.Marshal(&deleted)
				if err != nil {
					t.Fatal(err)
				}
				req.CloudDeletion = raw
				body["deletion"] = raw
			case "peer-not-signer":
				ctx = peer.NewContext(ctx, otherOperatorPeer(t))
			}
			req.CloudResetAuthorization = fixtureJWS(t, key, header, body)
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); err == nil {
				t.Fatal("substituted authority accepted")
			}
			unchanged()
		})
	}
}

func TestCloudResetVerifiedPendingRecoversWithoutNetworkOrNewFile(t *testing.T) {
	for _, name := range []string{"before-erasure", "partial-erasure", "readable-file", "writable-directory", "symlink"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			p, _ := peer.FromContext(ctx)
			operator := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
			at := time.Now()
			cloudKey, err := svc.verifyCloudReset(ctx, req.CloudResetAuthorization, svc.cloudHost, svc.principalURI, req.ExpectedCertificateSha256, req.CloudDeletion, operator, at, at)
			if err != nil {
				t.Fatal(err)
			}
			leaves, _ := certs.ParseCertsFromPEM([]byte(svc.certPEM))
			pair, err := certs.TLSKeyPair(svc.certPEM, svc.chainPEM, string(svc.keyPEM))
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := unenrollproof.SignCompletion(unenrollproof.Completion{
				Principal: svc.principalURI, Cloud: svc.cloudHost, AssetID: "33333333-3333-4333-8333-333333333333", Fingerprint: req.ExpectedCertificateSha256,
				AuthorizedAt: at.Unix(), AuthorizedAtNanos: at.UnixNano(), Certificate: leaves[0].Raw, Chain: svc.chainPEM, Revocation: req.RevocationProof,
				CloudDeletion: req.CloudDeletion, CloudAuthorization: req.CloudResetAuthorization, CloudVerificationKey: cloudKey, ResetOperator: operator.Raw,
			}, pair.PrivateKey.(crypto.Signer))
			if err != nil {
				t.Fatal(err)
			}
			state, err := svc.readProvisioningState()
			if err != nil {
				t.Fatal(err)
			}
			state.Unenrollment = &unenrollmentState{Status: unenrollmentPending, Receipt: receipt}
			state.KeyPEM = ""
			if err = svc.saveState(state); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "partial-erasure":
				if err = os.Remove(filepath.Join(svc.configPath, "device-key.pem")); err != nil {
					t.Fatal(err)
				}
			case "readable-file":
				if err = os.Chmod(svc.statePath(), 0644); err != nil {
					t.Fatal(err)
				}
			case "writable-directory":
				if err = os.Chmod(svc.configPath, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err = os.Rename(svc.statePath(), svc.statePath()+".moved"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(svc.statePath()+".moved", svc.statePath()); err != nil {
					t.Fatal(err)
				}
			}
			unchanged := func() {}
			if name == "readable-file" || name == "writable-directory" || name == "symlink" {
				unchanged = checkResetStateUnchanged(t, svc)
			}
			// No time provider, Cloud dialer or HTTPS client is supplied on restart.
			recovered := NewProvisioningService(zap.NewNop(), svc.configPath)
			if name == "readable-file" || name == "writable-directory" || name == "symlink" {
				if recovered.resetRecoveryErr == nil {
					t.Fatal("unprotected saved key authorized erasure")
				}
				unchanged()
				return
			}
			if recovered.resetRecoveryErr != nil {
				t.Fatal(recovered.resetRecoveryErr)
			}
			got, err := recovered.cloudCompletion()
			if err != nil || string(got) != string(receipt) {
				t.Fatal("lost exact receipt", err)
			}
			entries, err := os.ReadDir(svc.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "provisioning.json" {
				t.Fatalf("extra recovery/credential files: %v", entries)
			}
			record, _, err := unenrollproof.ReadCompletion(got)
			if err != nil || record.Version != 2 || !record.AuthorizationTime().Equal(at) {
				t.Fatal("lost authenticated timestamp", err)
			}
			if _, err = x509.ParseCertificate(record.ResetOperator); err != nil {
				t.Fatal(err)
			}
		})
	}
}
