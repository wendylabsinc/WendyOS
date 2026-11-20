package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// absentAgentRelay is a Cloud and broker for a device that holds presence but
// never joins a session, with grants that expire after ttl seconds.
func absentAgentRelay(t *testing.T, ttl int64) (*Verifier, *grpc.ClientConn) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	signing, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	encryption, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	r := &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: verifier, grantTTL: ttl,
		requested: make(chan *pb.RequestTunnelRequest, 1), offers: make(chan *pb.SessionOffer, 1),
		paired: make(chan struct{}), joined: map[pb.JoinRole]bool{},
		signing: publicDER(signing), encryption: publicDER(encryption)}
	r.lease = claims{Route: "opaque-route", JTI: "opaque-lease", Signing: binding(r.signing)}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
	go srv.Serve(listener)
	t.Cleanup(srv.Stop)
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	verifier.relayDial = dial
	cloudConn, err := dial(r.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cloudConn.Close() })
	return verifier, cloudConn
}

func openAbsentAgent(ctx context.Context, verifier *Verifier, cloudConn *grpc.ClientConn) error {
	authCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-test"))
	conn, err := OpenTCP(ctx, authCtx, cloudConn, verifier, "00000000-0000-4000-8000-000000000042", "wendy-agent", func(b []byte) ([]byte, error) { return b, nil })
	if err == nil {
		conn.Close()
	}
	return err
}

// A device that never joins leaves the caller waiting until the grant's
// admission window closes. That is a device that did not accept the session,
// not a cancelled command (WDY-4295: "context canceled").
func TestCallerLearnsTheDeviceDidNotAcceptTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	verifier, cloudConn := absentAgentRelay(t, 2)

	err := openAbsentAgent(ctx, verifier, cloudConn)
	var notAdmitted *NotAdmittedError
	if !errors.As(err, &notAdmitted) {
		t.Fatalf("OpenTCP error = %v, want a NotAdmittedError", err)
	}
	if !strings.Contains(err.Error(), "did not accept the Cloud session") || strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("message = %q", err.Error())
	}
	if status.Code(notAdmitted.Err) != codes.Canceled {
		t.Fatalf("cause = %v, want the admission timer's cancellation", notAdmitted.Err)
	}
}

// The caller's own cancellation stays a cancellation.
func TestCallerCancellationIsNotADeviceRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	verifier, cloudConn := absentAgentRelay(t, 60)

	callCtx, stop := context.WithCancel(ctx)
	time.AfterFunc(300*time.Millisecond, stop)
	err := openAbsentAgent(callCtx, verifier, cloudConn)
	var notAdmitted *NotAdmittedError
	if err == nil || errors.As(err, &notAdmitted) {
		t.Fatalf("OpenTCP error = %v, want the caller's cancellation", err)
	}
}

func TestAdmissionError(t *testing.T) {
	open := claims{Exp: time.Now().Add(time.Minute).Unix()}
	closed := claims{Exp: time.Now().Add(-time.Second).Unix()}
	cancelled := status.Error(codes.Canceled, "context canceled")
	done, stop := context.WithCancel(context.Background())
	stop()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		c    claims
		err  error
		want bool
	}{
		{"admission timer", context.Background(), closed, cancelled, true},
		{"deadline as the cause", context.Background(), closed, context.DeadlineExceeded, true},
		{"broker closed admission", context.Background(), closed, status.Error(codes.FailedPrecondition, "admission closed"), true},
		{"window still open", context.Background(), open, cancelled, false},
		{"caller cancelled", done, closed, cancelled, false},
		{"another failure at expiry", context.Background(), closed, status.Error(codes.Unavailable, "connection reset"), false},
	} {
		var notAdmitted *NotAdmittedError
		got := errors.As(admissionError(tc.ctx, tc.c, time.Now(), tc.err), &notAdmitted)
		if got != tc.want {
			t.Errorf("%s: NotAdmittedError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
