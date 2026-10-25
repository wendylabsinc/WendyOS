//go:build go1.27

package liteenroll

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"google.golang.org/protobuf/proto"
)

func testAlgorithmTrustPEM(t *testing.T, pub crypto.PublicKey, signer crypto.Signer, signature x509.SignatureAlgorithm) []byte {
	t.Helper()
	c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test algorithm CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		SignatureAlgorithm: signature}
	der, err := x509.CreateCertificate(rand.Reader, c, c, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testMLDSATrustPEM(t *testing.T) []byte {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA87())
	if err != nil {
		t.Fatal(err)
	}
	return testAlgorithmTrustPEM(t, key.Public(), key, x509.MLDSA87)
}

func TestMbedTLSTrustAcceptsClassicalCAAlgorithms(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, signature := range []x509.SignatureAlgorithm{
		x509.SHA1WithRSA, x509.SHA256WithRSA, x509.SHA384WithRSA, x509.SHA512WithRSA,
		x509.SHA256WithRSAPSS, x509.SHA384WithRSAPSS, x509.SHA512WithRSAPSS,
	} {
		t.Run(signature.String(), func(t *testing.T) {
			if err := validateMbedTLSTrustBundle(testAlgorithmTrustPEM(t, rsaKey.Public(), rsaKey, signature)); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, curve := range []elliptic.Curve{elliptic.P224(), elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		for _, signature := range []x509.SignatureAlgorithm{x509.ECDSAWithSHA1, x509.ECDSAWithSHA256, x509.ECDSAWithSHA384, x509.ECDSAWithSHA512} {
			t.Run(curve.Params().Name+"/"+signature.String(), func(t *testing.T) {
				if err := validateMbedTLSTrustBundle(testAlgorithmTrustPEM(t, key.Public(), key, signature)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestMbedTLSTrustChecksBothPublicKeyAndIssuerSignature(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pq, err := mldsa.GenerateKey(mldsa.MLDSA87())
	if err != nil {
		t.Fatal(err)
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		pub    crypto.PublicKey
		signer crypto.Signer
	}{
		{"ML-DSA root", pq.Public(), pq},
		{"ML-DSA key signed by ECDSA", pq.Public(), ec},
		{"ECDSA key signed by ML-DSA", ec.Public(), pq},
		{"Ed25519 root", ed.Public(), ed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testAlgorithmTrustPEM(t, tc.pub, tc.signer, x509.UnknownSignatureAlgorithm)
			if err := validateTrustBundle(root, testInspector{}); err != nil {
				t.Fatalf("fixture must pass generic CA validation: %v", err)
			}
			err := validateMbedTLSTrustBundle(root)
			if err == nil || !strings.Contains(err.Error(), "PKI deployment needs an mbedTLS-compatible certificate chain") {
				t.Fatalf("missing actionable compatibility failure: %v", err)
			}
		})
	}
}

func TestProvisionTrustRejectsIncompatibleBundlesWithoutMutation(t *testing.T) {
	valid := testTrustPEM(t, true, time.Now().Add(time.Hour))
	incompatible := testMLDSATrustPEM(t)
	dir := t.TempDir()
	validPath, incompatiblePath := filepath.Join(dir, "classical.pem"), filepath.Join(dir, "mldsa.pem")
	for path, data := range map[string][]byte{validPath: valid, incompatiblePath: incompatible} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"device", "TSA", "HTTPS"} {
		t.Run(name, func(t *testing.T) {
			paths := []string{validPath, validPath, validPath}
			paths[i] = incompatiblePath
			cfg := &litepb.WendyConfEnrollment{Token: "do-not-print-test-credential", DeviceRoots: bytes.Clone(valid)}
			before := proto.Clone(cfg)
			client, err := ProvisionTrust(cfg, paths[0], paths[1], paths[2])
			if err == nil || client != nil || !strings.Contains(err.Error(), "ML-DSA") {
				t.Fatalf("expected incompatible bundle rejection: %v", err)
			}
			if strings.Contains(err.Error(), cfg.Token) {
				t.Fatal("compatibility error exposed credential")
			}
			if !proto.Equal(cfg, before) {
				t.Fatal("failed compatibility check changed configuration")
			}
		})
	}
}
