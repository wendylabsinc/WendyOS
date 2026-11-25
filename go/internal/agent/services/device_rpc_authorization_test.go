package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshappstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/rpcauth"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// These fixtures use ephemeral loopback TLS and fake backends only. In
// particular, the production HostShell service never receives a real spawner.
type rpcPolicyBackend struct {
	agentpb.UnimplementedWendyContainerServiceServer
	mu    sync.Mutex
	calls map[string]int
}

func (b *rpcPolicyBackend) called(method string) { b.mu.Lock(); defer b.mu.Unlock(); b.calls[method]++ }
func (b *rpcPolicyBackend) count(method string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[method]
}
func (b *rpcPolicyBackend) QueryChunks(context.Context, *agentpb.QueryChunksRequest) (*agentpb.QueryChunksResponse, error) {
	b.called("QueryChunks")
	return &agentpb.QueryChunksResponse{}, nil
}
func (b *rpcPolicyBackend) QueryLayers(context.Context, *agentpb.QueryLayersRequest) (*agentpb.QueryLayersResponse, error) {
	b.called("QueryLayers")
	return &agentpb.QueryLayersResponse{}, nil
}
func (b *rpcPolicyBackend) PrepareImage(context.Context, *agentpb.RunContainerLayersRequest) (*agentpb.PrepareImageResponse, error) {
	b.called("PrepareImage")
	return &agentpb.PrepareImageResponse{}, nil
}
func (b *rpcPolicyBackend) WriteChunks(s grpc.ClientStreamingServer[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]) error {
	b.called("WriteChunks")
	for {
		_, err := s.Recv()
		if err == io.EOF {
			return s.SendAndClose(&agentpb.WriteChunksResponse{})
		}
		if err != nil {
			return err
		}
	}
}
func (b *rpcPolicyBackend) CreateContainer(context.Context, *agentpb.CreateContainerRequest) (*agentpb.CreateContainerResponse, error) {
	b.called("CreateContainer")
	return &agentpb.CreateContainerResponse{}, nil
}
func (b *rpcPolicyBackend) RunContainer(_ *agentpb.RunContainerLayersRequest, _ grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]) error {
	b.called("RunContainer")
	return nil
}
func (b *rpcPolicyBackend) ExecContainer(_ grpc.BidiStreamingServer[agentpb.ExecContainerRequest, agentpb.ExecContainerResponse]) error {
	b.called("ExecContainer")
	return nil
}

type rpcPolicySensors struct {
	agentpbv2.UnimplementedWendySensorServiceServer
	manifest, stream atomic.Int32
}

func (s *rpcPolicySensors) GetSensorManifest(context.Context, *agentpbv2.GetSensorManifestRequest) (*sensorlinkpb.SensorManifest, error) {
	s.manifest.Add(1)
	return &sensorlinkpb.SensorManifest{}, nil
}
func (s *rpcPolicySensors) StreamSensors(_ *agentpbv2.StreamSensorsRequest, stream grpc.ServerStreamingServer[sensorlinkpb.SensorData]) error {
	s.stream.Add(1)
	return stream.Send(&sensorlinkpb.SensorData{})
}

type rpcPolicyFakeShell struct{ calls atomic.Int32 }

func (s *rpcPolicyFakeShell) Run(_ context.Context, _ []string, _ io.Reader, out io.Writer, _ <-chan [2]uint32) (int, error) {
	s.calls.Add(1)
	_, err := out.Write([]byte("FAKE_SPAWNER_ONLY"))
	return 42, err
}

// Register genuinely new unary and stream methods: successful operator calls
// prove they exist, while device calls must be denied before their handlers.
type rpcPolicyFuture interface{}

func registerRPCPolicyFuture(s *grpc.Server, calls *atomic.Int32) {
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.FutureAdmin", HandlerType: (*rpcPolicyFuture)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Do", Handler: func(srv any, ctx context.Context, dec func(any) error, next grpc.UnaryServerInterceptor) (any, error) {
			req := &emptypb.Empty{}
			if err := dec(req); err != nil {
				return nil, err
			}
			handler := func(context.Context, any) (any, error) { calls.Add(1); return &emptypb.Empty{}, nil }
			if next == nil {
				return handler(ctx, req)
			}
			return next(ctx, req, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.FutureAdmin/Do"}, handler)
		}}},
		Streams: []grpc.StreamDesc{{StreamName: "DoStream", ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
			calls.Add(1)
			return stream.SendMsg(&emptypb.Empty{})
		}}},
	}, struct{}{})
}

