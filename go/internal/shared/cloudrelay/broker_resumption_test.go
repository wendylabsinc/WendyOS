package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
)

func TestPublicBrokerTLSResumptionAndVerification(t *testing.T) {
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "broker root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "broker.test"}, DNSNames: []string{"broker.test"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, leafKey.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	states := make(chan tls.ConnectionState, 8)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}})), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		p, _ := peer.FromContext(ctx)
		states <- p.AuthInfo.(credentials.TLSInfo).State
		return next(ctx, req)
	}))
	healthpb.RegisterHealthServer(server, health.NewServer())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	config := publicBrokerTLSConfig()
	if config.InsecureSkipVerify || config.VerifyConnection != nil || config.VerifyPeerCertificate != nil || config.RootCAs != nil || len(config.Certificates) != 0 || config.GetClientCertificate != nil || config.MinVersion != tls.VersionTLS13 {
		t.Fatal("public broker departed from standard verified server TLS without client identity")
	}
	// The fixture has its own isolated trust policy and cache; production always
	// uses system roots and the shared public-only cache from the factory.
	config.RootCAs = x509.NewCertPool()
	config.RootCAs.AddCert(root)
	config.ServerName = "broker.test"
	if config.ClientSessionCache != nil {
		config.ClientSessionCache = tls.NewLRUClientSessionCache(4)
	}
	check := func(cfg *tls.Config) error {
		t.Helper()
		conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		return err
	}
	for i := range 2 {
		if err := check(config.Clone()); err != nil {
			t.Fatal(err)
		}
		state := <-states
		if state.DidResume != (i == 1) {
			t.Fatalf("connection %d resumed=%v, want %v", i, state.DidResume, i == 1)
		}
		if len(state.PeerCertificates) != 0 {
			t.Fatal("public broker TLS presented a client identity")
		}
	}
	for _, tc := range []struct {
		name, reason string
		change       func(*tls.Config)
	}{
		{"wrong hostname", "not other.test", func(c *tls.Config) { c.ServerName = "other.test" }},
		{"unknown root", "unknown authority", func(c *tls.Config) {
			c.RootCAs = x509.NewCertPool()
			c.ClientSessionCache = tls.NewLRUClientSessionCache(4)
		}},
		{"expired cached server", "expired", func(c *tls.Config) { c.Time = func() time.Time { return now.Add(2 * time.Hour) } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Clone()
			tc.change(cfg)
			if err := check(cfg); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("verification error=%v, want %q", err, tc.reason)
			}
		})
	}
}
