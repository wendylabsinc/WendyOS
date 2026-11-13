package cloudrelay

import (
	"bytes"
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResetSignatureUsesOnlyConfiguredHTTPSIssuer(t *testing.T) {
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	public := key.Public().(*mldsa.PublicKey).Bytes()
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/.well-known/wendy-cloud-grants/jwks.json" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("unsafe key request: %v", r)
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "AKP", "alg": "ML-DSA-65", "use": "sig", "kid": "cloud-key", "pub": base64.RawURLEncoding.EncodeToString(public)}}})
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	verifier := Verifier{Issuer: server.URL, HTTP: client}
	token := func(header map[string]any) string {
		raw, e := canonicalJSON(header)
		if e != nil {
			t.Fatal(e)
		}
		input := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"proof":"exact"}`))
		signature, e := key.Sign(rand.Reader, []byte(input), crypto.Hash(0))
		if e != nil {
			t.Fatal(e)
		}
		return input + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	valid := map[string]any{"alg": "ML-DSA-65", "kid": "cloud-key", "typ": "asset-reset-authorization+jws"}
	payload, resolved, err := verifier.VerifyResetSignature(context.Background(), token(valid))
	if err != nil || !bytes.Equal(resolved, public) || string(payload) != `{"proof":"exact"}` || calls.Load() != 1 {
		t.Fatal("configured issuer was not authenticated", err)
	}
	for _, field := range []string{"jku", "x5c", "jwk", "x5u"} {
		header := map[string]any{"alg": "ML-DSA-65", "kid": "cloud-key", "typ": "asset-reset-authorization+jws", field: "https://evil.example"}
		if _, _, err := verifier.VerifyResetSignature(context.Background(), token(header)); err == nil {
			t.Fatalf("caller-controlled %s accepted", field)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("caller key references triggered network access")
	}
	valid["typ"] = "tunnel-session-grant+jws"
	if _, _, err := verifier.VerifyResetSignature(context.Background(), token(valid)); err == nil {
		t.Fatal("cross-purpose proof accepted")
	}
	verifier.Issuer = "http://untrusted.example"
	if _, _, err := verifier.VerifyResetSignature(context.Background(), token(valid)); err == nil {
		t.Fatal("insecure issuer accepted")
	}

	// A kid the enrolled Cloud does not publish is refused, not guessed.
	valid["typ"] = "asset-reset-authorization+jws"
	valid["kid"] = "rotated-or-forged-key"
	fresh := Verifier{Issuer: server.URL, HTTP: client}
	if _, _, err := fresh.VerifyResetSignature(context.Background(), token(valid)); err == nil {
		t.Fatal("unpublished kid accepted")
	}

	// An unreachable JWKS fails closed; there is no cached or caller key fallback.
	valid["kid"] = "cloud-key"
	server.Close()
	unreachable := Verifier{Issuer: server.URL, HTTP: client}
	if _, _, err := unreachable.VerifyResetSignature(context.Background(), token(valid)); err == nil || !strings.Contains(err.Error(), "fetching Cloud grant keys") {
		t.Fatalf("unreachable JWKS did not fail closed: %v", err)
	}
}
