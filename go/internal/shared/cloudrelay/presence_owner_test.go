package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"net"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type delayedPresenceOwner struct {
	*testRelay
	refusal     codes.Code
	nearExpiry  bool
	beforeProof bool
	attempts    int
}

func (r *delayedPresenceOwner) IssuePresenceLease(ctx context.Context, req *pb.IssuePresenceLeaseRequest) (*pb.IssuePresenceLeaseResponse, error) {
	l, err := r.testRelay.IssuePresenceLease(ctx, req)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nearExpiry && r.issues == 1 {
		r.lease.Exp = time.Now().Add(20 * time.Second).Unix()
		l.PresenceLeaseJws = r.artifact(r.lease, leaseType)
	}
	return l, nil
}

func (r *delayedPresenceOwner) RegisterPresence(s grpc.BidiStreamingServer[pb.RegisterPresenceRequest, pb.RegisterPresenceResponse]) error {
	r.mu.Lock()
	r.attempts++
	first := r.attempts == 1
	r.mu.Unlock()
	if !first {
		return r.testRelay.RegisterPresence(s)
	}
	m, err := s.Recv()
	if err != nil || m.GetOpen() == nil {
		return fmt.Errorf("missing presence open: %v", err)
	}
	o := m.GetOpen()
	c, err := r.verifier.verify(s.Context(), o.PresenceLeaseJws, leaseType, r.broker().Audience, time.Now())
	if err != nil || c.Signing != binding(o.AgentSigningPublicKeySpkiDer) {
		return fmt.Errorf("invalid presence lease or key: %v", err)
	}
	pub, err := x509.ParsePKIXPublicKey(o.AgentSigningPublicKeySpkiDer)
	if err != nil {
		return err
	}
	if r.beforeProof {
		return status.Error(r.refusal, "presence route is not ready")
	}
	ch := challenge()
	if err = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Challenge{Challenge: ch}}); err != nil {
		return err
	}
	m, err = s.Recv()
	if err != nil || m.GetProof() == nil {
		return fmt.Errorf("missing presence proof: %v", err)
	}
	proof := m.GetProof()
	if !verifyProof(pub.(*ecdsa.PublicKey), "presence", c, o.PresenceLeaseJws, "agent-presence", ch, proof.Signature) {
		return fmt.Errorf("invalid presence proof")
	}
	// Production checks the lease and one-use holder proof before reporting
	// that its previous, half-open transport still owns the route.
	return status.Error(r.refusal, "presence route is not ready")
}

func TestAgentRetainsValidLeaseWhilePriorPresenceOwnerDrains(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refusal     codes.Code
		nearExpiry  bool
		beforeProof bool
		issues      int
	}{
		{"owned route", codes.AlreadyExists, false, false, 1},
		{"refused lease", codes.PermissionDenied, false, false, 2},
		{"near expiry still refreshes", codes.AlreadyExists, true, false, 2},
		{"unproven ownership refusal", codes.AlreadyExists, false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			v := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			r := &delayedPresenceOwner{testRelay: &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: v, present: make(chan struct{}), offers: make(chan *pb.SessionOffer)}, refusal: tc.refusal, nearExpiry: tc.nearExpiry, beforeProof: tc.beforeProof}
			srv := grpc.NewServer()
			pb.RegisterTunnelAuthorizationServiceServer(srv, r)
			pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
			go srv.Serve(listener)
			defer srv.Stop()
			dial := func(endpoint string) (*grpc.ClientConn, error) {
				return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
			}
			v.relayDial = dial
			a := &Agent{Endpoint: r.endpoint, Verifier: v, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
			done := make(chan struct{})
			go func() { defer close(done); a.Run(ctx) }()
			defer func() { cancel(); <-done }()
			select {
			case <-r.present:
			case <-ctx.Done():
				t.Fatal("presence did not recover after the prior owner drained")
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.issues != tc.issues || r.attempts != 2 {
				t.Fatalf("issued %d leases for %d attempts, want %d leases for 2 attempts", r.issues, r.attempts, tc.issues)
			}
		})
	}
}
