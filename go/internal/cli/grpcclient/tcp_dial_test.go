package grpcclient

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestTLSConnectionReportsActualTCPFailure(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	wantErr := errors.New("TCP route is unreachable")
	stubLinkLocalDial(t, func(context.Context, string) (net.Conn, error) { return nil, wantErr })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := ConnectWithTLSExpecting(ctx, "127.0.0.1:50052", testCertInfo(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err == nil {
		t.Fatal("unreachable TCP endpoint answered")
	}
	if err := conn.TCPDialError(); !errors.Is(err, wantErr) {
		t.Fatalf("TCPDialError = %v, want %v", err, wantErr)
	}
}

func TestTLSConnectionDoesNotReportHandshakeFailureAsTCPFailure(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	lis := bufconn.Listen(1 << 20)
	defer lis.Close()
	serveFakeAgent(t, lis)
	stubLinkLocalDial(t, func(ctx context.Context, _ string) (net.Conn, error) {
		// The endpoint answers after one lost SYN. The TLS failure must still
		// let callers try another operator certificate, rather than discard
		// the endpoint as an unreachable TCP route.
		timer := time.NewTimer(1200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return lis.DialContext(ctx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := ConnectWithTLSExpecting(ctx, "127.0.0.1:50052", testCertInfo(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err == nil {
		t.Fatal("TLS connection to plaintext server succeeded")
	}
	if err := conn.TCPDialError(); err != nil {
		t.Fatalf("TLS handshake failure became TCP failure: %v", err)
	}
}

func TestTLSConnectionExtraDialOptionsTakePrecedence(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	stubLinkLocalDial(t, func(context.Context, string) (net.Conn, error) {
		t.Error("default TCP dialer overrode caller option")
		return nil, net.ErrClosed
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := ConnectWithTLSExpecting(ctx, "127.0.0.1:50052", testCertInfo(t), nil, nil,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return nil, net.ErrClosed }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err := conn.TCPDialError(); err != nil {
		t.Fatalf("overridden TCP dialer reported failure: %v", err)
	}
}
