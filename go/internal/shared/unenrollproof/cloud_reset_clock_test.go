package unenrollproof

import (
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func signJWS(t *testing.T, key *mldsa.PrivateKey, header, body any) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	sig, err := key.Sign(rand.Reader, []byte(input), crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Cloud stamps the reset authorization from its own clock; the CLI checks it
// against an unauthenticated workstation clock (prod unenroll, 2026-11-14).
func TestCloudResetClockSkew(t *testing.T) {
	const tenant, device, asset = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device
	fingerprint := strings.Repeat("ab", 32)
	now := time.Now()

	opKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	opURI, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/operator/op")
	operator := &x509.Certificate{PublicKey: opKey.Public(), Raw: []byte("operator-leaf"), URIs: []*url.URL{opURI}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	// Deleted by a Cloud clock 8 s ahead, a second before it signed.
	deletion, _ := proto.Marshal(&cloudpb.DeletedAsset{Id: asset, OrganizationId: tenant, DeviceId: device, DeletedAt: timestamppb.New(now.Add(7 * time.Second))})
	request, _ := proto.Marshal(&cloudpb.DeleteAssetRequest{Id: asset, ExpectedDeviceId: proto.String(device), ResetCertificateSha256: proto.String(fingerprint)})
	digest, thumb := sha256.Sum256(request), sha256.Sum256(operator.Raw)
	opJWS := signJWS(t, opKey, map[string]string{"alg": "ML-DSA-65", "kid": base64.RawURLEncoding.EncodeToString(thumb[:])}, map[string]any{
		"aud": "https://cloud.wendy.sh/broker", "body_sha256": base64.RawURLEncoding.EncodeToString(digest[:]), "correlation_id": "clock-test",
		"expiry": now.Unix() + 29, "iat": now.Unix() - 1, "nonce": base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "operation": "wendycloud.v2.AssetService/DeleteAsset",
		"target": map[string]any{"resource": "asset/" + asset, "tenant": tenant},
	})
	cloudKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	public := cloudKey.Public().(*mldsa.PublicKey).Bytes()
	token := func(aud string, iat int64) []byte {
		return []byte(signJWS(t, cloudKey, map[string]string{"alg": "ML-DSA-65", "kid": "cloud", "typ": "asset-reset-authorization+jws"}, map[string]any{
			"iss": "https://api.example", "aud": aud, "iat": iat, "exp": iat + 30, "jti": uuid.NewString(), "correlation_id": "clock-test",
			"deletion": deletion, "request_body": request, "request_jws": opJWS,
		}))
	}
	cli := func(tok []byte) error {
		return verifySavedCloudReset(tok, public, "api.example:443", principal, fingerprint, deletion, operator, now, now, cliClockSkew)
	}
	const aud = "wendy-agent/unprovision"
	n := now.Unix()

	for name, iat := range map[string]int64{"cloud 8s ahead": n + 8, "cloud 35s behind": n - 35} {
		if err := cli(token(aud, iat)); err != nil {
			t.Fatalf("%s: skew inside the margin refused: %v", name, err)
		}
	}
	var skew *ClockSkewError
	if err := cli(token(aud, n+15)); !errors.As(err, &skew) || skew.Offset != 15*time.Second || !strings.Contains(err.Error(), "about 15s behind Cloud's") {
		t.Fatalf("cloud 15s ahead: want clock message, got %v", err)
	}
	if err := cli(token(aud, n-41)); !errors.As(err, &skew) {
		t.Fatalf("expired 11s ago: want clock refusal, got %v", err)
	}
	if err := cli(token("wendy-agent/other", n)); err == nil || errors.As(err, &skew) || !strings.Contains(err.Error(), "not a device reset authorization") {
		t.Fatalf("wrong audience: want its own message, got %v", err)
	}
	// The Agent checks against authenticated time and gets no tolerance.
	if err := VerifySavedCloudReset(token(aud, n+8), public, "api.example:443", principal, fingerprint, deletion, operator, now, now); !errors.As(err, &skew) {
		t.Fatalf("zero-tolerance path accepted a future iat: %v", err)
	}
}
