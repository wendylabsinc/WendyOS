package grpcclient

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestBoundedAgentTCPDialerDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
	}{
		{name: "default", budget: agentTCPDialTimeout},
		{name: "shorter caller", budget: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.name == "shorter caller" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.budget)
				defer cancel()
			}
			dial, state := boundedAgentTCPDialer(func(ctx context.Context, addr string) (net.Conn, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > tc.budget || time.Until(deadline) < tc.budget-25*time.Millisecond {
					t.Errorf("dial deadline = %v, want a %s budget", deadline, tc.budget)
				}
				if addr != "192.0.2.1:50052" {
					t.Errorf("dial address = %q", addr)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			})
			if _, err := dial(ctx, "192.0.2.1:50052"); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("dial error = %v, want deadline", err)
			}
			conn := &AgentConnection{tcpDial: state}
			if err := conn.TCPDialError(); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("TCPDialError = %v, want deadline", err)
			}
		})
	}
}

func TestBoundedAgentTCPDialerKeepsSuccessfulConnectionEvidence(t *testing.T) {
	wantErr := errors.New("unreachable route")
	var mu sync.Mutex
	attempt := 0
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	dial, state := boundedAgentTCPDialer(func(context.Context, string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		attempt++
		if attempt == 2 {
			return client, nil
		}
		return nil, wantErr
	})
	conn := &AgentConnection{tcpDial: state}
	if err := conn.TCPDialError(); err != nil {
		t.Fatalf("unattempted dial error = %v", err)
	}
	_, _ = dial(context.Background(), "device:50052")
	if err := conn.TCPDialError(); !errors.Is(err, wantErr) {
		t.Fatalf("failed dial error = %v, want %v", err, wantErr)
	}
	_, _ = dial(context.Background(), "device:50052")
	_, _ = dial(context.Background(), "device:50052")
	if err := conn.TCPDialError(); err != nil {
		t.Fatalf("a TCP success must survive later failed reconnects, got %v", err)
	}
}

func TestBoundedAgentTCPDialerAllowsSYNRetransmission(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	dial, state := boundedAgentTCPDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		// Model a successful connect after the first SYN was lost. The first
		// retransmission starts around one second, so a one-second budget
		// rejects this reachable endpoint before the retry can complete.
		timer := time.NewTimer(1200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return client, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if _, err := dial(context.Background(), "device:50052"); err != nil {
		t.Fatalf("TCP connection after a SYN retransmission failed: %v", err)
	}
	conn := &AgentConnection{tcpDial: state}
	if err := conn.TCPDialError(); err != nil {
		t.Fatalf("successful delayed connection reported TCP failure: %v", err)
	}
}

func TestBoundedAgentTCPDialerPreservesReconnectDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
	}{
		{name: "without deadline"},
		{name: "longer caller deadline", budget: 10 * time.Second},
		{name: "shorter caller deadline", budget: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			ctx := context.Background()
			if tc.budget > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.budget)
				defer cancel()
			}
			attempt := 0
			dial, _ := boundedAgentTCPDialer(func(dialCtx context.Context, _ string) (net.Conn, error) {
				attempt++
				if attempt > 1 {
					gotDeadline, gotOK := dialCtx.Deadline()
					wantDeadline, wantOK := ctx.Deadline()
					if gotOK != wantOK || !gotDeadline.Equal(wantDeadline) {
						t.Errorf("reconnect deadline = %v (%v), want caller deadline %v (%v)", gotDeadline, gotOK, wantDeadline, wantOK)
					}
				}
				return client, nil
			})
			if _, err := dial(ctx, "device:50052"); err != nil {
				t.Fatal(err)
			}
			if _, err := dial(ctx, "device:50052"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func stubAgentProxy(t *testing.T, fn func(*http.Request) (*url.URL, error)) {
	t.Helper()
	orig := agentProxyFromEnvironment
	agentProxyFromEnvironment = fn
	t.Cleanup(func() { agentProxyFromEnvironment = orig })
}

func TestAgentTCPDialOptionsPreservesProxyRouting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
		proxy   *url.URL
		err     error
		direct  bool
	}{
		{name: "direct", address: "192.0.2.1:50052", direct: true},
		{name: "HTTP CONNECT proxy", address: "192.0.2.1:50052", proxy: &url.URL{Scheme: "http", Host: "proxy:8080"}},
		{name: "invalid proxy remains an error", address: "192.0.2.1:50052", err: errors.New("invalid proxy")},
		{name: "IPv4 link local bypasses proxy", address: "169.254.1.2:50052", proxy: &url.URL{Scheme: "http", Host: "proxy:8080"}, direct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubAgentProxy(t, func(req *http.Request) (*url.URL, error) {
				if req.URL.Scheme != "https" || req.URL.Host != tc.address {
					t.Errorf("proxy lookup URL = %v", req.URL)
				}
				if tc.address == "169.254.1.2:50052" {
					t.Error("link-local target consulted the proxy")
				}
				return tc.proxy, tc.err
			})
			opts, state := agentTCPDialOptions(tc.address)
			if got := len(opts) > 0 && state != nil; got != tc.direct {
				t.Fatalf("direct dial options = %v, want %v", got, tc.direct)
			}
		})
	}
}

func TestTLSConnectionReportsActualTCPFailure(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	stubAgentProxy(t, func(*http.Request) (*url.URL, error) { return nil, nil })
	wantErr := errors.New("TCP route is unreachable")
	stubLinkLocalDial(t, func(context.Context, string) (net.Conn, error) { return nil, wantErr })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := ConnectWithTLSExpecting(ctx, "192.0.2.1:50052", testCertInfo(t), nil, &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"})
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
	stubAgentProxy(t, func(*http.Request) (*url.URL, error) { return nil, nil })
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
	conn, err := ConnectWithTLSExpecting(ctx, "192.0.2.1:50052", testCertInfo(t), nil, &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"})
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
	stubAgentProxy(t, func(*http.Request) (*url.URL, error) { return nil, nil })
	stubLinkLocalDial(t, func(context.Context, string) (net.Conn, error) {
		t.Error("default TCP dialer overrode caller option")
		return nil, net.ErrClosed
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := ConnectWithTLSExpecting(ctx, "192.0.2.1:50052", testCertInfo(t), nil, &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"},
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
