package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

type oidcJWK map[string]string

type oidcTokenVerifier struct {
	issuer string
	keys   []oidcJWK
}

type verifiedJWT struct {
	claims map[string]any
	alg    string
}

func loadOIDCTokenVerifier(ctx context.Context, meta *oidcProviderMetadata) (*oidcTokenVerifier, error) {
	if meta == nil || meta.Issuer == "" || meta.JWKSURI == "" {
		return nil, fmt.Errorf("OIDC metadata does not identify issuer signing keys")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.JWKSURI, nil)
	if err != nil {
		return nil, fmt.Errorf("building JWKS request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching issuer signing keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("issuer JWKS returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("reading issuer JWKS: %w", err)
	}
	if len(body) > 1<<20 {
		return nil, fmt.Errorf("issuer JWKS exceeds 1 MiB")
	}
	var document struct {
		Keys []oidcJWK `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil || len(document.Keys) == 0 {
		return nil, fmt.Errorf("issuer JWKS contains no usable keys")
	}
	return &oidcTokenVerifier{issuer: meta.Issuer, keys: document.Keys}, nil
}

func (v *oidcTokenVerifier) verify(raw, audience string) (*verifiedJWT, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("token is not a compact JWS")
	}
	decode := base64.RawURLEncoding.DecodeString
	headerBytes, err := decode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decoding token header: %w", err)
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("parsing token header: %w", err)
	}
	if header.Kid == "" || len(header.Crit) != 0 || (header.Alg != "ES256" && header.Alg != "ML-DSA-65") {
		return nil, fmt.Errorf("token uses an unsupported JOSE header")
	}
	signature, err := decode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decoding token signature: %w", err)
	}
	message := []byte(parts[0] + "." + parts[1])
	verified := false
	for _, key := range v.keys {
		if key["kid"] != header.Kid || key["alg"] != header.Alg {
			continue
		}
		switch header.Alg {
		case "ES256":
			verified = verifyES256JWK(key, message, signature)
		case "ML-DSA-65":
			verified = verifyMLDSA65JWK(key, message, signature)
		}
		if verified {
			break
		}
	}
	if !verified {
		return nil, fmt.Errorf("token signature verification failed")
	}
	payload, err := decode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decoding token payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parsing token claims: %w", err)
	}
	if issuer, _ := claims["iss"].(string); issuer != v.issuer {
		return nil, fmt.Errorf("token issuer %q does not match %q", issuer, v.issuer)
	}
	if !audienceContains(claims["aud"], audience) {
		return nil, fmt.Errorf("token audience does not include %s", audience)
	}
	now := float64(time.Now().Unix())
	expires, ok := claims["exp"].(float64)
	if !ok || expires <= now {
		return nil, fmt.Errorf("token is expired or carries no valid exp claim")
	}
	if notBefore, ok := claims["nbf"].(float64); ok && notBefore > now+60 {
		return nil, fmt.Errorf("token is not yet valid")
	}
	return &verifiedJWT{claims: claims, alg: header.Alg}, nil
}

func verifyES256JWK(key oidcJWK, message, signature []byte) bool {
	if key["kty"] != "EC" || key["crv"] != "P-256" || len(signature) != 64 {
		return false
	}
	xBytes, errX := base64.RawURLEncoding.DecodeString(key["x"])
	yBytes, errY := base64.RawURLEncoding.DecodeString(key["y"])
	if errX != nil || errY != nil {
		return false
	}
	x, y := new(big.Int).SetBytes(xBytes), new(big.Int).SetBytes(yBytes)
	if !elliptic.P256().IsOnCurve(x, y) {
		return false
	}
	digest := sha256.Sum256(message)
	return ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, digest[:],
		new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
}

func verifyMLDSA65JWK(key oidcJWK, message, signature []byte) bool {
	if key["kty"] != "AKP" {
		return false
	}
	publicBytes, err := base64.RawURLEncoding.DecodeString(key["pub"])
	if err != nil {
		return false
	}
	publicKey, err := mldsa.NewPublicKey(mldsa.MLDSA65(), publicBytes)
	return err == nil && mldsa.Verify(publicKey, message, signature, nil) == nil
}

func verifyIDToken(verifier *oidcTokenVerifier, token *oidcTokenResponse, clientID, nonce string) (string, error) {
	if token.IDToken == "" {
		return "", fmt.Errorf("token response contained no id_token")
	}
	verified, err := verifier.verify(token.IDToken, clientID)
	if err != nil {
		return "", fmt.Errorf("verifying ID token: %w", err)
	}
	gotNonce, _ := verified.claims["nonce"].(string)
	if nonce == "" || gotNonce != nonce {
		return "", fmt.Errorf("ID token nonce does not match the authorization request")
	}
	subject, _ := verified.claims["sub"].(string)
	if subject == "" {
		return "", fmt.Errorf("ID token carries no sub claim")
	}
	if atHash, _ := verified.claims["at_hash"].(string); atHash != "" {
		if verified.alg != "ES256" {
			return "", fmt.Errorf("ID token carries at_hash for unsupported algorithm %s", verified.alg)
		}
		digest := sha256.Sum256([]byte(token.AccessToken))
		want := base64.RawURLEncoding.EncodeToString(digest[:len(digest)/2])
		if atHash != want {
			return "", fmt.Errorf("ID token at_hash does not match the access token")
		}
	}
	return subject, nil
}

func verifyAccessToken(verifier *oidcTokenVerifier, raw, audience, thumbprint string) (map[string]any, error) {
	verified, err := verifier.verify(raw, audience)
	if err != nil {
		return nil, err
	}
	if got := confirmationThumbprint(verified.claims); got != thumbprint {
		return nil, fmt.Errorf("access token is not bound to the expected DPoP key")
	}
	if subject, _ := verified.claims["sub"].(string); subject == "" {
		return nil, fmt.Errorf("access token carries no sub claim")
	}
	return verified.claims, nil
}
