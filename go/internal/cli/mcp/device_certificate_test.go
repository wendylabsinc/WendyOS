package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// certificateTestConn is a direct mTLS connection that is not Ready (gRPC
// dials lazily, so it stays Idle) and whose certificate expires at notAfter.
func certificateTestConn(t *testing.T, notAfter time.Time) *grpcclient.AgentConnection {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	conn := grpcclient.NewFromConn(cc)
	conn.Host, conn.Addr, conn.IsMTLS = "mesh-1.local", "mesh-1.local:50052", true
	conn.CertInfo = &config.CertificateInfo{PemCertificate: authTestCertPEM(t, notAfter)}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A long-running server must not keep redialling a device with a certificate
// that has expired: it reconnects with the login stored now, and when that
// fails it lets go of the connection instead of retrying the expired one.
func TestExpiredDeviceCertificateConnectionIsReplaced(t *testing.T) {
	now := time.Now()
	target := commandTarget{Device: "mesh-1.local:50051", Transport: "direct"}
	for _, tc := range []struct {
		name       string
		notAfter   time.Time
		connectErr error
		wantDials  int
		want       string // "old", "new" or "none"
	}{
		{"renewed login reconnects", now.Add(-time.Hour), nil, 1, "new"},
		{"no usable login lets go", now.Add(-time.Hour), errors.New("remote error: tls: bad certificate"), 1, "none"},
		{"valid certificate is left alone", now.Add(time.Hour), nil, 0, "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := certificateTestConn(t, tc.notAfter)
			renewed := certificateTestConn(t, now.Add(24*time.Hour))
			dials := 0
			s := New(&config.Config{}, func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
				dials++
				if address != target.Device {
					t.Fatalf("reconnected to %q, want the same target %q", address, target.Device)
				}
				if tc.connectErr != nil {
					return nil, tc.connectErr
				}
				return renewed, nil
			})
			s.setConnection(old, "direct", target)

			s.replaceExpiredDeviceConnection(context.Background(), now)

			if dials != tc.wantDials {
				t.Fatalf("dials = %d, want %d", dials, tc.wantDials)
			}
			conn, connType, gotTarget := s.connectionSnapshot()
			switch tc.want {
			case "old":
				if conn != old || old.Conn.GetState() == connectivity.Shutdown {
					t.Fatalf("a connection with a valid certificate was replaced")
				}
			case "new":
				if conn != renewed || connType != "direct" || gotTarget != target {
					t.Fatalf("conn=%p type=%q target=%+v, want the renewed connection to the same target", conn, connType, gotTarget)
				}
			case "none":
				if conn != nil {
					t.Fatalf("kept a connection that can only present an expired certificate")
				}
				diags := s.proxyDiagnostics()
				if len(diags) != 1 || !strings.Contains(diags[0].Error, "operator certificate for this connection expired at") {
					t.Fatalf("diagnostics = %+v", diags)
				}
			}
			if tc.want != "old" && old.Conn.GetState() != connectivity.Shutdown {
				t.Fatalf("the expired connection was not closed, so gRPC keeps redialling with it")
			}
		})
	}
}

// A connection a tool chose while the reconnect was in flight wins over it.
func TestExpiredDeviceCertificateReplacementYieldsToNewerConnection(t *testing.T) {
	now := time.Now()
	old := certificateTestConn(t, now.Add(-time.Hour))
	chosen := certificateTestConn(t, now.Add(time.Hour))
	replacement := certificateTestConn(t, now.Add(time.Hour))
	var s *mcpServer
	s = New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		s.setConnection(chosen, "direct", commandTarget{Device: "other:50051", Transport: "direct"})
		return replacement, nil
	})
	s.setConnection(old, "direct", commandTarget{Device: "mesh-1.local:50051", Transport: "direct"})

	s.replaceExpiredDeviceConnection(context.Background(), now)

	if conn, _, _ := s.connectionSnapshot(); conn != chosen {
		t.Fatalf("the tool's connection was replaced")
	}
	if replacement.Conn.GetState() != connectivity.Shutdown {
		t.Fatalf("the unused replacement was left open")
	}
}
