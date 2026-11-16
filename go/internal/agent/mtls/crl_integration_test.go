package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const crlTenant = "11111111-1111-4111-8111-111111111111"
const crlPrincipal = "spiffe://wendy.sh/tenant/" + crlTenant + "/"

var crlRPCMethods = []struct {
	method string
	stream bool
}{
	{agentpb.WendyAgentService_GetAgentVersion_FullMethodName, false},
	{agentpb.WendyContainerService_QueryLayers_FullMethodName, false},
	{agentpb.WendyProvisioningService_Unprovision_FullMethodName, false},
	{agentpbv2.WendyProvisioningService_Unprovision_FullMethodName, false},
	{agentpb.WendyAgentService_UpdateOS_FullMethodName, true},
	{agentpbv2.WendyOSUpdateService_UpdateOS_FullMethodName, true},
	{agentpb.WendyAgentService_UpdateAgent_FullMethodName, true},
	{agentpbv2.WendyAgentUpdateService_UpdateAgent_FullMethodName, true},
	{agentpbv2.WendyAgentUpdateService_StartOnlineUpdate_FullMethodName, false},
	{agentpb.WendyContainerService_RemoveVolume_FullMethodName, false},
	{agentpbv2.WendyContainerService_RemoveVolume_FullMethodName, false},
	{agentpbv2.DataService_DeleteEpisode_FullMethodName, false},
	{"/regression.FutureService/NewMethod", false},
	{"/regression.FutureService/NewStream", true},
}

// The real TLS server and its mandatory interceptors run against inert handlers
// registered under the actual method names. No update or deletion is performed.
type crlRPCServer struct {
	roots   *x509.CertPool
	address string
	calls   atomic.Int64
	states  chan tls.ConnectionState
}

func startCRLRPCServer(t *testing.T, p *crltest.PKI, policy certs.CertificateTimePolicy) *crlRPCServer {
	t.Helper()
	own := p.Issue(t, 10, crlPrincipal+"device/22222222-2222-4222-8222-222222222222")
	server, err := NewServerWithTimePolicy(own.PEM, p.PEM, own.KeyPEM, zap.NewNop(), policy, certs.Scope{TenantUUID: crlTenant}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(p.CA)
	fixture := &crlRPCServer{roots: roots, states: make(chan tls.ConnectionState, 64)}
	entered := func(ctx context.Context) {
		fixture.calls.Add(1)
		p, _ := peer.FromContext(ctx)
		select {
		case fixture.states <- p.AuthInfo.(credentials.TLSInfo).State:
		default:
		}
	}
	services := map[string]*grpc.ServiceDesc{}
	for _, method := range crlRPCMethods {
		parts := strings.Split(method.method, "/")
		desc := services[parts[1]]
		if desc == nil {
			desc = &grpc.ServiceDesc{ServiceName: parts[1], HandlerType: (*any)(nil)}
			services[parts[1]] = desc
		}
		if method.stream {
			desc.Streams = append(desc.Streams, grpc.StreamDesc{StreamName: parts[2], ServerStreams: true, ClientStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				entered(stream.Context())
				for {
					request := new(emptypb.Empty)
					if err := stream.RecvMsg(request); err != nil {
						return err
					}
					if err := stream.SendMsg(request); err != nil {
						return err
					}
				}
			}})
		} else {
			desc.Methods = append(desc.Methods, grpc.MethodDesc{MethodName: parts[2], Handler: func(srv any, ctx context.Context, decode func(any) error, intercept grpc.UnaryServerInterceptor) (any, error) {
				request := new(emptypb.Empty)
				if err := decode(request); err != nil {
					return nil, err
				}
				handler := func(ctx context.Context, _ any) (any, error) { entered(ctx); return new(emptypb.Empty), nil }
				if intercept == nil {
					return handler(ctx, request)
				}
				return intercept(ctx, request, &grpc.UnaryServerInfo{Server: srv, FullMethod: method.method}, handler)
			}})
		}
	}
	for _, desc := range services {
		server.RegisterService(desc, fixture)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.address = listener.Addr().String()
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return fixture
}

func useCRLManager(t *testing.T, p *crltest.PKI, grace time.Duration) *revocation.Manager {
	t.Helper()
	previous := revocation.Default
	manager := revocation.New(p.Client)
	if err := manager.Configure(revocation.Config{Grace: grace}); err != nil {
		t.Fatal(err)
	}
	revocation.Default = manager
	t.Cleanup(func() { manager.Refresh(context.Background()); revocation.Default = previous })
	return manager
}

func (s *crlRPCServer) connect(t *testing.T, certificate tls.Certificate, cache tls.ClientSessionCache) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(s.address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: s.roots, ServerName: "localhost", MinVersion: tls.VersionTLS13, ClientSessionCache: cache})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func callCRLRPC(ctx context.Context, conn *grpc.ClientConn, method string, stream bool) error {
	if !stream {
		return conn.Invoke(ctx, method, new(emptypb.Empty), new(emptypb.Empty))
	}
	s, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, method)
	if err != nil {
		return err
	}
	_ = s.SendMsg(new(emptypb.Empty))
	err = s.RecvMsg(new(emptypb.Empty))
	_ = s.CloseSend()
	return err
}

