package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestCRLRevokesWarmPathResumedTLSAndExistingRPC(t *testing.T) {
	p := crltest.New(t, true)
	own := p.Issue(t, 10, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	remote := p.Issue(t, 42, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	previous := revocation.Default
	revocation.Default = revocation.New(p.Client)
	defer func() { revocation.Default = previous }()
	cfg, err := NewTLSConfig(own.PEM, p.PEM, own.KeyPEM, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	verify, err := NewPeerCertificateVerifier(p.PEM, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]byte{remote.Cert.Raw}
	now := time.Now()
	for range 3 {
		if _, err := verify(chain, now); err != nil {
			t.Fatal(err)
		}
	}
	state := tls.ConnectionState{DidResume: true, PeerCertificates: []*x509.Certificate{remote.Cert}}
	if err := cfg.VerifyConnection(state); err != nil {
		t.Fatal(err)
	}
	check := livePeerCheck(cfg.VerifyPeerCertificate)
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
	if err := check(ctx); err != nil {
		t.Fatal(err)
	}
	p.Publish(t, 2, 42)
	revocation.Default.Refresh(context.Background())
	if _, err := verify(chain, now); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("warm path accepted revoked peer: %v", err)
	}
	if err := cfg.VerifyPeerCertificate(chain, nil); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("new handshake accepted revoked peer: %v", err)
	}
	if err := cfg.VerifyConnection(state); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("resumed handshake accepted revoked peer: %v", err)
	}
	if err := check(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("existing RPC accepted revoked peer: %v", err)
	}
}
