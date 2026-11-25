package upstreamcerts

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
)

func TestRealMLDSAFallbackAndNativePathsAgree(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := testidentity.New(t, now)
	for _, fallback := range []bool{false, true} {
		for _, profile := range []struct {
			name      string
			usage     []x509.ExtKeyUsage
			ca, valid bool
		}{
			{"clientAuth", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false, true},
			{"both", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, false, true},
			{"missing", nil, false, false},
			{"any", []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, false, false},
			{"server", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, false, false},
			{"CA", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, true, false},
		} {
			leaf := *f.Leaf
			leaf.ExtKeyUsage = profile.usage
			leaf.IsCA = profile.ca
			// Retain actual ML-DSA DER/signature, but make the standard verifier
			// unable to verify it, as on a toolchain without native recognition.
			if fallback {
				leaf.SignatureAlgorithm = x509.UnknownSignatureAlgorithm
			}
			until, err := VerifyPeerInterval(&leaf, []*x509.Certificate{f.Issuer}, []*x509.Certificate{f.Root}, now, now)
			if (err == nil) != profile.valid {
				t.Fatalf("fallback=%v profile=%s: %v", fallback, profile.name, err)
			}
			if err == nil && !until.Equal(f.NotAfter) {
				t.Fatal("fallback path expiry differs")
			}
		}
	}
}