func TestEveryOperatorRPCRequiresFreshEvidence(t *testing.T) {
	for _, grace := range []time.Duration{-1, 2 * time.Hour} {
		t.Run(grace.String(), func(t *testing.T) {
			p := crltest.New(t, true)
			m := useCRLManager(t, p, grace)
			operator := p.Issue(t, 42, crlPrincipal+"operator/alice")
			server := startCRLRPCServer(t, p, certs.CertificateTimePolicy{})
			pair, err := certs.TLSKeyPair(operator.PEM, p.PEM, operator.KeyPEM)
			if err != nil {
				t.Fatal(err)
			}
			m.Refresh(context.Background())
			conn := server.connect(t, pair, nil)
			run := func(want codes.Code) {
				for _, method := range crlRPCMethods {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					before := server.calls.Load()
					err := callCRLRPC(ctx, conn, method.method, method.stream)
					cancel()
					if status.Code(err) != want {
						t.Fatalf("%s: %v, want %s", method.method, err, want)
					}
					if want != codes.OK && server.calls.Load() != before {
						t.Fatalf("refused call reached handler: %s", method.method)
					}
				}
			}
			run(codes.OK)
			now := time.Now()
			p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: now.Add(-30 * time.Second), NextUpdate: now.Add(-time.Second)}), 200)
			m.Refresh(context.Background())
			want := codes.FailedPrecondition
			if grace < 0 {
				want = codes.OK
			}
			run(want)
			p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(3), ThisUpdate: now, NextUpdate: now.Add(time.Hour), RevokedCertificateEntries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: now}}}), 200)
			m.Refresh(context.Background())
			run(codes.Unauthenticated)
		})
	}
}

func TestExistingGRPCStreamSurvivesOutageButRejectsRevocation(t *testing.T) {
	p := crltest.New(t, true)
	m := useCRLManager(t, p, 2*time.Hour)
	operator := p.Issue(t, 42, crlPrincipal+"operator/alice")
	server := startCRLRPCServer(t, p, certs.CertificateTimePolicy{})
	pair, err := certs.TLSKeyPair(operator.PEM, p.PEM, operator.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	conn := server.connect(t, pair, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, "/regression.FutureService/NewStream")
	if err != nil {
		t.Fatal(err)
	}
	exchange := func() error { _ = stream.SendMsg(new(emptypb.Empty)); return stream.RecvMsg(new(emptypb.Empty)) }
	if err := exchange(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: now.Add(-30 * time.Second), NextUpdate: now.Add(-time.Second)}), 200)
	m.Refresh(context.Background())
	p.Respond(nil, 503)
	m.Refresh(context.Background())
	if err := exchange(); err != nil {
		t.Fatalf("outage interrupted admitted stream: %v", err)
	}
	p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(3), ThisUpdate: now, NextUpdate: now.Add(time.Hour), RevokedCertificateEntries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: now}}}), 200)
	m.Refresh(context.Background())
	if err := exchange(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked stream: %v", err)
	}
}

