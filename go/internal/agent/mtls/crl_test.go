package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
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
	cfg, peerCheck, err := newFloorTLSConfig(own.PEM, p.PEM, own.KeyPEM, nil, time.Time{})
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
	check := livePeerCheck(peerCheck)
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
	if err := check(ctx, ""); err != nil {
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
	if err := check(ctx, ""); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("existing RPC accepted revoked peer: %v", err)
	}
}

// An outage or an expired CRL is not a revocation. Existing connections keep
// working, new operator sessions follow the fleet's grace, and only
// destructive methods insist on current evidence.
func TestAgedEvidenceGradesByDecision(t *testing.T) {
	p := crltest.New(t, true)
	own := p.Issue(t, 10, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	operator := p.Issue(t, 42, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	device := p.Issue(t, 43, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/33333333-3333-4333-8333-333333333333")
	previous := revocation.Default
	defer func() { revocation.Default = previous }()
	revocation.Default = revocation.New(&http.Client{Transport: failingTransport{}})
	if err := revocation.Default.Configure(revocation.Config{Grace: 0}); err != nil {
		t.Fatal(err)
	}
	cfg, peerCheck, err := newFloorTLSConfig(own.PEM, p.PEM, own.KeyPEM, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	state := func(leaf *x509.Certificate) tls.ConnectionState {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	}
	if err := cfg.VerifyConnection(state(operator.Cert)); !errors.Is(err, revocation.ErrUnavailable) {
		t.Fatalf("operator session admitted without evidence under a zero grace: %v", err)
	}
	// Device revocation has its own authority channel: never fail closed here.
	if err := cfg.VerifyConnection(state(device.Cert)); err != nil {
		t.Fatalf("device certificate refused for missing evidence: %v", err)
	}
	// The node's own identity is trusted as its own anchor and is not judged.
	if err := cfg.VerifyConnection(state(own.Cert)); err != nil {
		t.Fatalf("node refused its own identity: %v", err)
	}
	check := livePeerCheck(peerCheck)
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state(operator.Cert)}})
	if err := check(ctx, "/wendy.agent.services.v2.WendyContainerService/ListContainers"); err != nil {
		t.Fatalf("established connection refused for missing evidence: %v", err)
	}
	if err := check(ctx, agentpbv2.WendyProvisioningService_Unprovision_FullMethodName); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("destructive method ran without current evidence: %v", err)
	}
	// No configured grace: nothing is refused for age or absence.
	revocation.Default = revocation.New(&http.Client{Transport: failingTransport{}})
	if err := cfg.VerifyConnection(state(operator.Cert)); err != nil {
		t.Fatalf("operator refused without a configured grace: %v", err)
	}
	if err := check(ctx, agentpbv2.WendyProvisioningService_Unprovision_FullMethodName); err != nil {
		t.Fatalf("destructive method refused without a configured grace: %v", err)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("offline")
}

// The operator's machine staples the CRL into its TLS 1.3 client certificate
// message, so a device that cannot fetch still gets current evidence.
func TestStapledCRLAdmitsOperatorOnOfflineDevice(t *testing.T) {
	p := crltest.New(t, true)
	own := p.Issue(t, 10, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	operator := p.Issue(t, 42, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	previous := revocation.Default
	defer func() { revocation.Default = previous }()

	// The operator machine is online: its key pair picks up the issuer CRL.
	revocation.Default = revocation.New(p.Client)
	if _, err := certs.TLSKeyPair(operator.PEM, p.PEM, operator.KeyPEM); err != nil {
		t.Fatal(err)
	}
	revocation.Default.Refresh(context.Background())
	stapled, err := certs.TLSKeyPair(operator.PEM, p.PEM, operator.KeyPEM)
	if err != nil || len(stapled.OCSPStaple) == 0 {
		t.Fatalf("online operator key pair carries no CRL: %v", err)
	}
	bare := stapled
	bare.OCSPStaple = nil

	handshake := func(client tls.Certificate) error {
		// A fresh offline device that requires evidence for operator sessions.
		revocation.Default = revocation.New(&http.Client{Transport: failingTransport{}})
		if err := revocation.Default.Configure(revocation.Config{Grace: 0}); err != nil {
			t.Fatal(err)
		}
		cfg, err := NewTLSConfig(own.PEM, p.PEM, own.KeyPEM, nil, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		server := tls.Server(a, cfg)
		go func() {
			c := tls.Client(b, &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{client}, MinVersion: tls.VersionTLS13})
			_ = c.Handshake()
			_, _ = io.Copy(io.Discard, c)
		}()
		_ = a.SetDeadline(time.Now().Add(10 * time.Second))
		return server.Handshake()
	}
	if err := handshake(bare); !errors.Is(err, revocation.ErrUnavailable) {
		t.Fatalf("offline device admitted an operator without evidence: %v", err)
	}
	if err := handshake(stapled); err != nil {
		t.Fatalf("stapled CRL did not admit the operator: %v", err)
	}
}
