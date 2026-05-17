package mcp

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	"google.golang.org/grpc"
)

type fakeCloudAssetServer struct {
	cloudpb.UnimplementedAssetServiceServer
	assets []*cloudpb.Asset
	req    *cloudpb.ListAssetsRequest
}

func (s *fakeCloudAssetServer) ListAssets(req *cloudpb.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpb.ListAssetsResponse]) error {
	s.req = req
	for _, a := range s.assets {
		if err := stream.Send(&cloudpb.ListAssetsResponse{Asset: a}); err != nil {
			return err
		}
	}
	return nil
}

func startFakeCloudAssetServer(t *testing.T, svc *fakeCloudAssetServer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	cloudpb.RegisterAssetServiceServer(g, svc)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop() })
	return ln.Addr().String()
}

func TestCloudDiscover_ReturnsConfiguredCloudDevices(t *testing.T) {
	fake := &fakeCloudAssetServer{
		assets: []*cloudpb.Asset{
			{
				Id:              42,
				OrganizationId:  7,
				Name:            "edge-one",
				AssetType:       "device",
				IsComputeDevice: true,
			},
		},
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var devices []map[string]any
	if err := json.Unmarshal([]byte(text), &devices); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if len(devices) != 1 {
		t.Fatalf("len(devices) = %d, want 1", len(devices))
	}
	if devices[0]["name"] != "edge-one" {
		t.Errorf("name = %v, want edge-one", devices[0]["name"])
	}
	if fake.req == nil || fake.req.GetOrganizationId() != 7 {
		t.Fatalf("ListAssets organization_id = %v, want 7", fake.req)
	}
	if !fake.req.GetIsComputeDevice() {
		t.Fatal("ListAssets did not request compute devices")
	}
}

func TestCloudDiscover_RequiresAuth(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
}

func TestCloudDiscover_RequiresCloudGRPCWhenMultipleAuthSessionsExist(t *testing.T) {
	srv := New(&config.Config{
		Auth: []config.AuthConfig{
			{CloudGRPC: "one:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			{CloudGRPC: "two:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
		},
	}, nil)
	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
}

func TestCloudRun_RequiresProjectPath(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_run", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
}

func TestRun_MissingProjectPath(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "run", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when project_path is missing")
	}
}
