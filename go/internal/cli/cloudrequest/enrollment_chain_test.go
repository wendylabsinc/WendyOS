package cloudrequest

import (
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnrollmentRequestBindsCertificateChain(t *testing.T) {
	auth, key, _ := testAuth(t)
	for _, algorithm := range []string{"", EnrollmentChainP256} {
		t.Run(algorithm, func(t *testing.T) {
			jws, err := EnrollmentRequestForChain(auth, "lite-test", "C", algorithm)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(string(jws), ".")
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var claims map[string]any
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			got, present := claims["chain_algorithm"]
			if algorithm == "" && present || algorithm != "" && got != algorithm {
				t.Fatalf("signed chain_algorithm = %v, want %q", got, algorithm)
			}
			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatal(err)
			}
			if err := mldsa.Verify(key.PublicKey(), []byte(parts[0]+"."+parts[1]), sig, &mldsa.Options{}); err != nil {
				t.Fatal(err)
			}
			claims["chain_algorithm"] = "tampered"
			changed, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			if mldsa.Verify(key.PublicKey(), []byte(parts[0]+"."+base64.RawURLEncoding.EncodeToString(changed)), sig, &mldsa.Options{}) == nil {
				t.Fatal("certificate chain choice was not signed")
			}
		})
	}
	for _, tc := range []struct{ class, algorithm string }{
		{"A", EnrollmentChainP256}, {"B", EnrollmentChainP256}, {"C", "ML_DSA_65"}, {"C", "rsa"},
	} {
		if _, err := EnrollmentRequestForChain(auth, "lite-test", tc.class, tc.algorithm); err == nil {
			t.Fatalf("accepted class %s with chain %s", tc.class, tc.algorithm)
		}
	}
}
