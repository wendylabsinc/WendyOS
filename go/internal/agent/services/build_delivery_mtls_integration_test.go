//go:build linux

package services_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	agentruntime "github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/registry"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/buildassignment"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/rpcauth"
	"github.com/wendylabsinc/wendy/go/internal/testutil/deliverypki"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// This test needs real BuildKit/buildctl, a fresh containerd daemon, and an empty
// /var/lib/wendy in a disposable Linux filesystem or private mount namespace.
// Set both socket addresses explicitly; ordinary package tests never run it.
func TestBuildDeliveryRealRuntimesMTLS(t *testing.T) {
	buildkitAddr, containerdAddr := os.Getenv("WENDY_TEST_BUILDKIT_ADDR"), os.Getenv("WENDY_TEST_CONTAINERD_ADDR")
	if buildkitAddr == "" || containerdAddr == "" {
		t.Skip("set WENDY_TEST_BUILDKIT_ADDR and WENDY_TEST_CONTAINERD_ADDR for isolated real-runtime validation")
	}
	if os.Geteuid() != 0 {
		t.Fatal("real image preparation requires root in the isolated test environment")
	}
	if entries, err := os.ReadDir("/var/lib/wendy"); (err != nil && !os.IsNotExist(err)) || len(entries) != 0 {
		t.Fatal("/var/lib/wendy must be empty and isolated from the installed agent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	logger := zap.NewNop()
	raw, err := containerdclient.New(containerdAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	runtimeCtx := namespaces.WithNamespace(ctx, "default")
	images, err := raw.ListImages(runtimeCtx)
	if err != nil || len(images) != 0 {
		t.Fatalf("containerd must be a fresh isolated daemon: images=%d err=%v", len(images), err)
	}
	rt, err := agentruntime.NewClient(logger, containerdAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	f := deliverypki.New(t)
	rootPEM := f.Chain
	target, builder := f.TargetID, mustDeliveryDevice(t, deliverypki.BuilderPrincipal)
	targetCert, targetKey := f.Target.PEM, f.Target.Key
	builderCert, builderKey := f.Builder.PEM, f.Builder.Key
	primary := f.Mint(t, deliverypki.OperatorPrincipal, nil)
	operatorCert, operatorKey := primary.PEM, primary.Key
	targetManager := f.Manager(t)
	workerManager := delivery.NewManager(t.TempDir(), "localhost:5000/", func() (string, string) { return builderCert, rootPEM })
	t.Cleanup(workerManager.Close)
	rt.SetDeliverySource(targetManager.Get)
	registryTLS, err := mtls.NewTLSConfig(targetCert, rootPEM, targetKey, logger, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	registryServer, err := registry.Start(ctx, containerdAddr, "127.0.0.1:5000", logger, registryTLS, registry.WithDeliveryAuthorization(targetManager.Get, interceptor.OrgModeStrict))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registryServer.Shutdown(context.Background()) })
	start := func(cert, key string, register func(*grpc.Server), opts ...grpc.ServerOption) string {
		t.Helper()
		srv, err := mtls.NewServer(cert, rootPEM, key, logger, time.Time{}, certs.Scope{TenantUUID: rpcTestDeliveryTenant}, interceptor.OrgModeStrict, opts...)
		if err != nil {
			t.Fatal(err)
		}
		register(srv)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Stop)
		go func() { _ = srv.Serve(listener) }()
		return listener.Addr().String()
	}
	var mu sync.Mutex
	calls := map[string]int{}
	record := func(method string) { mu.Lock(); calls[method]++; mu.Unlock() }
	targetAddr := start(targetCert, targetKey, func(s *grpc.Server) {
		agentpb.RegisterWendyContainerServiceServer(s, services.NewContainerService(logger, rt, services.WithDeliverySource(targetManager.Get)))
		agentpbv2.RegisterWendyBuildDeliveryServiceServer(s, services.NewBuildDeliveryService(targetManager.Get, rt))
	}, grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		record(info.FullMethod)
		return next(ctx, req)
	}), grpc.ChainStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		record(info.FullMethod)
		return next(srv, stream)
	}))
	contextTar := new(bytes.Buffer)
	tw := tar.NewWriter(contextTar)
	for name, body := range map[string]string{"Dockerfile": "FROM scratch\nCOPY marker.txt /marker.txt\n", "marker.txt": "real BuildKit to operator-authorized PrepareImage and registry\n"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	contextHash := sha256.Sum256(contextTar.Bytes())
	pushTLS := func(want meshid.Device) (*tls.Config, error) {
		return mtls.NewClientTLSConfigExpectingDevice(builderCert, rootPEM, builderKey, logger, want)
	}
	buildConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildConfig, "build-host-enabled"), []byte("true"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := services.NewBuildService(logger, services.BuildServiceOptions{
		ConfigPath: buildConfig, StateDir: t.TempDir(), BuildkitAddress: buildkitAddr, DeliverySource: workerManager.Get,
		Chunks: deliveryContext(contextTar.Bytes()), Peers: deliveryPeer{agent: targetAddr, registry: "127.0.0.1:5000"}, PushTLS: pushTLS,
	})
	builderAddr := start(builderCert, builderKey, func(s *grpc.Server) { agentpbv2.RegisterWendyBuildServiceServer(s, svc) })
	operatorTLS, err := mtls.NewClientTLSConfigExpectingDevice(operatorCert, rootPEM, operatorKey, logger, builder)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := grpc.NewClient(builderAddr, grpc.WithTransportCredentials(credentials.NewTLS(operatorTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	firstUploads := 0
	for iteration := range 3 {
		stream, err := agentpbv2.NewWendyBuildServiceClient(cc).BuildImage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		spec := &agentpbv2.BuildSpec{
			AppId: "f1-delivery", Platform: "linux/" + runtime.GOARCH, Chunking: agentpbv2.ChunkingMode_CHUNKING_MODE_FORCE,
			Context:     &agentpbv2.ChunkManifest{ChunkHashes: [][]byte{contextHash[:]}},
			PushTargets: []*agentpbv2.PushTarget{{AssetUuid: target.UUID().String(), RegistryPort: 5000, Repository: "f1-delivery:latest"}},
			Definition:  &agentpbv2.BuildSpec_DockerfileBuild{DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile"}},
		}
		if iteration == 2 {
			spec.Chunking = agentpbv2.ChunkingMode_CHUNKING_MODE_OFF
		}
		input, err := buildassignment.InputSHA256(spec)
		if err != nil {
			t.Fatal(err)
		}
		claims := f.Claims("f1-delivery:latest")
		claims.InputSHA256 = input
		if prior, err := rt.ImageDigest(ctx, "localhost:5000/f1-delivery:latest"); err == nil {
			claims.ExpectedDigest = prior
		}
		spec.PushTargets[0].DeliveryAuthorizationJws = f.Sign(t, claims)
		snapshot, err := targetManager.Get()
		if err != nil {
			t.Fatal(err)
		}
		spec.PushTargets[0].DeliverySession, err = snapshot.Store.Admit(spec.PushTargets[0].DeliveryAuthorizationJws, primary.Leaf)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&agentpbv2.BuildImageRequest{Spec: spec}); err != nil {
			t.Fatal(err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		var result *agentpbv2.BuildImageResult
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("build/delivery %d: %v", iteration+1, err)
			}
			if event.GetLogLine() != "" {
				t.Log(event.GetLogLine())
			}
			if event.GetResult() != nil {
				result = event.GetResult()
			}
		}
		if result == nil || len(result.GetDeliveries()) != 1 || !result.GetDeliveries()[0].GetDelivered() {
			t.Fatalf("build/delivery %d did not finish: %v", iteration+1, result)
		}
		if _, done, err := snapshot.Store.CompletedOutput(spec.PushTargets[0].GetDeliverySession(), f.Builder.Leaf); err != nil || !done {
			t.Fatalf("target did not complete delivery %d: %v", iteration, err)
		}
		if iteration == 0 {
			mu.Lock()
			firstUploads = calls[agentpb.WendyContainerService_WriteChunks_FullMethodName]
			mu.Unlock()
		}
	}
	image, err := raw.GetImage(runtimeCtx, "localhost:5000/f1-delivery:latest")
	if err != nil {
		t.Fatalf("PrepareImage did not register the built image: %v", err)
	}
	unpacked := false
	for _, snapshotter := range []string{"overlayfs", "native"} {
		ready, err := image.IsUnpacked(runtimeCtx, snapshotter)
		unpacked = unpacked || (err == nil && ready)
	}
	if !unpacked {
		t.Fatal("PrepareImage registered the image without unpacking its layer")
	}
	mu.Lock()
	observed := make(map[string]int, len(calls))
	for method, count := range calls {
		observed[method] = count
	}
	mu.Unlock()
	for _, method := range []string{agentpb.WendyContainerService_QueryChunks_FullMethodName, agentpb.WendyContainerService_QueryLayers_FullMethodName, agentpb.WendyContainerService_WriteChunks_FullMethodName, agentpb.WendyContainerService_PrepareImage_FullMethodName} {
		if observed[method] == 0 {
			t.Fatalf("real delivery did not exercise %s", method)
		}
	}
	if observed[agentpb.WendyContainerService_PrepareImage_FullMethodName] != 2 || firstUploads == 0 || observed[agentpb.WendyContainerService_WriteChunks_FullMethodName] != firstUploads {
		t.Fatalf("expected two preparations and no extra upload after layer reuse: %v", observed)
	}
	deviceTLS, err := pushTLS(target)
	if err != nil {
		t.Fatal(err)
	}
	device, err := grpc.NewClient(targetAddr, grpc.WithTransportCredentials(credentials.NewTLS(deviceTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	listing, err := agentpb.NewWendyContainerServiceClient(device).ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err == nil {
		_, err = listing.Recv()
	}
	if !rpcauth.IsIdentityDenied(err) {
		t.Fatalf("device gained container management after staging: %v", err)
	}
}

const rpcTestDeliveryTenant = "11111111-1111-4111-8111-111111111111"

type deliveryContext []byte

func (data deliveryContext) OpenChunkStream(context.Context, [][32]byte) io.Reader {
	return bytes.NewReader(data)
}

// Discovery is fixed to the loopback test target; the production delivery
// dialer still performs its pinned TLS handshake with the builder's device key.
type deliveryPeer struct{ agent, registry string }

func (peer deliveryPeer) DialTarget(ctx context.Context, _ services.DeliveryTarget, port uint16) (net.Conn, string, error) {
	addr := peer.agent
	if port == 5000 {
		addr = peer.registry
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	return conn, "lan-direct", err
}

func mustDeliveryDevice(t *testing.T, principal string) meshid.Device {
	t.Helper()
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	return device
}
