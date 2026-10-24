package grpcclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// testCertInfo builds a self-contained ECDSA CA + client leaf and returns the
// CertificateInfo shape ConnectWithTLSAndPins consumes.
func testCertInfo(t *testing.T) *config.CertificateInfo {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "TLS Config Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "tls-config-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return &config.CertificateInfo{
		PemCertificate:      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})),
		PemPrivateKey:       string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		PemCertificateChain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
	}
}

func TestSDKHostSessionCache(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			store := "off"
			if enabled {
				store = "file"
			}
			t.Setenv("WENDY_TLS_SESSION_STORE", store)
			cert := testCertInfo(t)
			connector := sdkDeviceConnector("192.168.1.10:50052", cert, nil, nil)
			credential, err := connector.Credential(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			cache := connector.SessionCache("192.168.1.10:50052", credential.Certificate.Certificate[0])
			if (cache != nil) != enabled {
				t.Fatalf("cache=%v enabled=%v", cache, enabled)
			}
		})
	}
}
func TestSDKHostDebugDiagnostics(t *testing.T) {
	t.Setenv("WENDY_TLS_DEBUG", "1")
	var buf bytes.Buffer
	old := tlsDebugWriter
	tlsDebugWriter = &buf
	t.Cleanup(func() { tlsDebugWriter = old })
	connector := sdkDeviceConnector("passthrough:///device", nil, nil, nil)
	connector.Debugf("[tls-debug] %s resumed=%v\n", "device:443", true)
	if got := buf.String(); got != "[tls-debug] passthrough:///device resumed=true\n" {
		t.Fatal(got)
	}
}
