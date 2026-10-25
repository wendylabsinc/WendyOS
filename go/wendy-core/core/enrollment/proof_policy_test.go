package enrollment

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type proofHost struct {
	cert    platform.ProofCertificate
	failure error
	proof   platform.RevocationEvidence
}

func (p proofHost) ParseProofCertificate(context.Context, []byte) (platform.ProofCertificate, error) {
	return p.cert, p.failure
}
func (p proofHost) VerifyMLDSA(ctx context.Context, key, message, signature []byte, algorithm string, certificate bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.failure
}
func (p proofHost) RevocationEvidence(context.Context, []byte, []byte, []byte) (platform.RevocationEvidence, error) {
	return p.proof, p.failure
}
func (p proofHost) CertificateIssuer(context.Context, []byte, string) ([]byte, error) {
	return []byte("issuer"), p.failure
}
func (p proofHost) VerifyCompletionSignature(context.Context, []byte, []byte, []byte) error {
	return p.failure
}
func compact(header, body any) []byte {
	h, _ := json.Marshal(header)
	b, _ := json.Marshal(body)
	return []byte(base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString([]byte("host-verified-signature")))
}
func TestResetPolicyBindsExactRequestIdentityAndFreshness(t *testing.T) {
	at := time.Unix(1700000000, 0)
	leaf := []byte("host-parsed-leaf")
	thumb := sha256.Sum256(leaf)
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/device/" + testDevice
	fingerprint := strings.Repeat("ab", 32)
	deletion, _ := proto.Marshal(&pb.DeletedAsset{Id: testAsset, OrganizationId: testTenant, DeviceId: testDevice, DeletedAt: timestamppb.New(at.Add(-time.Second))})
	request, _ := proto.Marshal(&pb.DeleteAssetRequest{Id: testAsset, ExpectedDeviceId: proto.String(testDevice), ResetCertificateSha256: proto.String(fingerprint)})
	digest := sha256.Sum256(request)
	descriptor := map[string]any{"aud": "https://cloud.wendy.sh/broker", "body_sha256": base64.RawURLEncoding.EncodeToString(digest[:]), "correlation_id": "reset-flow", "expiry": at.Unix() + 29, "iat": at.Unix() - 1, "nonce": base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "operation": "wendycloud.v2.AssetService/DeleteAsset", "target": map[string]any{"tenant": testTenant, "resource": "asset/" + testAsset}}
	operatorJWS := string(compact(map[string]string{"alg": "ML-DSA-65", "kid": base64.RawURLEncoding.EncodeToString(thumb[:])}, descriptor))
	claims := map[string]any{"iss": "https://api.example", "aud": "wendy-agent/unprovision", "iat": at.Unix() - 1, "exp": at.Unix() + 29, "jti": testAsset, "correlation_id": "reset-flow", "deletion": deletion, "request_body": request, "request_jws": operatorJWS}
	header := map[string]string{"alg": "ML-DSA-65", "typ": "asset-reset-authorization+jws", "kid": "authenticated-cloud-key"}
	host := proofHost{cert: platform.ProofCertificate{DER: leaf, Principal: testSession.Principal, NotBefore: at.Add(-time.Hour), NotAfter: at.Add(time.Hour)}}
	verify := func(h proofHost, c map[string]any) error {
		return VerifySavedCloudReset(context.Background(), h, compact(header, c), []byte("trusted-cloud-key"), "api.example:443", principal, fingerprint, deletion, leaf, at, at)
	}
	if err := verify(host, claims); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"iss", "aud", "exp", "jti", "correlation_id", "deletion", "request_body", "request_jws"} {
		t.Run(field, func(t *testing.T) {
			altered := map[string]any{}
			for k, v := range claims {
				altered[k] = v
			}
			switch field {
			case "exp":
				altered[field] = at.Unix()
			case "deletion", "request_body":
				altered[field] = []byte("wrong bytes")
			default:
				altered[field] = "wrong"
			}
			if err := verify(host, altered); err == nil {
				t.Fatal("accepted altered binding")
			}
		})
	}
	host.failure = errors.New("bad host signature")
	if err := verify(host, claims); !errors.Is(err, host.failure) {
		t.Fatalf("signature error lost: %v", err)
	}
	host.failure = nil
	host.cert.Principal = "spiffe://wendy.sh/tenant/7aa28a2c-3747-4dd5-aa38-c863128c1f9d/operator/op"
	if err := verify(host, claims); err == nil {
		t.Fatal("cross-tenant operator accepted")
	}
}
func TestRevocationPolicyRefusesStaleFutureAndDelegatedEvidence(t *testing.T) {
	at := time.Unix(1700000000, 0)
	good := platform.RevocationEvidence{Revoked: true, ThisUpdate: at.Add(-time.Minute), ProducedAt: at.Add(-time.Minute), RevokedAt: at.Add(-time.Hour), NextUpdate: at.Add(time.Minute)}
	for _, which := range []string{"good", "not-revoked", "delegated", "future", "stale", "missing-next", "missing-revoked"} {
		t.Run(which, func(t *testing.T) {
			p := good
			switch which {
			case "not-revoked":
				p.Revoked = false
			case "delegated":
				p.Delegated = true
			case "future":
				p.ProducedAt = at.Add(time.Second)
			case "stale":
				p.NextUpdate = at
			case "missing-next":
				p.NextUpdate = time.Time{}
			case "missing-revoked":
				p.RevokedAt = time.Time{}
			}
			err := VerifyRevocation(context.Background(), proofHost{proof: p}, []byte("authenticated evidence"), []byte("leaf"), []byte("issuer"), at)
			if (err == nil) != (which == "good") {
				t.Fatalf("%s: %v", which, err)
			}
		})
	}
}
