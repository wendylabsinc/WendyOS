package cloudrequest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const testMethod = "/wendy.cloud.v2.OrganizationService/ListOrganizations"

func boundSession(t *testing.T) (*config.AuthConfig, DPoPTokenProvider, *mldsa.PublicKey, string) {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatalf("GenerateMLDSAKeyPair: %v", err)
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatalf("ParseSigningPrivateKeyPEM: %v", err)
	}
	pub, ok := signer.Public().(*mldsa.PublicKey)
	if !ok {
		t.Fatalf("public key is %T, want *mldsa.PublicKey", signer.Public())
	}
	const token = "header.payload.access-token"
	auth := &config.AuthConfig{
		CloudGRPC:      "api.dev.wendy.sh:443",
		OAuthIssuer:    "https://auth.dev.wendy.sh/realms/acme",
		DPoPPrivateKey: keyPEM,
		APIKey:         token,
	}
	provider := func(context.Context) (string, crypto.Signer, error) { return token, signer, nil }
	return auth, provider, pub, token
}

func headersFromCtx(t *testing.T, ctx context.Context) (authz, proof string) {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("interceptor set no outgoing metadata")
	}
	az, dp := md["authorization"], md["dpop"]
	if len(az) != 1 || len(dp) != 1 {
		t.Fatalf("want one authorization and one dpop header, got authz=%v dpop=%v", az, dp)
	}
	return az[0], dp[0]
}

func runUnary(t *testing.T, auth *config.AuthConfig, p DPoPTokenProvider) (authz, proof string) {
	t.Helper()
	var got context.Context
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		got = ctx
		return nil
	}
	if err := dpopUnaryInterceptor(auth, p)(context.Background(), testMethod, nil, nil, nil, invoker); err != nil {
		t.Fatalf("unary interceptor: %v", err)
	}
	return headersFromCtx(t, got)
}

func runStream(t *testing.T, auth *config.AuthConfig, p DPoPTokenProvider) (authz, proof string) {
	t.Helper()
	var got context.Context
	streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		got = ctx
		return nil, nil
	}
	if _, err := dpopStreamInterceptor(auth, p)(context.Background(), &grpc.StreamDesc{}, nil, testMethod, streamer); err != nil {
		t.Fatalf("stream interceptor: %v", err)
	}
	return headersFromCtx(t, got)
}

func verifyProof(t *testing.T, proof, token string, pub *mldsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("proof has %d segments, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	if err := mldsa.Verify(pub, []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
		t.Fatalf("proof signature does not verify against the session key: %v", err)
	}
	var header struct {
		Typ string            `json:"typ"`
		JWK map[string]string `json:"jwk"`
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(hdr, &header); err != nil {
		t.Fatalf("parsing header: %v", err)
	}
	if header.Typ != "dpop+jwt" {
		t.Errorf("typ = %q, want dpop+jwt", header.Typ)
	}
	if header.JWK["kty"] != "AKP" || header.JWK["alg"] != "ML-DSA-65" {
		t.Errorf("jwk = %v, want an RFC 9964 AKP ML-DSA-65 key", header.JWK)
	}
	var payload map[string]any
	pl, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(pl, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	sum := sha256.Sum256([]byte(token))
	if payload["ath"] != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Errorf("ath = %v, want base64url(sha256(token))", payload["ath"])
	}
	if payload["htm"] != "POST" {
		t.Errorf("htm = %v, want POST", payload["htm"])
	}
	return payload
}

func TestDPoPUnaryProofNotBearer(t *testing.T) {
	auth, p, pub, token := boundSession(t)
	authz, proof := runUnary(t, auth, p)
	if authz != "DPoP "+token {
		t.Fatalf("authorization = %q, want DPoP (Bearer must never be emitted for a bound token)", authz)
	}
	payload := verifyProof(t, proof, token, pub)
	if got := payload["htu"]; got != "https://"+auth.CloudGRPC+testMethod {
		t.Errorf("htu = %v, want the invoked method path %q", got, "https://"+auth.CloudGRPC+testMethod)
	}
}

func TestDPoPStreamProofPerRPC(t *testing.T) {
	auth, p, pub, token := boundSession(t)
	authz, proof := runStream(t, auth, p)
	if authz != "DPoP "+token {
		t.Fatalf("stream authorization = %q, want DPoP", authz)
	}
	if got := verifyProof(t, proof, token, pub)["htu"]; got != "https://"+auth.CloudGRPC+testMethod {
		t.Errorf("stream htu = %v, want %q", got, "https://"+auth.CloudGRPC+testMethod)
	}
}

func TestDPoPProofFreshPerCall(t *testing.T) {
	auth, p, _, _ := boundSession(t)
	_, p1 := runUnary(t, auth, p)
	_, p2 := runUnary(t, auth, p)
	jti := func(proof string) any {
		var payload map[string]any
		pl, _ := base64.RawURLEncoding.DecodeString(strings.Split(proof, ".")[1])
		_ = json.Unmarshal(pl, &payload)
		return payload["jti"]
	}
	if jti(p1) == jti(p2) {
		t.Fatal("jti repeated across calls; proof is not fresh per RPC")
	}
}

func TestDPoPDialOptionsNilForUnbound(t *testing.T) {
	_, p, _, _ := boundSession(t)
	if opts := DPoPDialOptions(&config.AuthConfig{CloudGRPC: "self:443", APIKey: "k"}, p); opts != nil {
		t.Fatalf("unbound session got %d dial options, want nil", len(opts))
	}
	bound, p2, _, _ := boundSession(t)
	if opts := DPoPDialOptions(bound, p2); len(opts) != 2 {
		t.Fatalf("bound session got %d dial options, want 2 (unary+stream)", len(opts))
	}
	if DPoPDialOptions(bound, nil) != nil {
		t.Fatal("nil provider must yield nil dial options")
	}
}

func TestDPoPInvalidKeyKeepsExistingProofErrorContext(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := &config.AuthConfig{CloudGRPC: "cloud.example:443"}
	provider := func(context.Context) (string, crypto.Signer, error) { return "access-token", key, nil }
	_, err = dpopAuthContext(context.Background(), auth, provider, testMethod)
	const want = "DPoP: building proof: this session's operator key is *ecdsa.PublicKey, but Wendy now requires an ML-DSA-65 operator credential; re-run 'wendy auth login'"
	if err == nil || err.Error() != want {
		t.Fatalf("proof diagnostic changed: %v", err)
	}
}
