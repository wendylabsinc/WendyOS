package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
)

func brokerTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "audit trusted CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func brokerTestServer(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, names []string, ips []net.IP, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: names, IPAddresses: ips, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key}
}

// This invokes the real gRPC TLS credential handshake. The socket connects to
// loopback while authority names the intended endpoint, just as a redirected
// DNS/IP connection would; the server proves possession of its actual key.
func brokerTestGRPCHandshake(t *testing.T, cfg *tls.Config, identity tls.Certificate, authority string) (tls.ConnectionState, error) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		conn := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{identity}, NextProtos: []string{"h2"}})
		err = conn.Handshake()
		if err == nil {
			_, err = conn.Write([]byte{1})
		}
		done <- err
	}()
	raw, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	secure, info, err := credentials.NewTLS(cfg).ClientHandshake(ctx, authority, raw)
	if err != nil {
		raw.Close()
		<-done
		return tls.ConnectionState{}, err
	}
	var one [1]byte
	_, err = secure.Read(one[:])
	state := info.(credentials.TLSInfo).State
	secure.Close()
	if serverErr := <-done; err == nil {
		err = serverErr
	}
	return state, err
}

func TestBrokerTLSConfigVerifiesEndpointIdentity(t *testing.T) {
	ca, key, pemCA := brokerTestCA(t)
	otherCA, otherKey, _ := brokerTestCA(t)
	tests := []struct {
		name, authority, cn string
		dns                 []string
		ips                 []net.IP
		usage               x509.ExtKeyUsage
		other, reject       bool
	}{
		{name: "expected DNS SAN", authority: "broker.example:443", cn: "broker", dns: []string{"broker.example"}, usage: x509.ExtKeyUsageServerAuth},
		{name: "unrelated trusted DNS SAN", authority: "broker.example:443", cn: "unrelated", dns: []string{"unrelated.example"}, usage: x509.ExtKeyUsageServerAuth, reject: true},
		{name: "custom Wendy CA localhost", authority: "localhost:50052", cn: "localhost", dns: []string{"localhost", "*.local"}, ips: []net.IP{net.ParseIP("127.0.0.1")}, usage: x509.ExtKeyUsageServerAuth},
		{name: "custom Wendy CA local mDNS SAN", authority: "lab-mac.local:50052", cn: "localhost", dns: []string{"localhost", "*.local"}, usage: x509.ExtKeyUsageServerAuth},
		{name: "custom Wendy CA explicit LAN IP", authority: "192.168.1.161:50052", cn: "localhost", ips: []net.IP{net.ParseIP("192.168.1.161")}, usage: x509.ExtKeyUsageServerAuth},
		{name: "stale LAN IP certificate", authority: "192.168.1.161:50052", cn: "localhost", ips: []net.IP{net.ParseIP("192.168.1.99")}, usage: x509.ExtKeyUsageServerAuth, reject: true},
		{name: "legacy CN only localhost", authority: "localhost:50052", cn: "localhost", usage: x509.ExtKeyUsageServerAuth, reject: true},
		{name: "client-only certificate", authority: "broker.example:443", cn: "broker", dns: []string{"broker.example"}, usage: x509.ExtKeyUsageClientAuth, reject: true},
		{name: "unknown CA expected hostname", authority: "broker.example:443", cn: "broker", dns: []string{"broker.example"}, usage: x509.ExtKeyUsageServerAuth, other: true, reject: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := brokerTLSConfig(zap.NewNop(), "", "", pemCA)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.InsecureSkipVerify || cfg.RootCAs == nil {
				t.Fatal("standard configured-root verification required")
			}
			issuer, issuerKey := ca, key
			if test.other {
				issuer, issuerKey = otherCA, otherKey
			}
			state, err := brokerTestGRPCHandshake(t, cfg, brokerTestServer(t, issuer, issuerKey, test.cn, test.dns, test.ips, test.usage), test.authority)
			if test.reject && err == nil {
				t.Fatal("wrong identity/purpose/trust accepted")
			}
			if !test.reject && err != nil {
				t.Fatalf("supported broker rejected: %v", err)
			}
			if !test.reject {
				t.Logf("TLS=%x key_exchange=%s cipher=%s", state.Version, state.CurveID, tls.CipherSuiteName(state.CipherSuite))
			}
			if test.name == "legacy CN only localhost" && !strings.Contains(err.Error(), "legacy Common Name") {
				t.Fatalf("unexpected CN-only rejection: %v", err)
			}
		})
	}
}
