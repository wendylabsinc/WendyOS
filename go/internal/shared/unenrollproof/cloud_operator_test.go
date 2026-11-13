package unenrollproof

import (
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestResetOperatorPreservesBothMLDSAProfiles(t *testing.T) {
	for _, profile := range []struct {
		name   string
		params mldsa.Parameters
	}{{"ML-DSA-65", mldsa.MLDSA65()}, {"ML-DSA-87", mldsa.MLDSA87()}} {
		t.Run(profile.name, func(t *testing.T) {
			key, err := mldsa.GenerateKey(profile.params)
			if err != nil {
				t.Fatal(err)
			}
			leaf := &x509.Certificate{PublicKey: key.Public(), Raw: []byte("validated-peer-leaf")}
			request := []byte{1, 2, 3}
			hash := sha256.Sum256(request)
			thumb := sha256.Sum256(leaf.Raw)
			at := time.Now()
			descriptor := map[string]any{"aud": "https://cloud.wendy.sh/broker", "body_sha256": base64.RawURLEncoding.EncodeToString(hash[:]), "correlation_id": "operator-test", "expiry": at.Unix() + 29, "iat": at.Unix() - 1, "nonce": base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "operation": "wendycloud.v2.AssetService/DeleteAsset", "target": map[string]any{"tenant": "tenant", "resource": "asset/asset"}}
			body, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			token := func(algorithm string) string {
				header, err := json.Marshal(map[string]string{"alg": algorithm, "kid": base64.RawURLEncoding.EncodeToString(thumb[:])})
				if err != nil {
					t.Fatal(err)
				}
				input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
				sig, err := key.Sign(rand.Reader, []byte(input), crypto.Hash(0))
				if err != nil {
					t.Fatal(err)
				}
				return input + "." + base64.RawURLEncoding.EncodeToString(sig)
			}
			if err := verifyOperatorReset(token(profile.name), request, "asset", "tenant", "operator-test", leaf, at, at); err != nil {
				t.Fatal(err)
			}
			for _, wrong := range []string{"ES256", "ML-DSA-44", "ML-DSA-65", "ML-DSA-87"} {
				if wrong != profile.name && verifyOperatorReset(token(wrong), request, "asset", "tenant", "operator-test", leaf, at, at) == nil {
					t.Fatalf("algorithm confusion accepted: %s", wrong)
				}
			}
		})
	}
}

// The operator stamps the DeleteAsset descriptor from its own clock, and the
// broker accepted it with iat within ±60 s of Cloud. The agent must accept the
// same descriptor rather than refuse the reset after Cloud already deleted
// the asset; the 30 s lifetime cap still holds (WDY-3939 #11).
func TestResetOperatorClockSkew(t *testing.T) {
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{PublicKey: key.Public(), Raw: []byte("validated-peer-leaf")}
	request := []byte{1, 2, 3}
	hash := sha256.Sum256(request)
	thumb := sha256.Sum256(leaf.Raw)
	latest := time.Now()
	token := func(iat, expiry int64) string {
		body, err := json.Marshal(map[string]any{"aud": "https://cloud.wendy.sh/broker", "body_sha256": base64.RawURLEncoding.EncodeToString(hash[:]), "correlation_id": "operator-test", "expiry": expiry, "iat": iat, "nonce": base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "operation": "wendycloud.v2.AssetService/DeleteAsset", "target": map[string]any{"tenant": "tenant", "resource": "asset/asset"}})
		if err != nil {
			t.Fatal(err)
		}
		header, err := json.Marshal(map[string]string{"alg": "ML-DSA-65", "kid": base64.RawURLEncoding.EncodeToString(thumb[:])})
		if err != nil {
			t.Fatal(err)
		}
		input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		sig, err := key.Sign(rand.Reader, []byte(input), crypto.Hash(0))
		if err != nil {
			t.Fatal(err)
		}
		return input + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	now := latest.Unix()
	for _, c := range []struct {
		name        string
		iat, expiry int64
		ok          bool
	}{
		{"operator 10s fast", now + 10, now + 40, true},
		{"operator 60s fast", now + 60, now + 90, true},
		{"operator 61s fast", now + 61, now + 91, false},
		{"lifetime over 30s", now + 10, now + 41, false},
		{"expired", now - 30, now, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := verifyOperatorReset(token(c.iat, c.expiry), request, "asset", "tenant", "operator-test", leaf, latest, latest)
			if c.ok && err != nil {
				t.Fatalf("want accept, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("want reject, got accept")
			}
		})
	}
}
