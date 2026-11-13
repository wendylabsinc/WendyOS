package services

import (
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
)

type resetActorKey struct{}

func fixtureJWS(t *testing.T, key *mldsa.PrivateKey, header, body any) []byte {
	t.Helper()
	h, e := json.Marshal(header)
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(body)
	if e != nil {
		t.Fatal(e)
	}
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	sig, e := key.Sign(rand.Reader, []byte(input), crypto.Hash(0))
	if e != nil {
		t.Fatal(e)
	}
	return []byte(input + "." + base64.RawURLEncoding.EncodeToString(sig))
}

// This fixture replaces NETWORK only. Both Cloud and operator signatures and
// all bindings still go through the production cryptographic verifier, twice.
func renewFixtureAuthorization(t *testing.T, svc *ProvisioningService, req *agentpbv2.UnprovisionRequest, ctx context.Context, now time.Time) *mldsa.PrivateKey {
	t.Helper()
	key := ctx.Value(resetActorKey{}).(*mldsa.PrivateKey)
	p, _ := peer.FromContext(ctx)
	actor := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
	var deletion cloudpbv2.DeletedAsset
	if e := proto.Unmarshal(req.CloudDeletion, &deletion); e != nil {
		t.Fatal(e)
	}
	target := deletion.GetDeviceId()
	fingerprint := req.ExpectedCertificateSha256
	body, e := proto.Marshal(&cloudpbv2.DeleteAssetRequest{Id: deletion.Id, ExpectedDeviceId: &target, ResetCertificateSha256: &fingerprint})
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(body)
	thumb := sha256.Sum256(actor.Raw)
	issued := now.Unix() - 1
	nonce := make([]byte, 32)
	rand.Read(nonce)
	op := fixtureJWS(t, key, map[string]any{"alg": "ML-DSA-65", "kid": base64.RawURLEncoding.EncodeToString(thumb[:])}, map[string]any{
		"aud": "https://cloud.wendy.sh/broker", "body_sha256": base64.RawURLEncoding.EncodeToString(digest[:]), "correlation_id": "reset-test",
		"expiry": issued + 30, "iat": issued, "nonce": base64.RawURLEncoding.EncodeToString(nonce), "operation": "wendycloud.v2.AssetService/DeleteAsset",
		"target": map[string]any{"resource": "asset/" + deletion.Id, "tenant": deletion.OrganizationId},
	})
	cloud, e := mldsa.GenerateKey(mldsa.MLDSA65())
	if e != nil {
		t.Fatal(e)
	}
	req.CloudResetAuthorization = fixtureJWS(t, cloud, map[string]any{"alg": "ML-DSA-65", "kid": "trusted-cloud-key", "typ": "asset-reset-authorization+jws"}, map[string]any{
		"iss": "https://api.example", "aud": "wendy-agent/unprovision", "iat": issued, "exp": issued + 30, "jti": uuid.NewString(), "correlation_id": "reset-test",
		"deletion": req.CloudDeletion, "request_body": body, "request_jws": string(op),
	})
	public := cloud.Public().(*mldsa.PublicKey).Bytes()
	svc.verifyCloudReset = func(_ context.Context, token []byte, host, principal, fp string, deletion []byte, operator *x509.Certificate, earliest, latest time.Time) ([]byte, error) {
		err := unenrollproof.VerifySavedCloudReset(token, public, host, principal, fp, deletion, operator, earliest, latest)
		return public, err
	}
	return cloud
}

func TestCloudResetMissingOrForgedAuthorityRetainsKeys(t *testing.T) {
	for _, name := range []string{"missing", "truncated", "signature", "caller-deletion", "caller-fingerprint", "wrong-cloud"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			unchanged := checkResetStateUnchanged(t, svc)
			switch name {
			case "missing":
				req.CloudResetAuthorization = nil
			case "truncated":
				req.CloudResetAuthorization = []byte("a.b.c")
			case "signature":
				req.CloudResetAuthorization[len(req.CloudResetAuthorization)-20] ^= 1
			case "caller-deletion":
				req.CloudDeletion = append(req.CloudDeletion, 0)
			case "caller-fingerprint":
				req.ExpectedCertificateSha256 = "00"
			case "wrong-cloud":
				svc.cloudHost = "evil.example:443"
			}
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); err == nil {
				t.Fatal("unauthenticated reset accepted")
			}
			unchanged()
		})
	}
}
