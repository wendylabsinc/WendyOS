package services

import (
	"context"
	"crypto/sha256"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/testutil/deliverypki"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type deliveryTestBackend struct {
	fakeContainerd
	source       delivery.Source
	mu           sync.Mutex
	images       map[string]string
	preparations atomic.Int32
	queries      atomic.Int32
	started      chan struct{}
	chunksReady  <-chan struct{}
}

func (b *deliveryTestBackend) PrepareImage(ctx context.Context, name string, _ []*agentpb.RunContainerLayerHeader, config []byte) error {
	b.preparations.Add(1)
	if b.started != nil {
		close(b.started)
	}
	if b.chunksReady != nil {
		select {
		case <-b.chunksReady:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return delivery.Commit(ctx, b.source, name, digest.FromBytes(config).String(), func() (string, error) { return b.ImageDigest(ctx, name) }, func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.images[name] = digest.FromBytes(config).String()
		return nil
	})
}
func (b *deliveryTestBackend) ImageDigest(_ context.Context, name string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.images[name], nil
}

type deliveryRPCLab struct {
	f                          *deliverypki.Fixture
	manager                    *delivery.Manager
	backend                    *deliveryTestBackend
	operator, builder, rotated *grpc.ClientConn
}

func newDeliveryRPCLab(t *testing.T) *deliveryRPCLab {
	t.Helper()
	f := deliverypki.New(t)
	m := f.Manager(t)
	backend := &deliveryTestBackend{source: m.Get, images: map[string]string{}}
	backend.missingFn = func(_ context.Context, hashes [][32]byte) ([][32]byte, error) {
		backend.queries.Add(1)
		return hashes, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := mtls.NewServer(f.Target.PEM, f.Chain, f.Target.Key, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: deliverypki.Tenant}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	agentpb.RegisterWendyContainerServiceServer(server, NewContainerService(zap.NewNop(), backend, WithDeliverySource(m.Get)))
	agentpbv2.RegisterWendyBuildDeliveryServiceServer(server, NewBuildDeliveryService(m.Get, backend))
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	connect := func(c deliverypki.Credential) *grpc.ClientConn {
		conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(f.ClientTLS(t, c))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	// Admission transports use the primary identity; the JWS carries a separate
	// entitlement-bearing operator certificate, as the real Cloud flow does.
	primary := f.Mint(t, deliverypki.OperatorPrincipal, nil)
	return &deliveryRPCLab{f, m, backend, connect(primary), connect(f.Builder), connect(f.OtherBuilder)}
}
func (l *deliveryRPCLab) admit(t *testing.T, c builddelivery.Claims) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := agentpbv2.NewWendyBuildDeliveryServiceClient(l.operator).AdmitDelivery(ctx, &agentpbv2.AdmitDeliveryRequest{AuthorizationJws: l.f.Sign(t, c)})
	if err != nil {
		t.Fatal(err)
	}
	return result.GetSession()
}
func grantContext(session string) context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(builddelivery.MetadataKey, session))
}

