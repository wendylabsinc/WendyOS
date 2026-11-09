package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPresenceLeaseWireDeadlineAndCallerCancellation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan time.Duration, 2)
	canceled := make(chan struct{})
	var block atomic.Bool
	srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		until, ok := ctx.Deadline()
		if !ok {
			t.Error("lease request has no deadline")
		}
		received <- time.Until(until)
		if block.Load() {
			<-ctx.Done()
			close(canceled)
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return next(ctx, req)
	}))
	r := &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: v}
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	go srv.Serve(listener)
	defer srv.Stop()
	a := &Agent{Endpoint: r.endpoint, Verifier: v, StateDir: t.TempDir(), Logger: zap.NewNop(), Credentials: func() (string, string, []byte) { return "", "", nil }, dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}}
	if err := a.keys(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.requestLease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if budget := <-received; budget < 55*time.Second || budget > 61*time.Second {
		t.Fatalf("wire request budget %v cannot accommodate a slow Cloud connection", budget)
	}
	block.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := a.requestLease(ctx); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("short caller deadline: %v", err)
	}
	if budget := <-received; budget <= 0 || budget > 150*time.Millisecond {
		t.Fatalf("caller deadline was extended: %v", budget)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Cloud request outlived caller cancellation")
	}
}

func TestPresenceRenewalLeavesRetryRoomWithoutShortLeaseChurn(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	c := claims{Iat: now.Unix(), Exp: now.Add(5 * time.Minute).Unix()}
	d := renewalDelay(&pb.IssuePresenceLeaseResponse{}, c)
	headroom := time.Until(time.Unix(c.Exp, 0)) - d
	if headroom < 135*time.Second-time.Millisecond || headroom > 135*time.Second+time.Millisecond {
		t.Fatalf("normal lease renewal headroom=%v, want two slow attempts plus retry room", headroom)
	}
	c.Exp = now.Add(time.Minute).Unix()
	if delay := renewalDelay(&pb.IssuePresenceLeaseResponse{}, c); delay < 25*time.Second || delay > 30*time.Second {
		t.Fatalf("short lease renews too often: %v", delay)
	}
	c.Exp = now.Add(5 * time.Minute).Unix()
	hint := time.Now().Add(5 * time.Second)
	if delay := renewalDelay(&pb.IssuePresenceLeaseResponse{RenewAfter: timestamppb.New(hint)}, c); delay < 4*time.Second || delay > 5*time.Second {
		t.Fatalf("earlier Cloud hint ignored: %v", delay)
	}
	c.Exp = now.Add(time.Second).Unix()
	if delay := renewalDelay(&pb.IssuePresenceLeaseResponse{}, c); delay != time.Second {
		t.Fatalf("near-expiry backoff=%v, want1s", delay)
	}
}