func TestCRLRejectsActualResumedTLSConnection(t *testing.T) {
	p := crltest.New(t, false)
	m := useCRLManager(t, p, -1)
	operator := p.Issue(t, 42, crlPrincipal+"operator/alice")
	server := startCRLRPCServer(t, p, certs.CertificateTimePolicy{})
	pair, err := certs.TLSKeyPair(operator.PEM, p.PEM, operator.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	cache := tls.NewLRUClientSessionCache(4)
	cfg := &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: server.roots, ServerName: "localhost", MinVersion: tls.VersionTLS13, ClientSessionCache: cache, NextProtos: []string{"h2"}}
	handshake := func() (bool, error) {
		raw, err := net.DialTimeout("tcp", server.address, time.Second)
		if err != nil {
			return false, err
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		conn := tls.Client(raw, cfg)
		err = conn.Handshake()
		if err == nil {
			_, err = conn.Read(make([]byte, 1))
		}
		return conn.ConnectionState().DidResume, err
	}
	if resumed, err := handshake(); err != nil || resumed {
		t.Fatalf("full handshake: resumed=%v err=%v", resumed, err)
	}
	if resumed, err := handshake(); err != nil || !resumed {
		t.Fatalf("resumed handshake: resumed=%v err=%v", resumed, err)
	}
	p.Publish(t, 2, 42)
	m.Refresh(context.Background())
	if resumed, err := handshake(); err == nil || !resumed {
		t.Fatalf("revoked resumed handshake: resumed=%v err=%v", resumed, err)
	}
}

func TestRPCFreshnessUsesTrustedClockAfterWallRollback(t *testing.T) {
	p := crltest.New(t, false)
	m := useCRLManager(t, p, 30*time.Second)
	operator := p.Issue(t, 42, crlPrincipal+"operator/alice")
	now := time.Now()
	var wall, floor atomic.Int64
	wall.Store(now.UnixNano())
	floor.Store(now.UnixNano())
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return time.Unix(0, wall.Load()) }, Floor: func() time.Time { return time.Unix(0, floor.Load()) }}
	server := startCRLRPCServer(t, p, policy)
	pair, err := certs.TLSKeyPair(operator.PEM, p.PEM, operator.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	conn := server.connect(t, pair, nil)
	call := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return callCRLRPC(ctx, conn, agentpb.WendyAgentService_GetAgentVersion_FullMethodName, false)
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}
	p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Minute)}), 200)
	m.Refresh(context.Background())
	floor.Store(now.Add(2 * time.Minute).UnixNano())
	wall.Store(now.Add(-30 * time.Minute).UnixNano())
	if err := call(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("rollback made expired CRL fresh: %v", err)
	}
	// New sessions also reject evidence beyond grace using the trusted clock.
	own := p.Issue(t, 10, crlPrincipal+"device/22222222-2222-4222-8222-222222222222")
	cfg, err := NewTLSConfigWithTimePolicy(own.PEM, p.PEM, own.KeyPEM, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{operator.Cert}}); !errors.Is(err, revocation.ErrUnavailable) {
		t.Fatalf("handshake ignored trusted time: %v", err)
	}
	if err := m.Check(operator.Cert, p.CA, now, revocation.Fresh); !errors.Is(err, revocation.ErrUnavailable) {
		t.Fatalf("older time restored freshness: %v", err)
	}
}

func TestDeviceRPCFreshness(t *testing.T) {
	p := crltest.New(t, false)
	m := useCRLManager(t, p, time.Hour)
	device := p.Issue(t, 43, crlPrincipal+"device/33333333-3333-4333-8333-333333333333")
	server := startCRLRPCServer(t, p, certs.CertificateTimePolicy{})
	pair, err := certs.TLSKeyPair(device.PEM, p.PEM, device.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	conn := server.connect(t, pair, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	method := agentpb.WendyContainerService_QueryLayers_FullMethodName
	if err := callCRLRPC(ctx, conn, method, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: now.Add(-30 * time.Second), NextUpdate: now.Add(-time.Second)}), 200)
	m.Refresh(context.Background())
	if err := callCRLRPC(ctx, conn, method, false); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("device RPC bypassed uniform freshness policy: %v", err)
	}
}