func newRPCPolicyIssuer(t *testing.T) (string, func(int64, string, string) (string, string)) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ephemeral RPC policy test CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	issue := func(serial int64, cn, principal string) (string, string) {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{Subject: pkix.Name{CommonName: cn}, SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: nil}
		if principal != "" {
			leaf.URIs = []*url.URL{uri}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	}
	return rootPEM, issue
}

func TestDeviceRPCPolicyOverProductionMTLS(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const serverID = "22222222-2222-4222-8222-222222222222"
	rootPEM, issueWithCN := newRPCPolicyIssuer(t)
	issue := func(serial int64, principal string) (string, string) { return issueWithCN(serial, "", principal) }
	serverCert, serverKey := issue(2, "spiffe://wendy.sh/tenant/"+tenant+"/device/"+serverID)
	var unaryExtras, streamExtras atomic.Int32
	srv, err := mtls.NewServer(serverCert, rootPEM, serverKey, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: tenant}, interceptor.OrgModeStrict,
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			unaryExtras.Add(1)
			return next(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
			streamExtras.Add(1)
			return next(srv, stream)
		}))
	if err != nil {
		t.Fatal(err)
	}
	backend := &rpcPolicyBackend{calls: make(map[string]int)}
	ingress := meshingress.NewRegistry()
	mesh := NewMeshService(zap.NewNop(), t.TempDir(), certs.Scope{TenantUUID: tenant})
	mesh.SetIngressAuthorizer(ingress)
	agentpbv2.RegisterWendyMeshServiceServer(srv, mesh)
	var meshDials atomic.Int32
	mesh.dialLocal = func(address string, _ time.Duration) (net.Conn, error) {
		meshDials.Add(1)
		if address != "10.3.0.2:8080" {
			t.Errorf("MeshDial reached unexpected container endpoint %q", address)
		}
		client, app := net.Pipe()
		go func() {
			defer app.Close()
			_, _ = io.Copy(app, app)
		}()
		return client, nil
	}
	sensors := &rpcPolicySensors{}
	shell := &rpcPolicyFakeShell{}
	var future atomic.Int32
	agentpb.RegisterWendyContainerServiceServer(srv, backend)
	agentpbv2.RegisterWendySensorServiceServer(srv, sensors)
	agentpb.RegisterWendyShellServiceServer(srv, NewShellService(zap.NewNop(), shell))
	registerRPCPolicyFuture(srv, &future)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	go func() { _ = srv.Serve(lis) }()
	tenantID, err := meshid.ParseTenant(tenant)
	if err != nil {
		t.Fatal(err)
	}
	want, err := meshid.ParseDevice(tenantID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	connect := func(serial int64, principal string) *grpc.ClientConn {
		t.Helper()
		cert, key := issue(serial, principal)
		cfg, err := mtls.NewClientTLSConfigExpectingDevice(cert, rootPEM, key, zap.NewNop(), want)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ClientSessionCache = nil
		cfg.MinVersion = tls.VersionTLS13
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	device := connect(3, "spiffe://wendy.sh/tenant/"+tenant+"/device/33333333-3333-4333-8333-333333333333")
	if err := ingress.Claim("running-app", 8080, "10.3.0.2", 8080); err != nil {
		t.Fatal(err)
	}
	app, err := meshappstream.Dial(ctx, device, 8080)
	if err != nil {
		t.Fatalf("device could not reach a published app through MeshDial: %v", err)
	}
	if err := app.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	const message = "device-app-stream"
	if _, err := app.Write([]byte(message)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len(message))
	if _, err := io.ReadFull(app, reply); err != nil || string(reply) != message {
		t.Fatalf("MeshDial app reply=%q err=%v", reply, err)
	}
	app.Close()
	ingress.Release("running-app")
	for _, port := range []uint16{8080, 50052} {
		conn, err := meshappstream.Dial(ctx, device, port)
		if conn != nil {
			conn.Close()
		}
		if status.Code(err) != codes.PermissionDenied || rpcauth.IsIdentityDenied(err) {
			t.Fatalf("unpublished port %d must be refused by app ingress, got %v", port, err)
		}
	}
	if meshDials.Load() != 1 {
		t.Fatalf("unpublished ports reached a destination: %d dials", meshDials.Load())
	}
	client := agentpb.NewWendyContainerServiceClient(device)
	if _, err := client.QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryLayers(ctx, &agentpb.QueryLayersRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PrepareImage(ctx, &agentpb.RunContainerLayersRequest{}); err != nil {
		t.Fatal(err)
	}
	write, err := client.WriteChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := write.Send(&agentpb.WriteChunksRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := write.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"QueryChunks", "QueryLayers", "PrepareImage", "WriteChunks"} {
		if backend.count(method) != 1 {
			t.Errorf("%s backend calls=%d", method, backend.count(method))
		}
	}
	sensorClient := agentpbv2.NewWendySensorServiceClient(device)
	if _, err := sensorClient.GetSensorManifest(ctx, &agentpbv2.GetSensorManifestRequest{}); err != nil {
		t.Fatal(err)
	}
	sensorStream, err := sensorClient.StreamSensors(ctx, &agentpbv2.StreamSensorsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sensorStream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := sensorStream.Recv(); err != io.EOF {
		t.Fatalf("sensor stream end: %v", err)
	}
	if sensors.manifest.Load() != 1 || sensors.stream.Load() != 1 {
		t.Fatal("sensor backends not reached")
	}

	unaryBefore, streamBefore := unaryExtras.Load(), streamExtras.Load()
	denied := func(err error) {
		t.Helper()
		if status.Code(err) != codes.PermissionDenied || !rpcauth.IsIdentityDenied(err) {
			t.Fatalf("want PermissionDenied with RPC identity reason, got %v", err)
		}
	}
	_, err = client.CreateContainer(ctx, &agentpb.CreateContainerRequest{})
	denied(err)
	run, err := client.RunContainer(ctx, &agentpb.RunContainerLayersRequest{})
	if err == nil {
		_, err = run.Recv()
	}
	denied(err)
	exec, err := client.ExecContainer(ctx)
	if err == nil {
		_ = exec.CloseSend()
		_, err = exec.Recv()
	}
	denied(err)
	hostShell := func(conn *grpc.ClientConn) ([]byte, int32, error) {
		stream, err := agentpb.NewWendyShellServiceClient(conn).HostShell(ctx)
		if err != nil {
			return nil, -1, err
		}
		if err := stream.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_Start_{Start: &agentpb.HostShellRequest_Start{Command: []string{"fake-only"}}}}); err != nil && err != io.EOF {
			return nil, -1, err
		}
		_ = stream.CloseSend()
		var output []byte
		exit := int32(-1)
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return output, exit, nil
			}
			if err != nil {
				return output, exit, err
			}
			output = append(output, msg.GetStdoutData()...)
			if _, ok := msg.ResponseType.(*agentpb.HostShellResponse_ExitCode); ok {
				exit = msg.GetExitCode()
			}
		}
	}
	_, _, err = hostShell(device)
	denied(err)
	if shell.calls.Load() != 0 {
		t.Fatal("device reached production host shell spawner")
	}
	for _, method := range []string{"CreateContainer", "RunContainer", "ExecContainer"} {
		if backend.count(method) != 0 {
			t.Fatalf("device reached %s backend", method)
		}
	}
	denied(device.Invoke(ctx, "/test.FutureAdmin/Do", &emptypb.Empty{}, &emptypb.Empty{}))
	futureStream, err := device.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/test.FutureAdmin/DoStream")
	if err == nil {
		_ = futureStream.CloseSend()
		err = futureStream.RecvMsg(&emptypb.Empty{})
	}
	denied(err)
	if future.Load() != 0 {
		t.Fatal("device reached newly registered RPC")
	}
	if unaryExtras.Load() != unaryBefore || streamExtras.Load() != streamBefore {
		t.Fatal("denied RPC reached caller-provided interceptor")
	}

	operator := connect(4, "spiffe://wendy.sh/tenant/"+tenant+"/operator/alice")
	output, exit, err := hostShell(operator)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "FAKE_SPAWNER_ONLY" || exit != 42 || shell.calls.Load() != 1 {
		t.Fatalf("operator fake shell output=%q exit=%d calls=%d", output, exit, shell.calls.Load())
	}
	if _, err := agentpb.NewWendyContainerServiceClient(operator).CreateContainer(ctx, &agentpb.CreateContainerRequest{}); err != nil {
		t.Fatal(err)
	}
	if backend.count("CreateContainer") != 1 {
		t.Fatal("operator did not reach privileged unary backend")
	}
	if err := operator.Invoke(ctx, "/test.FutureAdmin/Do", &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	opStream, err := operator.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/test.FutureAdmin/DoStream")
	if err != nil {
		t.Fatal(err)
	}
	_ = opStream.CloseSend()
	if err := opStream.RecvMsg(&emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	if err := opStream.RecvMsg(&emptypb.Empty{}); err != io.EOF {
		t.Fatalf("future stream end: %v", err)
	}
	if future.Load() != 2 {
		t.Fatal("operator did not reach new RPCs")
	}

	foreign := connect(5, "spiffe://wendy.sh/tenant/44444444-4444-4444-8444-444444444444/device/foreign")
	if _, err := agentpb.NewWendyContainerServiceClient(foreign).QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err == nil {
		t.Fatal("foreign-tenant device admitted")
	}
	if backend.count("QueryChunks") != 1 || shell.calls.Load() != 1 {
		t.Fatal("foreign tenant reached backend")
	}
}

// Use an old-chain server identity: direct-PKI Go listeners already require a
// tenant SAN at TLS verification and must not be weakened by this exception.
func TestLegacyUserRPCPolicyOverProductionMTLS(t *testing.T) {
	rootPEM, issue := newRPCPolicyIssuer(t)
	serverCert, serverKey := issue(2, "sh/wendy/7/12", "")
	for _, configured := range []string{"", "strict", "grace", "off"} {
		t.Run("mode="+configured, func(t *testing.T) {
			mode, _ := interceptor.ParseOrgMode(configured)
			srv, err := mtls.NewServer(serverCert, rootPEM, serverKey, zap.NewNop(), time.Time{}, certs.Scope{OrgID: 7}, mode)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			registerRPCPolicyFuture(srv, &calls)
			backend := &rpcPolicyBackend{calls: make(map[string]int)}
			agentpb.RegisterWendyContainerServiceServer(srv, backend)
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Stop()
			defer lis.Close()
			go func() { _ = srv.Serve(lis) }()
			for i, caller := range []struct {
				name, cn, uri  string
				legacy, device bool
			}{
				{name: "legacy user", cn: "wendy/user/human", legacy: true},
				{name: "empty user", cn: "wendy/user/"},
				{name: "unknown", cn: "unknown"},
				{name: "device with user CN", cn: "wendy/user/human", uri: "urn:wendy:org:7:asset:12", device: true},
				{name: "foreign device with user CN", cn: "wendy/user/human", uri: "urn:wendy:org:8:asset:12"},
				{name: "malformed claim with user CN", cn: "wendy/user/human", uri: "urn:wendy:org:0:user:human"},
				{name: "operator", cn: "operator", uri: "urn:wendy:org:7:user:human"},
			} {
				t.Run(caller.name, func(t *testing.T) {
					certPEM, keyPEM := issue(int64(i+3), caller.cn, caller.uri)
					cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
					if err != nil {
						t.Fatal(err)
					}
					roots := x509.NewCertPool()
					roots.AppendCertsFromPEM([]byte(rootPEM))
					cfg := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: roots, MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
					// Identity certificates have no DNS SAN. Verify the server chain explicitly.
					cfg.VerifyConnection = func(cs tls.ConnectionState) error {
						_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots})
						return err
					}
					conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					before := calls.Load()
					allowed := caller.name == "operator" || caller.legacy && mode == interceptor.OrgModeGrace
					check := func(err error) {
						t.Helper()
						if allowed && err != nil {
							t.Fatalf("legacy/operator RPC refused: %v", err)
						}
						if !allowed && status.Code(err) != codes.PermissionDenied {
							t.Fatalf("want PermissionDenied, got %v", err)
						}
					}
					check(conn.Invoke(ctx, "/test.FutureAdmin/Do", &emptypb.Empty{}, &emptypb.Empty{}))
					stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/test.FutureAdmin/DoStream")
					if err == nil {
						_ = stream.CloseSend()
						err = stream.RecvMsg(&emptypb.Empty{})
					}
					check(err)
					want := before
					if allowed {
						want += 2
					}
					if got := calls.Load(); got != want {
						t.Fatalf("management handler calls=%d; want %d", got, want)
					}
					if caller.device {
						if _, err := agentpb.NewWendyContainerServiceClient(conn).QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
							t.Fatalf("same-tenant device staging refused in %s: %v", mode, err)
						}
					}
				})
			}
		})
	}
}
