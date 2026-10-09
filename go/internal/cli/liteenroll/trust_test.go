package liteenroll

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func testTrustPEM(t *testing.T, ca bool, expiry time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry, IsCA: ca, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestProvisionTrust(t *testing.T) {
	valid := testTrustPEM(t, true, time.Now().Add(time.Hour))
	path := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &litepb.WendyConfEnrollment{}
	client, err := ProvisionTrust(cfg, path, path, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if !cfg.ProvisionTrust || !bytes.Equal(cfg.DeviceRoots, valid) || !bytes.Equal(cfg.TsaRoots, valid) || !bytes.Equal(cfg.HttpsRoots, valid) {
		t.Fatal("bundles not provisioned")
	}
	transport := client.Transport.(*http.Transport)
	if transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("HTTPS verification disabled")
	}
	if err := CheckTrustSupport(cfg, &litepb.WendyComEnrollmentChallenge{}); err == nil {
		t.Fatal("old firmware accepted")
	}
	if err := CheckTrustSupport(cfg, &litepb.WendyComEnrollmentChallenge{UsbTrustSupported: true}); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][3]string{{path, "", path}, {"", path, path}, {path, path, ""}, {path, path, path + "-missing"}} {
		cfg := &litepb.WendyConfEnrollment{}
		if _, err := ProvisionTrust(cfg, paths[0], paths[1], paths[2]); err == nil {
			t.Fatal("incomplete trust accepted")
		}
		if cfg.ProvisionTrust || len(cfg.DeviceRoots) != 0 {
			t.Fatal("partial trust mutation")
		}
	}
	legacy := &litepb.WendyConfEnrollment{}
	if _, err := ProvisionTrust(legacy, "", "", ""); err != nil || legacy.ProvisionTrust {
		t.Fatal("legacy configuration changed", err)
	}
	if err := CheckTrustSupport(legacy, &litepb.WendyComEnrollmentChallenge{BuiltinRootsReady: true, BuiltinTsaRootsReady: true}); err != nil {
		t.Fatal(err)
	}
}

func TestTrustBundleRejectsInvalidInput(t *testing.T) {
	valid := testTrustPEM(t, true, time.Now().Add(time.Hour))
	cases := map[string][]byte{
		"empty": nil, "whitespace": []byte(" \n"), "oversize": bytes.Repeat([]byte("x"), maxTrustBundle+1),
		"leaf":      testTrustPEM(t, false, time.Now().Add(time.Hour)),
		"expired":   testTrustPEM(t, true, time.Now().Add(-time.Minute)),
		"trailing":  append(bytes.Clone(valid), []byte("garbage")...),
		"leading":   append([]byte("garbage"), valid...),
		"nul":       append(bytes.Clone(valid), 0),
		"too many":  bytes.Repeat(valid, 9),
		"malformed": []byte("-----BEGIN CERTIFICATE-----\nbad\n-----END CERTIFICATE-----"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateTrustBundle(data); err == nil {
				t.Fatal("accepted invalid CA bundle")
			}
		})
	}
	if err := validateTrustBundle(append(bytes.Clone(valid), valid...)); err != nil {
		t.Fatal(err)
	}
}

func TestTrustPreflightRequiresConfirmedRoots(t *testing.T) {
	for _, tc := range []struct {
		name               string
		mode               string
		roots, tsa, wantOK bool
	}{
		{"empty", "roughtime", false, false, false},
		{"roughtime", "roughtime", true, false, true},
		{"tsa missing", "https://time.example", true, false, false},
		{"legacy complete", "https://time.example", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckTrustSupport(&litepb.WendyConfEnrollment{TimeUrl: tc.mode}, &litepb.WendyComEnrollmentChallenge{BuiltinRootsReady: tc.roots, BuiltinTsaRootsReady: tc.tsa})
			if (err == nil) != tc.wantOK {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
