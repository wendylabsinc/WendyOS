package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Cloud may queue an offer under a lease that never reached broker admission.
// Its replacement survives on disk, but the earlier lease must also survive an
// agent process restart until grants bound to it expire.
func TestAgentRestartAdmitsRedeliveredOfferForEarlierLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &redeliveryRelay{cloud: &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: verifier}, resetBeforeAcceptance: true, acked: make(chan *pb.PresenceOfferAcknowledgement, 1)}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
	go srv.Serve(listener)
	defer srv.Stop()
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	verifier.relayDial = dial
	dir := t.TempDir()
	newAgent := func() *Agent {
		return &Agent{Endpoint: r.cloud.endpoint, Verifier: verifier, StateDir: dir, Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
	}
	first := newAgent()
	if err := first.runOnce(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("first presence should be reset by the broker: %v", err)
	}
	// No object state is transferred: the second process has only StateDir.
	second := newAgent()
	finished := make(chan struct{})
	var secondErr error
	go func() {
		defer close(finished)
		secondErr = second.runOnce(ctx)
	}()
	defer func() { cancel(); <-finished }()
	select {
	case ack := <-r.acked:
		if ack.GetOfferId() != "opaque-offer" || ack.GetSessionId() != "opaque-session" {
			t.Fatalf("wrong offer acknowledged: %v", ack)
		}
	case <-finished:
		t.Fatalf("replacement process stopped before acknowledging the earlier offer: %v", secondErr)
	case <-ctx.Done():
		t.Fatal("replacement process did not acknowledge the offer bound to its earlier lease")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.issued) != 2 || r.offerLease != r.issued[0].JTI || r.offerLease == r.issued[1].JTI {
		t.Fatal("the offer must be bound to the prior process's replaced lease")
	}
}
