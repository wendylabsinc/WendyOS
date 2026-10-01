package grpcclient

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
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
func TestNewAgentTLSConfigSetsSessionCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_TLS_SESSION_STORE", "file")
	cfg, err := newAgentTLSConfig("192.168.1.10:50052", testCertInfo(t), nil, new(atomic.Int32), new(atomic.Pointer[certs.WendyIdentity]), nil, nil, nil)
	if err != nil {
		t.Fatalf("newAgentTLSConfig: %v", err)
	}
	if cfg.ClientSessionCache == nil {
		t.Error("ClientSessionCache not set")
	}
}

func TestNewAgentTLSConfigHonorsStoreOff(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	cfg, err := newAgentTLSConfig("192.168.1.10:50052", testCertInfo(t), nil, new(atomic.Int32), new(atomic.Pointer[certs.WendyIdentity]), nil, nil, nil)
	if err != nil {
		t.Fatalf("newAgentTLSConfig: %v", err)
	}
	if cfg.ClientSessionCache != nil {
		t.Errorf("ClientSessionCache = %v, want nil with store=off", cfg.ClientSessionCache)
	}
}

func TestNewAgentTLSConfigDebugLogsResumption(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	t.Setenv("WENDY_TLS_DEBUG", "1")
	var buf bytes.Buffer
	origWriter := tlsDebugWriter
	tlsDebugWriter = &buf
	defer func() { tlsDebugWriter = origWriter }()

	cfg, err := newAgentTLSConfig("192.168.1.10:50052", testCertInfo(t), nil, new(atomic.Int32), new(atomic.Pointer[certs.WendyIdentity]), nil, nil, nil)
	if err != nil {
		t.Fatalf("newAgentTLSConfig: %v", err)
	}
	// The wrapped VerifyConnection must log DidResume before delegating; an
	// empty ConnectionState fails the inner verifier, which is fine here.
	cfg.VerifyConnection(tls.ConnectionState{DidResume: true})
	if !strings.Contains(buf.String(), "resumed=true") {
		t.Errorf("debug output %q missing resumed=true", buf.String())
	}
}

func TestOperatorCertificateSelectionIgnoresIssuerHintsOnly(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	cfg, err := newAgentTLSConfig("device:50052", testCertInfo(t), nil, new(atomic.Int32), new(atomic.Pointer[certs.WendyIdentity]), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := &tls.CertificateRequestInfo{Version: tls.VersionTLS13, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}, AcceptableCAs: [][]byte{[]byte("unrelated issuer")}}
	if err := request.SupportsCertificate(&cfg.Certificates[0]); err == nil {
		t.Fatal("fixture must reproduce default selection rejection")
	}
	if cfg.GetClientCertificate == nil {
		t.Fatal("operator certificate selector missing")
	}
	selected, err := cfg.GetClientCertificate(request)
	if err != nil || selected == nil || len(selected.Certificate) == 0 {
		t.Fatalf("operator certificate omitted: %v", err)
	}
	if len(request.AcceptableCAs) != 1 {
		t.Fatal("mutated server request")
	}
	request.SignatureSchemes = []tls.SignatureScheme{tls.PSSWithSHA256}
	if _, err := cfg.GetClientCertificate(request); err == nil {
		t.Fatal("accepted an incompatible signature scheme")
	}
	if cfg.VerifyConnection == nil {
		t.Fatal("device certificate verification removed")
	}
}
