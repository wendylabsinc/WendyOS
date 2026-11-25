package netmesh

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// The production mTLS server factory accepts the admitted management stream,
// preserving its actual ML-DSA primary peer/TLSInfo and mandatory tenant check.
// The HTTP/2 RPC opens no second TLS connection inside the carrier.
func TestProductionAgentRPCUsesOriginalOperatorCarrierTLS(t *testing.T) {
	var authority fixtureAuthority
	credentials := fixtureCredentials(t, &authority)
	device, operator := credentials[0], credentials[1]
	key, err := x509.MarshalPKCS8PrivateKey(device.Signer)
	if err != nil {
		t.Fatal(err)
	}
	server, err := mtls.NewServer(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: device.Certificate.Certificate[0]})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.certificate.Raw})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})), zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: device.Device.Tenant().String()}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	service := &normalOperatorAgent{principal: operator.Device.Principal(), address: "127.0.0.1"}
	agentpb.RegisterWendyAgentServiceServer(server, service)
	ingress := meshoperatorlink.NewAgentListener()
	grpcDone := make(chan error, 1)
	go func() { grpcDone <- server.Serve(ingress) }()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		server.Stop()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	serverDone := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		server.Stop()
		_ = ingress.Close()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Error("carrier server did not stop")
		}
		select {
		case <-grpcDone:
		case <-time.After(3 * time.Second):
			t.Error("agent ingress did not stop")
		}
	})
	serverConfig, err := device.PeerTLS(operator.Device)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig.NextProtos = []string{meshoperatorlink.RouterALPN}
	serverConfig.CurvePreferences = []tls.CurveID{tls.X25519}
	var handshakes atomic.Int32
	serverConfig.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { handshakes.Add(1); return nil, nil }
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		defer raw.Close()
		secure := tls.Server(raw, serverConfig)
		if err = secure.HandshakeContext(ctx); err != nil {
			serverDone <- err
			return
		}
		link, err := meshoperatorlink.AcceptRouter(ctx, secure, device.Device.Tenant(), []*x509.Certificate{authority.certificate}, ingress.Deliver)
		if err != nil {
			serverDone <- err
			return
		}
		<-link.Done()
		link.Wait()
		serverDone <- nil
	}()
	identity, err := meshoperator.VerifyPrimary(operator.Certificate.Certificate, []*x509.Certificate{authority.certificate}, device.Device.Tenant(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	config := meshoperatorlink.ClientConfig{Identity: identity, Signer: operator.Signer, Instance: meshoperator.InstanceID(operator.Device.UUID()), Router: device.Device, Roots: []*x509.Certificate{authority.certificate}}
	tlsConfig, err := meshoperatorlink.ClientTLSConfig(config, operator.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.CurvePreferences = []tls.CurveID{tls.X25519}
	raw, err := (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	link, err := meshoperatorlink.RouterClient(ctx, raw.(*tls.Conn), config)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer func() { _ = link.Close(); link.Wait() }()
	rpc, err := grpc.NewClient("passthrough:///operator-agent", grpc.WithTransportCredentials(meshoperatorlink.AgentTransportCredentials(nil)), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return link.OpenAgent(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	reply, err := agentpb.NewWendyAgentServiceClient(rpc).GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal("production same-TLS agent RPC:", err)
	}
	if reply.Version != "full-operator-router" || !service.called.Load() || handshakes.Load() != 1 {
		t.Fatal("agent RPC lost primary identity or repeated TLS", handshakes.Load())
	}
	if !link.Live() {
		t.Fatal("management RPC retired routing carrier")
	}
}
