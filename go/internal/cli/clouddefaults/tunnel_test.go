package clouddefaults

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

func tunnelHealthServer(t *testing.T) (*bufconn.Listener, *tls.Config) {
	t.Helper()
	clientInfo, ca := testCertInfo(t)
	serverPEM, keyPEM := ca.leafPEM(t, "agent", 3, x509.ExtKeyUsageServerAuth)
	serverCert, err := tls.X509KeyPair([]byte(serverPEM), []byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair([]byte(clientInfo.PemCertificate), []byte(clientInfo.PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots,
		MinVersion:   tls.VersionTLS12,
	})))
	healthpb.RegisterHealthServer(server, health.NewServer())
	listener := bufconn.Listen(1024 * 1024)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	verify, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{ChainPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	return listener, &tls.Config{
		Certificates:       []tls.Certificate{clientCert},
		InsecureSkipVerify: true, // VerifyConnection checks the CA, as on an agent tunnel.
		VerifyConnection:   verify,
		MinVersion:         tls.VersionTLS12,
	}
}

func TestTunnelDialerReconnects(t *testing.T) {
	for _, failHandshake := range []bool{true, false} {
		name := "established connection drops"
		if failHandshake {
			name = "first TLS handshake fails"
		}
		t.Run(name, func(t *testing.T) {
			listener, tlsConfig := tunnelHealthServer(t)
			var attempts atomic.Int32
			first := make(chan net.Conn, 1)
			streams := make(chan context.Context, 2)
			dialOpt := TunnelDialer(func(ctx context.Context) (net.Conn, error) {
				attempt := attempts.Add(1)
				if attempt <= 2 {
					streams <- ctx
				}
				if failHandshake && attempt == 1 {
					local, remote := net.Pipe()
					remote.Close()
					return local, nil
				}
				conn, err := listener.DialContext(ctx)
				if err == nil {
					// Model the broker stream: cancelling its context kills the
					// connection, even if its TLS handshake already succeeded.
					context.AfterFunc(ctx, func() { conn.Close() })
					if attempt == 1 {
						first <- conn
					}
				}
				return conn, err
			})
			conn, err := grpc.NewClient("passthrough:///cloud-tunnel", dialOpt,
				grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
				grpc.WithConnectParams(grpc.ConnectParams{
					Backoff:           backoff.Config{BaseDelay: time.Millisecond, Multiplier: 1, MaxDelay: time.Millisecond},
					MinConnectTimeout: time.Second,
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := healthpb.NewHealthClient(conn)
			check := func() {
				t.Helper()
				if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true)); err != nil {
					t.Fatalf("RPC did not recover after %d tunnel opens: %v", attempts.Load(), err)
				}
			}
			check()
			if !failHandshake {
				(<-first).Close()
				for conn.GetState() == connectivity.Ready {
					if !conn.WaitForStateChange(ctx, connectivity.Ready) {
						t.Fatal("gRPC did not observe the tunnel closing")
					}
				}
				check()
			}
			if attempts.Load() < 2 {
				t.Fatalf("opened %d tunnels; want a new tunnel on reconnect", attempts.Load())
			}
			conn.Close()
			for range 2 {
				if (<-streams).Err() != context.Canceled {
					t.Fatal("closed gRPC connection left a broker stream alive")
				}
			}
		})
	}
}

func TestDialTunnelLifetime(t *testing.T) {
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "dial metadata"))
	defer cancel()
	local, remote := net.Pipe()
	defer remote.Close()
	var streamCtx context.Context
	conn, err := dialTunnel(ctx, func(ctx context.Context) (net.Conn, error) {
		streamCtx = ctx
		return local, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	if streamCtx.Value(contextKey{}) != "dial metadata" {
		t.Fatal("tunnel lost dial context values")
	}
	select {
	case <-streamCtx.Done():
		t.Fatal("returned tunnel died with the dial context")
	case <-time.After(50 * time.Millisecond):
	}
	conn.Close()
	if streamCtx.Err() != context.Canceled {
		t.Fatal("closing the connection did not cancel the broker stream")
	}
}

func TestDialTunnelCancellationDuringOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := dialTunnel(ctx, func(streamCtx context.Context) (net.Conn, error) {
		cancel()
		select {
		case <-streamCtx.Done():
			return nil, streamCtx.Err()
		case <-time.After(time.Second):
			t.Fatal("abandoned dial left its broker stream alive")
			return nil, nil
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestDialTunnelCleansUpFailedOpen(t *testing.T) {
	want := errors.New("broker unavailable")
	var streamCtx context.Context
	_, err := dialTunnel(context.Background(), func(ctx context.Context) (net.Conn, error) {
		streamCtx = ctx
		return nil, want
	})
	if !errors.Is(err, want) || streamCtx.Err() != context.Canceled {
		t.Fatalf("err = %v, stream context err = %v", err, streamCtx.Err())
	}
}

func TestDialTunnelAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dialTunnel(ctx, func(context.Context) (net.Conn, error) {
		t.Fatal("opened a broker stream for an already cancelled dial")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestDialTunnelCancellationAtHandoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	conn, err := dialTunnel(ctx, func(context.Context) (net.Conn, error) {
		cancel()
		return local, nil
	})
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got (%v, %v), want (nil, context.Canceled)", conn, err)
	}
	if err := local.SetDeadline(time.Now()); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("cancelled handoff leaked its connection: SetDeadline returned %v", err)
	}
}
