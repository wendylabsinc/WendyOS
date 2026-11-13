package clouddefaults

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// testCA is a minimal ECDSA certificate authority used only to mint test
// fixtures. Real broker/CLI mTLS trust chains are built by internal/shared/certs;
// this exists purely so the tests here can drive BrokerTLSConfig's
// VerifyConnection callback against real *x509.Certificate values instead of
// mocking crypto/tls itself.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

// newTestCA generates a fresh self-signed ECDSA CA. Each call produces an
// independent trust root, which is exactly what's needed to simulate "the
// broker's cert chains to a CA the client doesn't trust."
func newTestCA(t *testing.T, commonName string, serial int64) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return &testCA{
		cert: cert,
		key:  key,
		pem:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// leaf mints a certificate signed by ca, with the given extended key usage
// (client-auth for the CLI's own identity, server-auth to stand in for what
// the broker presents).
func (ca *testCA) leaf(t *testing.T, commonName string, serial int64, eku x509.ExtKeyUsage) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return cert
}

// leafPEM is like leaf but also returns the PEM-encoded cert and key, for
// building the config.CertificateInfo that BrokerTLSConfig itself consumes
// (as opposed to the raw *x509.Certificate values fed into VerifyConnection,
// which crypto/tls hands over as parsed certs, never PEM).
func (ca *testCA) leafPEM(t *testing.T, commonName string, serial int64, eku x509.ExtKeyUsage) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// testCertInfo builds a self-contained ECDSA CA + client leaf and returns the
// CertificateInfo shape BrokerTLSConfig consumes, mirroring the fixture in
// grpcclient/tls_config_test.go (test helpers aren't importable across
// packages, so this is a deliberate, small duplication). It also returns the
// CA itself so callers can mint additional leaves (e.g. a broker server cert)
// off the same trust root.
func testCertInfo(t *testing.T) (config.CertificateInfo, *testCA) {
	t.Helper()
	ca := newTestCA(t, "Broker TLS Test CA", 1)
	certPEM, keyPEM := ca.leafPEM(t, "broker-tls-test-client", 2, x509.ExtKeyUsageClientAuth)
	return config.CertificateInfo{
		PemCertificate:      certPEM,
		PemPrivateKey:       keyPEM,
		PemCertificateChain: ca.pem,
	}, ca
}

