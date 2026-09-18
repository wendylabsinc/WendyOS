package services

import (
	"context"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func startContainerV2Server(t *testing.T, client ContainerdClient) (agentpbv2.WendyContainerServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	v1svc := NewContainerService(zap.NewNop(), client)
	svc := NewContainerServiceV2(v1svc)
	agentpbv2.RegisterWendyContainerServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyContainerServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestContainerServiceV2_StopContainer_NoClient(t *testing.T) {
	client, cleanup := startContainerV2Server(t, nil)
	defer cleanup()

	_, err := client.StopContainer(context.Background(), &agentpbv2.StopContainerRequest{AppName: "myapp"})
	if status.Code(err) != codes.Internal {
		t.Errorf("error code = %v; want Internal", status.Code(err))
	}
}

func TestContainerServiceV2_ListContainers_Empty(t *testing.T) {
	mc := &mockContainerdClient{}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	stream, err := client.ListContainers(context.Background(), &agentpbv2.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	_, err = stream.Recv()
	if err != io.EOF {
		t.Errorf("expected EOF for empty list, got %v", err)
	}
}

type cachePruningContainerdClient struct {
	*mockContainerdClient
	result  CachePruneResult
	gotOpts CachePruneOptions
}

func (c *cachePruningContainerdClient) PruneCache(_ context.Context, opts CachePruneOptions) (CachePruneResult, error) {
	c.gotOpts = opts
	return c.result, nil
}

func TestContainerServiceV2_PruneCache(t *testing.T) {
	mc := &cachePruningContainerdClient{
		mockContainerdClient: &mockContainerdClient{},
		result: CachePruneResult{
			ContentBlobs: 2, ContentBytes: 30, Snapshots: 4, SnapshotBytes: 50, MinimumAgeSeconds: 3600,
		},
	}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	resp, err := client.PruneCache(context.Background(), &agentpbv2.PruneCacheRequest{DryRun: true})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if !mc.gotOpts.DryRun {
		t.Fatal("dry_run was not forwarded")
	}
	if resp.GetContentBlobs() != 2 || resp.GetContentBytes() != 30 || resp.GetSnapshots() != 4 || resp.GetSnapshotBytes() != 50 || resp.GetMinimumAgeSeconds() != 3600 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestContainerServiceV2_PruneCacheForwardsMinAge(t *testing.T) {
	mc := &cachePruningContainerdClient{mockContainerdClient: &mockContainerdClient{}}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	_, err := client.PruneCache(context.Background(), &agentpbv2.PruneCacheRequest{MinAgeSeconds: proto.Uint64(0)})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if mc.gotOpts.MinAge == nil {
		t.Fatal("MinAge = nil, want 0s")
	}
	if *mc.gotOpts.MinAge != 0 {
		t.Fatalf("MinAge = %v, want 0s", *mc.gotOpts.MinAge)
	}
}

func TestContainerServiceV2_PruneCacheDefaultsMinAgeWhenAbsent(t *testing.T) {
	mc := &cachePruningContainerdClient{mockContainerdClient: &mockContainerdClient{}}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	_, err := client.PruneCache(context.Background(), &agentpbv2.PruneCacheRequest{})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if mc.gotOpts.MinAge != nil {
		t.Fatalf("MinAge = %v, want nil when min_age_seconds is absent", *mc.gotOpts.MinAge)
	}
}

func TestContainerServiceV2_PruneCacheReportsReclaimedBytes(t *testing.T) {
	reclaimed := uint64(4096)
	mc := &cachePruningContainerdClient{
		mockContainerdClient: &mockContainerdClient{},
		result:               CachePruneResult{ReclaimedBytes: &reclaimed},
	}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	resp, err := client.PruneCache(context.Background(), &agentpbv2.PruneCacheRequest{})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if resp.ReclaimedBytes == nil || *resp.ReclaimedBytes != reclaimed {
		t.Fatalf("ReclaimedBytes = %v, want %d", resp.ReclaimedBytes, reclaimed)
	}

	mc.result = CachePruneResult{}
	resp, err = client.PruneCache(context.Background(), &agentpbv2.PruneCacheRequest{})
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if resp.ReclaimedBytes != nil {
		t.Fatalf("ReclaimedBytes = %v, want nil when the pruner did not measure it", *resp.ReclaimedBytes)
	}
}

// TestContainerServiceV2_PruneCacheRejectsOverflowingMinAge guards against
// time.Duration(*req.MinAgeSeconds) * time.Second overflowing (and
// potentially going negative) for a min_age_seconds value above
// math.MaxInt64/int64(time.Second); the handler must reject it outright
// rather than let a wrapped-negative duration reach pruneCutoff.
func TestContainerServiceV2_PruneCacheRejectsOverflowingMinAge(t *testing.T) {
	mc := &cachePruningContainerdClient{mockContainerdClient: &mockContainerdClient{}}
	client, cleanup := startContainerV2Server(t, mc)
	defer cleanup()

	tooLarge := uint64(math.MaxInt64/int64(time.Second)) + 1
	_, err := client.PruneCache(context.Background(), &agentpbv2.PruneCacheRequest{MinAgeSeconds: proto.Uint64(tooLarge)})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v; want InvalidArgument", status.Code(err))
	}
}

func TestContainerServiceV2_PruneCacheUnsupported(t *testing.T) {
	client, cleanup := startContainerV2Server(t, &mockContainerdClient{})
	defer cleanup()

	_, err := client.PruneCache(context.Background(), &agentpbv2.PruneCacheRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %v; want Unimplemented", status.Code(err))
	}
}
