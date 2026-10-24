package grpcclient_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestCustomDeviceRoutesPreserveTargetsAndTLS(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	pki := newIntegrationPKI(t)
	server, err := mtls.NewServer(pki.serverCertPEM, pki.caPEM, pki.serverKeyPEM,
		zap.NewNop(), time.Time{}, certs.Scope{}, interceptor.OrgModeOff)
	if err != nil {
		t.Fatal(err)
	}
	agentpb.RegisterWendyAgentServiceServer(server, &versionService{})
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client := &config.CertificateInfo{
		PemCertificate: pki.clientCertPEM, PemPrivateKey: pki.clientKeyPEM, PemCertificateChain: pki.caPEM,
	}
	for _, tc := range []struct{ target, dialed string }{
		{"passthrough:///cloud-device", "cloud-device"},
		{"passthrough:///wasm-agent.test:443", "wasm-agent.test:443"},
		{"fe80::1%en0:50052", "[fe80::1%en0]:50052"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dialed := make(chan string, 1)
			conn, err := grpcclient.ConnectWithTLSExpecting(ctx, tc.target, client, nil, nil,
				grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
					select {
					case dialed <- address:
					default:
					}
					return listener.DialContext(ctx)
				}))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
				t.Fatal(err)
			}
			if got := <-dialed; got != tc.dialed {
				t.Fatalf("dialed %q, want %q", got, tc.dialed)
			}
			if conn.Addr != tc.target || !conn.IsMTLS {
				t.Fatalf("connection lost original target or TLS: addr=%q mTLS=%v", conn.Addr, conn.IsMTLS)
			}
		})
	}
}
