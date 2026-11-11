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
	"google.golang.org/grpc/metadata"
)

// NewApplicationRelayTestBroker exposes only the existing signed-grant broker
// fixture to external-package integration tests. Keeping the adapter independent
// of localmesh avoids its discovery/lite relay import cycle with cloudrelay.
func NewApplicationRelayTestBroker(t *testing.T, ctx context.Context, appDial func(context.Context) (net.Conn, error)) (func(context.Context, string, string) (net.Conn, error), func(*testing.T)) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: verifier, requested: make(chan *pb.RequestTunnelRequest, 1), offers: make(chan *pb.SessionOffer, 1), present: make(chan struct{}), ack: make(chan struct{}), paired: make(chan struct{}), joined: map[pb.JoinRole]bool{}, toCaller: make(chan *pb.TunnelFrame, 8), toAgent: make(chan *pb.TunnelFrame, 8)}
	r.instruction = &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: applicationServicePort, ServiceDescriptor: []byte(ApplicationService)}
	broker := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(broker, r)
	pb.RegisterTunnelBrokerV2ServiceServer(broker, r)
	go broker.Serve(listener)
	t.Cleanup(broker.Stop)
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	verifier.relayDial = dial
	agent := &Agent{Endpoint: r.endpoint, Verifier: verifier, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), ApplicationDial: appDial, dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agent.runOnce(metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer device-test")))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-agentDone:
		case <-time.After(time.Second):
			t.Error("Cloud agent failed to stop")
		}
	})
	select {
	case <-r.present:
	case err := <-agentDone:
		t.Fatalf("Cloud presence failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cloudConn, err := dial(r.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cloudConn.Close() })
	authCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-test"))
	appRelayDial := func(context.Context, string, string) (net.Conn, error) {
		// Relay lifetime belongs to the flow, rather than TLS setup's short
		// context (which is canceled after the authenticated port ACK).
		return OpenTCP(ctx, authCtx, cloudConn, verifier, "00000000-0000-4000-8000-000000000042", ApplicationService, func(descriptor []byte) ([]byte, error) { return descriptor, nil })
	}
	return appRelayDial, func(t *testing.T) {
		t.Helper()
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.joined[pb.JoinRole_JOIN_ROLE_CALLER] || !r.joined[pb.JoinRole_JOIN_ROLE_AGENT] {
			t.Fatal("application bypassed normal caller/agent broker joins")
		}
	}
}