func TestUsesPublicCA(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{name: "production cloud endpoint", addr: "cloud.wendy.dev:443", want: true},
		{name: "local broker port", addr: "localhost:50052", want: false},
		{name: "on-prem broker with non-443 port", addr: "broker.example.com:8443", want: false},
		{name: "empty address", addr: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UsesPublicCA(tt.addr)
			if got != tt.want {
				t.Errorf("UsesPublicCA(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

// TestBrokerTLSConfig_PublicCA_NoPinning is the regression test for WDY-2434:
// the MCP server's copy of this assembly pinned the Wendy CA unconditionally,
// so dialing the production broker (a public-CA :443 endpoint) failed x509
// verification. For :443 endpoints, BrokerTLSConfig must leave standard WebPKI
// verification in place — no InsecureSkipVerify, no VerifyConnection override,
// no custom RootCAs (LoadTLSConfig is called with an empty CA bundle, so the
// system roots apply).
func TestBrokerTLSConfig_PublicCA_NoPinning(t *testing.T) {
	certInfo, _ := testCertInfo(t)

	cfg, err := BrokerTLSConfig(certInfo, "cloud.wendy.dev:443")
	if err != nil {
		t.Fatalf("BrokerTLSConfig() error = %v, want nil", err)
	}

	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want false for a public-CA endpoint")
	}
	if cfg.VerifyConnection != nil {
		t.Error("VerifyConnection is set, want nil for a public-CA endpoint (standard WebPKI verification should apply)")
	}
	if cfg.RootCAs != nil {
		t.Error("RootCAs is set, want nil so the system trust store is used")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want tls.VersionTLS12", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("len(Certificates) = %d, want 1", len(cfg.Certificates))
	}
}

// serverCert mints a server-auth leaf signed by ca for the given SANs, as the
// tls.Certificate a broker would present.
func (ca *testCA) serverCert(t *testing.T, serial int64, dnsNames []string, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen server key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "broker"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// brokerHandshake runs a real TLS handshake from cfg, through gRPC's TLS
// credentials (which set ServerName from the dial target exactly as a broker
// dial does), against a local server presenting serverCert.
func brokerHandshake(t *testing.T, cfg *tls.Config, serverCert tls.Certificate) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.(*tls.Conn).Handshake()
	}()

	addr := ln.Addr().String()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := credentials.NewTLS(cfg).ClientHandshake(ctx, addr, raw)
	if err == nil {
		conn.Close()
	}
	return err
}

// TestBrokerTLSConfig_NonStandardPort_VerifiesHostname covers the
// local/on-prem broker path (WDY-3756): the chain must reach the Wendy CA in
// cert.PemCertificateChain AND the leaf must be issued for the dialed host.
// Before, non-:443 ports skipped hostname verification, so any Wendy-CA leaf
// (e.g. another device's or service's) could stand in for the broker.
func TestBrokerTLSConfig_NonStandardPort_VerifiesHostname(t *testing.T) {
	certInfo, ca := testCertInfo(t)

	cfg, err := BrokerTLSConfig(certInfo, "127.0.0.1:50052")
	if err != nil {
		t.Fatalf("BrokerTLSConfig() error = %v, want nil", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want standard verification on every port")
	}
	if cfg.VerifyConnection != nil {
		t.Error("VerifyConnection is set, want standard chain + hostname verification only")
	}
	if cfg.RootCAs == nil {
		t.Fatal("RootCAs is nil, want the Wendy CA pool for a non-:443 broker")
	}

	t.Run("trusted CA, leaf for dialed host is accepted", func(t *testing.T) {
		err := brokerHandshake(t, cfg, ca.serverCert(t, 3, nil, []net.IP{net.ParseIP("127.0.0.1")}))
		if err != nil {
			t.Errorf("handshake error = %v, want nil", err)
		}
	})

	t.Run("trusted CA, leaf for unrelated host is rejected", func(t *testing.T) {
		err := brokerHandshake(t, cfg, ca.serverCert(t, 4, []string{"other-device.wendy.local"}, nil))
		var hostErr x509.HostnameError
		if !errors.As(err, &hostErr) {
			t.Errorf("handshake error = %v, want x509.HostnameError", err)
		}
	})

	t.Run("untrusted CA, leaf for dialed host is rejected", func(t *testing.T) {
		otherCA := newTestCA(t, "Unrelated CA", 10)
		err := brokerHandshake(t, cfg, otherCA.serverCert(t, 11, nil, []net.IP{net.ParseIP("127.0.0.1")}))
		var authErr x509.UnknownAuthorityError
		if !errors.As(err, &authErr) {
			t.Errorf("handshake error = %v, want x509.UnknownAuthorityError", err)
		}
	})
}

// TestBrokerTLSConfig_BadChainPEM exercises the failure path where
// cert.PemCertificateChain doesn't contain a parseable CA certificate. This
// only matters on the non-:443 path, where the chain is loaded into an
// x509.CertPool for pinning; the :443 path never reads PemCertificateChain as
// a CA pool at all.
func TestBrokerTLSConfig_BadChainPEM(t *testing.T) {
	certInfo, _ := testCertInfo(t)
	certInfo.PemCertificateChain = "this is not a PEM certificate"

	_, err := BrokerTLSConfig(certInfo, "localhost:50052")
	if err == nil {
		t.Fatal("BrokerTLSConfig() error = nil, want an error for an unparseable CA chain")
	}
	const want = "no valid CA certificates in PemCertificateChain"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("BrokerTLSConfig() error = %v, want it to contain %q", err, want)
	}
}

// TestDialBroker_NoCertificates covers the guard shared by both former
// package-local broker-dial functions in commands and mcp (now deleted in
// favor of this one): an auth entry with no certificates can't build client
// mTLS, so DialBroker must fail fast with the standard re-login message
// rather than reaching gRPC dial machinery.
func TestDialBroker_NoCertificates(t *testing.T) {
	auth := &config.AuthConfig{CloudGRPC: "localhost:50051"}

	_, err := DialBroker(auth, "")
	if err == nil {
		t.Fatal("DialBroker() error = nil, want an error for an auth entry with no certificates")
	}
	const want = "re-run 'wendy auth login'"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("DialBroker() error = %v, want it to contain %q", err, want)
	}
}

// TestDialBroker_ReturnsLazyConn drives DialBroker with a valid certificate
// fixture against a non-:443 broker URL. grpc.NewClient dials lazily (no
// network I/O until the first RPC), so this only proves DialBroker assembles
// a connection without error; BrokerTLSConfig's own tests already prove the
// TLS assembly (pinning vs WebPKI) is correct.
func TestDialBroker_ReturnsLazyConn(t *testing.T) {
	certInfo, _ := testCertInfo(t)
	certInfo.UserID = "test-user"
	auth := &config.AuthConfig{
		CloudGRPC:    "localhost:50051",
		Certificates: []config.CertificateInfo{certInfo},
	}

	conn, err := DialBroker(auth, "localhost:50052")
	if err != nil {
		t.Fatalf("DialBroker() error = %v, want nil", err)
	}
	if conn == nil {
		t.Fatal("DialBroker() conn = nil, want a non-nil lazy *grpc.ClientConn")
	}
	if err := conn.Close(); err != nil {
		t.Errorf("conn.Close() error = %v, want nil", err)
	}
}
