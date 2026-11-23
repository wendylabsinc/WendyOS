package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
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
func absentAgentRelay(t *testing.T, ttl int64, joinOpenErr error) (*Verifier, *grpc.ClientConn, <-chan struct{}) {
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
		joinOpenErr: joinOpenErr, callerProved: make(chan struct{}),
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
	return verifier, cloudConn, r.callerProved
}

func openAbsentAgent(ctx context.Context, verifier *Verifier, cloudConn *grpc.ClientConn) error {
	authCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-test"))
	conn, err := OpenTCP(ctx, authCtx, cloudConn, verifier, "00000000-0000-4000-8000-000000000042", "wendy-agent", func(b []byte) ([]byte, error) { return b, nil })
	if err == nil {
		conn.Close()
	}
	return err
}

// The admission deadline is distinct from a caller canceling the command.
func TestCallerLearnsSessionAdmissionTimedOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	verifier, cloudConn, _ := absentAgentRelay(t, 2, nil)

	err := openAbsentAgent(ctx, verifier, cloudConn)
	var notAdmitted *NotAdmittedError
	if !errors.As(err, &notAdmitted) {
		t.Fatalf("OpenTCP error = %v, want a NotAdmittedError", err)
	}
	if !strings.HasPrefix(err.Error(), "Cloud session admission did not complete within ") {
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
	verifier, cloudConn, proved := absentAgentRelay(t, 60, nil)

	callCtx, stop := context.WithCancel(ctx)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- openAbsentAgent(callCtx, verifier, cloudConn) }()
	select {
	case <-proved:
	case err := <-result:
		t.Fatalf("OpenTCP ended before the caller proved its key: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	if err := <-result; err != context.Canceled {
		t.Fatalf("OpenTCP error = %v, want context.Canceled", err)
	}
}

func TestNearExpiryJoinKeepsDistinctBrokerFailures(t *testing.T) {
	for _, tc := range []struct {
		code    codes.Code
		message string
		closed  bool
	}{
		{codes.FailedPrecondition, "grant mismatch", false},
		{codes.Canceled, "broker shut down the stream", false},
		{codes.DeadlineExceeded, "broker setup timed out", false},
		{codes.FailedPrecondition, "admission closed", true},
	} {
		t.Run(tc.message, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			verifier, cloudConn, _ := absentAgentRelay(t, 4, status.Error(tc.code, tc.message))
			err := openAbsentAgent(ctx, verifier, cloudConn)
			var notAdmitted *NotAdmittedError
			if got := errors.As(err, &notAdmitted); got != tc.closed {
				t.Fatalf("OpenTCP error = %v, want NotAdmittedError = %v", err, tc.closed)
			}
			if notAdmitted != nil {
				err = notAdmitted.Err
			}
			if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("OpenTCP cause = %v, want %s: %s", err, tc.code, tc.message)
			}
		})
	}
}

func TestAdmissionError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		err   error
		want  bool
	}{
		{"local timer canceled GRPC", errAdmissionTimeout, status.FromContextError(context.Canceled).Err(), true},
		{"local timer canceled join", errAdmissionTimeout, context.Canceled, true},
		{"setup deadline without the timer", nil, context.DeadlineExceeded, false},
		{"cancellation without the timer", nil, status.FromContextError(context.Canceled).Err(), false},
		{"broker closed admission", nil, fmt.Errorf("joining Cloud relay: %w", status.Error(codes.FailedPrecondition, "admission closed")), true},
		{"broker grant mismatch during timer", errAdmissionTimeout, status.Error(codes.FailedPrecondition, "grant mismatch"), false},
		{"another failure during timer", errAdmissionTimeout, status.Error(codes.Unavailable, "connection reset"), false},
	} {
		var notAdmitted *NotAdmittedError
		got := errors.As(admissionError(context.Background(), tc.cause, time.Now(), tc.err), &notAdmitted)
		if got != tc.want {
			t.Errorf("%s: NotAdmittedError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
