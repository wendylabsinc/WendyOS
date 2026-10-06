package commands

// Unit coverage for the OIDC primitives. The parts most worth pinning are the
// ones whose failures are silent and intermittent: a thumbprint that is wrong
// only when a coordinate has a leading zero byte, or a signature encoded as
// ASN.1 instead of raw R||S.

import (
	"context"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// testKey mints an operator credential. ML-DSA-65 since WDY-3032 — the DPoP
// and token paths refuse anything else, so an EC key here would only test a
// rejection path.
func testKey(t *testing.T) *mldsa.PrivateKey {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return key
}

// RFC 7638 §3.1 worked example. If this drifts, every cnf.jkt comparison
// against the server silently stops matching.
func TestJWKThumbprintMatchesRFC7638Construction(t *testing.T) {
	key := testKey(t)
	got, err := operatorJWKThumbprint(key)
	if err != nil {
		t.Fatalf("operatorJWKThumbprint: %v", err)
	}

	// Recompute independently: canonical JSON with members in lexical order,
	// SHA-256, base64url without padding. RFC 9964 puts "alg" in the AKP
	// thumbprint input, unlike EC.
	jwk, _, err := operatorPublicJWK(key)
	if err != nil {
		t.Fatalf("operatorPublicJWK: %v", err)
	}
	canonical, err := json.Marshal(map[string]string{
		"alg": jwk["alg"], "kty": jwk["kty"], "pub": jwk["pub"],
	})
	if err != nil {
		t.Fatalf("marshaling canonical JWK: %v", err)
	}
	sum := sha256.Sum256(canonical)
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	if got != want {
		t.Fatalf("thumbprint mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestNewDPoPProofStructure(t *testing.T) {
	key := testKey(t)
	proof, err := newDPoPProof(key, "POST", "https://auth.example/token", "")
	if err != nil {
		t.Fatalf("newDPoPProof: %v", err)
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("proof has %d segments, want 3", len(parts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	var header struct {
		Typ string            `json:"typ"`
		Alg string            `json:"alg"`
		JWK map[string]string `json:"jwk"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("parsing header: %v", err)
	}
	if header.Typ != "dpop+jwt" {
		t.Errorf("typ = %q, want dpop+jwt", header.Typ)
	}
	if header.Alg != "ML-DSA-65" {
		t.Errorf("alg = %q, want ML-DSA-65", header.Alg)
	}
	// The public key must travel in the header — that is what lets the server
	// compute cnf.jkt.
	if header.JWK["kty"] != "AKP" || header.JWK["alg"] != "ML-DSA-65" {
		t.Errorf("jwk = %v, want an RFC 9964 AKP ML-DSA-65 key", header.JWK)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	for _, claim := range []string{"jti", "htm", "htu", "iat"} {
		if _, ok := payload[claim]; !ok {
			t.Errorf("payload missing %q", claim)
		}
	}
	if payload["htm"] != "POST" {
		t.Errorf("htm = %v, want POST", payload["htm"])
	}
	if _, ok := payload["nonce"]; ok {
		t.Error("nonce present when none was supplied")
	}
	if _, ok := payload["ath"]; ok {
		t.Error("ath present on a token-endpoint proof")
	}
}

func TestNewDPoPProofIncludesNonceWhenGiven(t *testing.T) {
	key := testKey(t)
	proof, err := newDPoPProof(key, "POST", "https://auth.example/token", "abc123")
	if err != nil {
		t.Fatalf("newDPoPProof: %v", err)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(strings.Split(proof, ".")[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	if payload["nonce"] != "abc123" {
		t.Fatalf("nonce = %v, want abc123", payload["nonce"])
	}
}

// RFC 9449 excludes query and fragment from htu.
func TestCanonicalHTUStripsQueryAndFragment(t *testing.T) {
	got, err := canonicalHTU("https://auth.example/realms/x/oauth2/token?a=1#frag")
	if err != nil {
		t.Fatalf("canonicalHTU: %v", err)
	}
	if want := "https://auth.example/realms/x/oauth2/token"; got != want {
		t.Fatalf("htu = %q, want %q", got, want)
	}
}

func TestPKCEVerifierProducesS256Challenge(t *testing.T) {
	verifier, challenge, err := newPKCEVerifier()
	if err != nil {
		t.Fatalf("newPKCEVerifier: %v", err)
	}
	// RFC 7636 §4.1: 43–128 characters.
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length %d outside RFC 7636 range", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Fatalf("challenge = %q, want %q", challenge, want)
	}
	if strings.ContainsAny(challenge, "+/=") {
		t.Fatalf("challenge %q is not base64url without padding", challenge)
	}
}

func TestOIDCCallbackUsesStaticPort8765(t *testing.T) {
	if oidcCallbackAddr != "127.0.0.1:8765" {
		t.Fatalf("callback address = %q, want 127.0.0.1:8765", oidcCallbackAddr)
	}
	if oidcRedirectURI != "http://127.0.0.1:8765/callback" {
		t.Fatalf("redirect URI = %q, want http://127.0.0.1:8765/callback", oidcRedirectURI)
	}
}

// aud may be a bare string or an array (RFC 7519 §4.1.3).
func TestAudienceContains(t *testing.T) {
	const want = defaultDevCloudResource
	cases := []struct {
		name string
		aud  any
		ok   bool
	}{
		{"bare string match", want, true},
		{"bare string mismatch", "https://cloud.wendy.sh", false},
		{"array containing", []any{"https://cloud.wendy.sh", want}, true},
		{"array without", []any{"https://cloud.wendy.sh"}, false},
		{"absent", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := audienceContains(tc.aud, want); got != tc.ok {
				t.Fatalf("audienceContains(%v) = %v, want %v", tc.aud, got, tc.ok)
			}
		})
	}
}

func TestConfirmationThumbprint(t *testing.T) {
	if got := confirmationThumbprint(map[string]any{
		"cnf": map[string]any{"jkt": "abc"},
	}); got != "abc" {
		t.Fatalf("jkt = %q, want abc", got)
	}
	// A token with no cnf must yield "", which the login flow treats as a
	// non-DPoP-bound client rather than a match.
	if got := confirmationThumbprint(map[string]any{"sub": "x"}); got != "" {
		t.Fatalf("jkt = %q, want empty", got)
	}
}

func TestDecodeJWTClaimsRejectsMalformed(t *testing.T) {
	if _, err := decodeJWTClaims("not-a-jwt"); err == nil {
		t.Fatal("expected an error for a non-JWS string")
	}
}

// Regression: wendy-auth parses the authorize query strictly, so a space
// encoded as "+" (what url.Values.Encode produces) arrives as a literal plus
// and the request is rejected with invalid_scope. Verified against a live
// realm: "openid+email" fails, "openid%20email" succeeds.
func TestBuildAuthorizeURLPercentEncodesScopeSpaces(t *testing.T) {
	meta := &oidcProviderMetadata{AuthorizationEndpoint: "https://auth.example/realms/x/authorize"}
	got, err := buildAuthorizeURL(meta, "wendy-cli", "http://127.0.0.1:8765/callback", "chal", "state", "nonce", defaultDevCloudResource)
	if err != nil {
		t.Fatalf("buildAuthorizeURL: %v", err)
	}
	if strings.Contains(got, "+") {
		t.Fatalf("query contains a raw '+', which the server reads literally:\n%s", got)
	}
	if !strings.Contains(got, "scope=openid%20email%20profile%20groups") {
		t.Fatalf("scope not percent-encoded with %%20:\n%s", got)
	}
	// The parsed form must still round-trip to real spaces.
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing built URL: %v", err)
	}
	if s := u.Query().Get("scope"); s != oidcScopes {
		t.Fatalf("scope round-trip = %q, want %q", s, oidcScopes)
	}
	if got := u.Query().Get("nonce"); got != "nonce" {
		t.Fatalf("nonce = %q, want nonce", got)
	}
}

func TestDiscoverOIDCIssuerUsesHomeRealm(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/login/realm" || r.Method != http.MethodPost {
			t.Fatalf("unexpected discovery request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"loginURL":"/realms/acme/account/login?email=user%40acme.test"}`))
	}))
	defer server.Close()

	issuer, err := discoverOIDCIssuer(context.Background(), server.URL, "user@acme.test")
	if err != nil {
		t.Fatalf("discoverOIDCIssuer: %v", err)
	}
	if want := server.URL + "/realms/acme"; issuer != want {
		t.Fatalf("issuer = %q, want %q", issuer, want)
	}
}

func TestRefreshOIDCTokenCarriesResourceAndDPoP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "refresh-1" {
			t.Errorf("refresh_token = %q", got)
		}
		if got := r.Form.Get("resource"); got != defaultDevCloudResource {
			t.Errorf("resource = %q", got)
		}
		if r.Header.Get("DPoP") == "" {
			t.Error("missing DPoP proof")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-2","token_type":"DPoP","expires_in":3600,"refresh_token":"refresh-2"}`))
	}))
	defer server.Close()

	token, err := refreshOIDCToken(
		context.Background(), testKey(t),
		&oidcProviderMetadata{TokenEndpoint: server.URL},
		"wendy-cli", "refresh-1", defaultDevCloudResource,
	)
	if err != nil {
		t.Fatalf("refreshOIDCToken: %v", err)
	}
	if token.AccessToken != "access-2" || token.RefreshToken != "refresh-2" {
		t.Fatalf("unexpected token response: %+v", token)
	}
}
