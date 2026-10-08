package commands

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

func testES256Token(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func testES256JWK(key *ecdsa.PrivateKey, kid string) oidcJWK {
	return oidcJWK{
		"kid": kid, "alg": "ES256", "kty": "EC", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		"y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

func TestOIDCTokenVerifierValidatesSignatureAndClaims(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://auth.example/realms/acme"
	verifier := &oidcTokenVerifier{issuer: issuer, keys: []oidcJWK{testES256JWK(key, "key-1")}}
	claims := map[string]any{
		"iss": issuer, "aud": "wendy-cli", "sub": "user-1", "nonce": "nonce-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	accessClaims := map[string]any{
		"iss": issuer, "aud": "https://pki.example/identity", "sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]string{"jkt": "thumbprint"},
	}
	access := testES256Token(t, key, "key-1", accessClaims)
	digest := sha256.Sum256([]byte(access))
	claims["at_hash"] = base64.RawURLEncoding.EncodeToString(digest[:16])
	idToken := testES256Token(t, key, "key-1", claims)

	subject, err := verifyIDToken(verifier, &oidcTokenResponse{IDToken: idToken, AccessToken: access}, "wendy-cli", "nonce-1")
	if err != nil || subject != "user-1" {
		t.Fatalf("verifyIDToken = %q, %v", subject, err)
	}
	if _, err := verifyAccessToken(verifier, access, "https://pki.example/identity", "thumbprint"); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCTokenVerifierAcceptsMLDSA65(t *testing.T) {
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://auth.example/realms/acme"
	header, _ := json.Marshal(map[string]string{"alg": "ML-DSA-65", "kid": "pq"})
	payload, _ := json.Marshal(map[string]any{
		"iss": issuer, "aud": "wendy-cli", "sub": "user-1", "exp": time.Now().Add(time.Hour).Unix(),
	})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := key.Sign(rand.Reader, []byte(input), crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	token := input + "." + base64.RawURLEncoding.EncodeToString(signature)
	verifier := &oidcTokenVerifier{issuer: issuer, keys: []oidcJWK{{
		"kid": "pq", "alg": "ML-DSA-65", "kty": "AKP",
		"pub": base64.RawURLEncoding.EncodeToString(key.Public().(*mldsa.PublicKey).Bytes()),
	}}}
	if _, err := verifier.verify(token, "wendy-cli"); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCTokenVerifierRejectsTamperingAndNonceMismatch(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://auth.example/realms/acme"
	verifier := &oidcTokenVerifier{issuer: issuer, keys: []oidcJWK{testES256JWK(key, "key-1")}}
	token := testES256Token(t, key, "key-1", map[string]any{
		"iss": issuer, "aud": "wendy-cli", "sub": "user-1", "nonce": "wrong",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := verifyIDToken(verifier, &oidcTokenResponse{IDToken: token, AccessToken: "access"}, "wendy-cli", "expected"); err == nil {
		t.Fatal("nonce mismatch was accepted")
	}
	parts := []byte(token)
	parts[len(parts)-1] ^= 1
	if _, err := verifier.verify(string(parts), "wendy-cli"); err == nil {
		t.Fatal("tampered signature was accepted")
	}
}

func TestVerifyES256JWKRejectsInvalidPoint(t *testing.T) {
	key := oidcJWK{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(big.NewInt(1).Bytes()), "y": base64.RawURLEncoding.EncodeToString(big.NewInt(1).Bytes())}
	if verifyES256JWK(key, []byte("message"), make([]byte, 64)) {
		t.Fatal("invalid curve point was accepted")
	}
}
