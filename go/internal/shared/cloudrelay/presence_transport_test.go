package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// publicTransportRelay keeps issuance separate from the public broker dial.
// The default broker validates the lease and a fresh one-use holder proof.
type publicTransportRelay struct {
	*testRelay
	lifetime           time.Duration
	malformedChallenge bool
}

func (r *publicTransportRelay) IssuePresenceLease(ctx context.Context, req *pb.IssuePresenceLeaseRequest) (*pb.IssuePresenceLeaseResponse, error) {
	l, err := r.testRelay.IssuePresenceLease(ctx, req)
	if err != nil {
		return nil, err
	}
	if r.lifetime > 0 {
		r.mu.Lock()
		r.lease.Exp = time.Now().Add(r.lifetime).Unix()
		r.leaseJWS = r.artifact(r.lease, leaseType)
		l.PresenceLeaseJws = r.leaseJWS
		r.mu.Unlock()
	}
	return l, nil
}

func (r *publicTransportRelay) RegisterPresence(s grpc.BidiStreamingServer[pb.RegisterPresenceRequest, pb.RegisterPresenceResponse]) error {
	if r.malformedChallenge {
		if _, err := s.Recv(); err != nil {
			return err
		}
		return s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Challenge{Challenge: &pb.BrokerChallenge{}}})
	}
	return r.testRelay.RegisterPresence(s)
}

func publicTransportFixture(t *testing.T, lifetime time.Duration, malformed bool) (*Agent, *publicTransportRelay, func(string) (*grpc.ClientConn, error)) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &publicTransportRelay{testRelay: &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: v, present: make(chan struct{}), offers: make(chan *pb.SessionOffer)}, lifetime: lifetime, malformedChallenge: malformed}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
	go srv.Serve(listener)
	t.Cleanup(srv.Stop)
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	v.relayDial = dial
	a := &Agent{Endpoint: r.endpoint, Verifier: v, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), sourceCheckInterval: time.Hour, dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
	return a, r, dial
}

func failingPublicDial(dialer func(context.Context, string) (net.Conn, error)) func(string) (*grpc.ClientConn, error) {
	return func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///local-public-broker-fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer))
	}
}

func unreachablePublicNetwork() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
}

func TestPublicPresencePreTCPUnavailableReusesVerifiedLease(t *testing.T) {
	a, r, dial := publicTransportFixture(t, 0, false)
	attempted := make(chan struct{}, 1)
	a.Verifier.relayDial = failingPublicDial(func(context.Context, string) (net.Conn, error) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		return nil, unreachablePublicNetwork()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.runOnce(ctx)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected public pre-TCP failure: %v", err)
	}
	renewalWait(t, attempted)
	if a.lease == nil || !time.Now().Before(time.Unix(a.leaseClaims.Exp, 0)) {
		t.Fatal("public network failure discarded verified unexpired lease")
	}
	old, oldClaims := a.lease.PresenceLeaseJws, a.leaseClaims
	assertResumeLease(t, a, old)
	r.mu.Lock()
	issues := r.issues
	r.mu.Unlock()
	if issues != 1 {
		t.Fatalf("issued %d leases before reconnect", issues)
	}
	// The next attempt uses a fresh public connection and the fixture verifies
	// the new holder proof. Cloud need not issue another signed lease.
	a.Verifier.relayDial = dial
	live, cancelLive := context.WithCancel(ctx)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- a.runOnce(live) }()
	t.Cleanup(func() {
		cancelLive()
		select {
		case <-exited:
		case <-time.After(time.Second):
		}
	})
	select {
	case <-r.present:
	case err := <-done:
		t.Fatalf("fresh proof reconnect failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelLive()
	if err := renewalWait(t, done); !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	if a.leaseClaims != oldClaims {
		t.Fatal("transport reconnect changed signed lease claims")
	}
	assertResumeLease(t, a, old)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.issues != 1 || r.registrations != 1 {
		t.Fatalf("reconnect issued %d leases, registered %d presences", r.issues, r.registrations)
	}
}

func TestPublicPresenceUnavailableCannotRetainExpiredLease(t *testing.T) {
	a, _, _ := publicTransportFixture(t, 2*time.Second, false)
	// Delay the synthetic TCP failure past the actual signed lease expiry.
	a.Verifier.relayDial = failingPublicDial(func(ctx context.Context, _ string) (net.Conn, error) {
		timer := time.NewTimer(2100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil, unreachablePublicNetwork()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.runOnce(ctx); status.Code(err) != codes.Unavailable {
		t.Fatalf("wrong transport failure: %v", err)
	}
	if a.lease != nil {
		t.Fatal("Unavailable retained an expired signed lease")
	}
	if _, err := os.Stat(filepath.Join(a.StateDir, "lease.pb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired resume lease remained on disk")
	}
}

func TestPublicPresenceConnectCancellationKeepsVerifiedLease(t *testing.T) {
	a, r, _ := publicTransportFixture(t, 0, false)
	started, stopped := make(chan struct{}), make(chan struct{})
	a.Verifier.relayDial = failingPublicDial(func(ctx context.Context, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.runOnce(ctx) }()
	renewalWait(t, started)
	cancel()
	if err := renewalWait(t, done); status.Code(err) != codes.Canceled {
		t.Fatalf("wrong parent cancellation: %v", err)
	}
	renewalWait(t, stopped)
	if a.lease == nil {
		t.Fatal("parent cancellation discarded verified resume lease")
	}
	assertResumeLease(t, a, a.lease.PresenceLeaseJws)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.issues != 1 || r.registrations != 0 {
		t.Fatal("canceled public dial performed issuance or admission again")
	}
}

func TestPublicPresenceMalformedChallengeStillInvalidatesLease(t *testing.T) {
	a, _, _ := publicTransportFixture(t, 0, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.runOnce(ctx); err == nil {
		t.Fatal("malformed broker challenge was accepted")
	}
	if a.lease != nil {
		t.Fatal("malformed broker challenge retained resume lease")
	}
	if _, err := os.Stat(filepath.Join(a.StateDir, "lease.pb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("malformed broker challenge left resume lease on disk")
	}
}