func TestChunkDeliveryRequiresAdmittedOperatorGrantOverMTLS(t *testing.T) {
	l := newDeliveryRPCLab(t)
	client := agentpb.NewWendyContainerServiceClient(l.builder)
	session := l.admit(t, l.f.Claims("lab/app:latest"))
	for _, tc := range []struct {
		name string
		ctx  context.Context
		conn *grpc.ClientConn
	}{{"no grant", context.Background(), l.builder}, {"forged grant", grantContext(strings.Repeat("A", 43)), l.builder}, {"wrong key", grantContext(session), l.rotated}, {"duplicate grant", metadata.NewOutgoingContext(context.Background(), metadata.Pairs(builddelivery.MetadataKey, session, builddelivery.MetadataKey, session)), l.builder}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(tc.ctx, 3*time.Second)
			defer cancel()
			_, err := agentpb.NewWendyContainerServiceClient(tc.conn).QueryChunks(ctx, &agentpb.QueryChunksRequest{})
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("query: %v", err)
			}
		})
	}
	if l.backend.queries.Load() != 0 {
		t.Fatal("denied query reached backend")
	}
	ctx, cancel := context.WithTimeout(grantContext(session), 5*time.Second)
	defer cancel()
	if _, err := client.QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryLayers(ctx, &agentpb.QueryLayersRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: "unrelated"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("grant elevated to app control: %v", err)
	}
	if _, err := agentpbv2.NewWendyBuildDeliveryServiceClient(l.builder).GetDeliveryReference(ctx, &agentpbv2.GetDeliveryReferenceRequest{Reference: "lab/app:latest"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("grant elevated to ambient inventory: %v", err)
	}
	if _, err := client.PrepareImage(ctx, &agentpb.RunContainerLayersRequest{ImageName: "localhost:5000/other:latest"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong reference: %v", err)
	}
	if l.backend.preparations.Load() != 0 {
		t.Fatal("denied prepare reached backend")
	}
	stream, err := client.WriteChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("one authorized chunk")
	hash := sha256.Sum256(data)
	if err = stream.Send(&agentpb.WriteChunksRequest{Hash: hash[:], Data: data}); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	if len(l.backend.stagedChunks) != 1 {
		t.Fatal("authorized chunk was not staged")
	}
	if _, err = client.PrepareImage(ctx, &agentpb.RunContainerLayersRequest{ImageName: "localhost:5000/lab/app:latest", ImageConfig: []byte("config")}); err != nil {
		t.Fatal(err)
	}
	result, err := agentpbv2.NewWendyBuildDeliveryServiceClient(l.builder).GetDeliveryStatus(ctx, &agentpbv2.GetDeliveryStatusRequest{Session: session})
	if err != nil || !result.GetCompleted() || result.GetOutputDigest() != digest.FromBytes([]byte("config")).String() {
		t.Fatalf("durable completion: %v %v", result, err)
	}
	if _, err = client.PrepareImage(ctx, &agentpb.RunContainerLayersRequest{ImageName: "localhost:5000/lab/app:latest", ImageConfig: []byte("other config")}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("completed grant reopened preparation: %v", err)
	}
	if l.backend.preparations.Load() != 1 {
		t.Fatal("completion replay mutated backend")
	}
}

func TestPrepareAndChunkUploadShareOneGrantWithoutSerializingAssembly(t *testing.T) {
	l := newDeliveryRPCLab(t)
	session := l.admit(t, l.f.Claims("pipeline:latest"))
	ready := make(chan struct{})
	started := make(chan struct{})
	l.backend.chunksReady = ready
	l.backend.started = started
	var once sync.Once
	l.backend.stageChunkFn = func(context.Context, [32]byte, []byte) error { once.Do(func() { close(ready) }); return nil }
	ctx, cancel := context.WithTimeout(grantContext(session), 5*time.Second)
	defer cancel()
	client := agentpb.NewWendyContainerServiceClient(l.builder)
	done := make(chan error, 1)
	go func() {
		_, err := client.PrepareImage(ctx, &agentpb.RunContainerLayersRequest{ImageName: "localhost:5000/pipeline:latest", ImageConfig: []byte("pipeline")})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("prepare did not start")
	}
	stream, err := client.WriteChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("pipeline chunk")
	hash := sha256.Sum256(data)
	if err = stream.Send(&agentpb.WriteChunksRequest{Hash: hash[:], Data: data}); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("admission store blocked chunks while preparation waited")
	}
}

func TestDelegatedChunkStreamExpiresWhileIdle(t *testing.T) {
	l := newDeliveryRPCLab(t)
	c := l.f.Claims("idle:latest")
	c.AdmitBefore = c.IssuedAt + 3
	c.ValidUntil = c.IssuedAt + 5
	session := l.admit(t, c)
	ctx, cancel := context.WithTimeout(grantContext(session), 9*time.Second)
	defer cancel()
	stream, err := agentpb.NewWendyContainerServiceClient(l.builder).WriteChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Do not send or half-close. The server must end the RPC at the grant's
	// deadline rather than retaining an idle receiving goroutine indefinitely.
	err = stream.RecvMsg(&agentpb.WriteChunksResponse{})
	if status.Code(err) != codes.DeadlineExceeded || time.Now().Unix() > c.ValidUntil+2 {
		t.Fatalf("idle stream did not expire at its job deadline: %v", err)
	}
}
