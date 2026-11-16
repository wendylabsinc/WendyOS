package grpcclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
)

type crlTransport func(*http.Request) (*http.Response, error)

func (f crlTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientRefreshesStapleOnHandshake(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	p := crltest.New(t, true)
	const principal = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/"
	own := p.Issue(t, 10, principal+"device/22222222-2222-4222-8222-222222222222")
	operator := p.Issue(t, 42, principal+"operator/alice")
	transport := p.Client.Transport
	release := make(chan struct{})
	p.Client.Transport = crlTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case <-release:
			return transport.RoundTrip(r)
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	manager := revocation.New(p.Client)
	previous := revocation.Default
	revocation.Default = manager
	defer func() { manager.Refresh(context.Background()); revocation.Default = previous }()
	expected, found, err := certs.IdentityFromCert(own.Cert)
	if err != nil || !found {
		t.Fatal(err)
	}
	identity := &config.CertificateInfo{PemCertificate: operator.PEM, PemCertificateChain: p.PEM, PemPrivateKey: operator.KeyPEM}
	cfg, err := newAgentTLSConfig("localhost:50052", identity, nil, new(atomic.Int32), new(atomic.Pointer[certs.WendyIdentity]), &expected, false, nil, nil)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates[0].OCSPStaple) != 0 {
		t.Fatal("cold config unexpectedly already had evidence")
	}
	serverPair, err := tls.X509KeyPair([]byte(own.PEM), []byte(own.KeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	handshake := func() []byte {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		_ = a.SetDeadline(time.Now().Add(5 * time.Second))
		_ = b.SetDeadline(time.Now().Add(5 * time.Second))
		server := tls.Server(a, &tls.Config{Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13})
		client := tls.Client(b, cfg)
		done := make(chan error, 1)
		go func() { done <- client.Handshake() }()
		if err := server.Handshake(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return server.ConnectionState().OCSPResponse
	}
	offline := revocation.New(&http.Client{Transport: crlTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })})
	if err := offline.Configure(revocation.Config{Grace: 0}); err != nil {
		t.Fatal(err)
	}
	offline.Offer(handshake())
	path := []*x509.Certificate{operator.Cert, p.CA}
	if err := offline.CheckPath(path, time.Now(), revocation.Fresh); err != nil {
		t.Fatalf("cold client could not carry evidence to offline verifier: %v", err)
	}
	p.Publish(t, 2, 42)
	manager.Refresh(context.Background())
	offline.Offer(handshake())
	if err := offline.CheckPath(path, time.Now(), revocation.Fresh); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("retained TLS config sent old evidence: %v", err)
	}
}
