package certs_test

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// trailingBytesChainPEM re-encodes a certificate with extra bytes after the
// outer ASN.1 SEQUENCE — the shape pki-core emits for ML-DSA certificates,
// which makes x509.ParseCertificate fail with "trailing data".
func trailingBytesChainPEM(t *testing.T, chainPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		t.Fatal("chainPEM is not valid PEM")
	}
	der := append(append([]byte{}, block.Bytes...), 0x00, 0x00)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestAppendChainToPool_TrailingBytes is the regression guard for the enrollment
// chain: a device provisioned by pki-core stores a chain whose certificates
// carry trailing bytes, AppendCertsFromPEM reports false for it, and every
// caller that read that false as fatal refused to build a TLS config at all.
func TestAppendChainToPool_TrailingBytes(t *testing.T) {
	_, cleanPEM := selfSignedCert(t, "tenant-device-ca", "")
	chainPEM := trailingBytesChainPEM(t, cleanPEM)

	// The premise: this is exactly what the standard helper cannot do.
	if x509.NewCertPool().AppendCertsFromPEM(chainPEM) {
		t.Fatal("AppendCertsFromPEM accepted a trailing-bytes chain; AppendChainToPool is no longer needed")
	}

	pool := x509.NewCertPool()
	if n := certs.AppendChainToPool(pool, string(chainPEM)); n != 1 {
		t.Fatalf("AppendChainToPool added %d certificates, want 1", n)
	}
	if len(pool.Subjects()) != 1 { //nolint:staticcheck // Subjects is the only way to inspect a pool's contents.
		t.Fatalf("pool holds %d subjects, want 1", len(pool.Subjects())) //nolint:staticcheck
	}
}

// A well-formed chain still lands in the pool, and an empty one is reported as
// zero rather than as an error — callers decide whether empty is fatal.
func TestAppendChainToPool_CleanAndEmpty(t *testing.T) {
	_, cleanPEM := selfSignedCert(t, "classical-ca", "")

	pool := x509.NewCertPool()
	if n := certs.AppendChainToPool(pool, string(cleanPEM)); n != 1 {
		t.Fatalf("AppendChainToPool(clean chain) = %d, want 1", n)
	}
	if n := certs.AppendChainToPool(x509.NewCertPool(), ""); n != 0 {
		t.Fatalf("AppendChainToPool(\"\") = %d, want 0", n)
	}
	if n := certs.AppendChainToPool(x509.NewCertPool(), "not-a-pem-chain"); n != 0 {
		t.Fatalf("AppendChainToPool(garbage) = %d, want 0", n)
	}
}
