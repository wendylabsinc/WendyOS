package browserauth

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

type assetInventoryServer struct {
	profileCloudServer
	asset  *cloudpbv2.Asset
	online bool
}

func (s assetInventoryServer) ListAssets(r *cloudpbv2.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpbv2.ListAssetsResponse]) error {
	if r.GetOnlineOnly() != s.online || r.GetOrganizationId() != "12345678-1234-1234-1234-123456789abc" {
		return fmt.Errorf("unexpected inventory scope or filter")
	}
	return stream.Send(&cloudpbv2.ListAssetsResponse{Asset: s.asset, Total: 1})
}
func TestCloudInventoryTenantAndOfflineFilter(t *testing.T) {
	for _, test := range []struct {
		name, id, tenant string
		online, valid    bool
	}{
		{"online", "00000000-0000-4000-8000-000000000001", "12345678-1234-1234-1234-123456789abc", true, true},
		{"offline", "00000000-0000-4000-8000-000000000001", "12345678-1234-1234-1234-123456789abc", false, true},
		{"another tenant", "00000000-0000-4000-8000-000000000001", "12345678-1234-1234-1234-123456789abd", true, false},
		{"invalid asset", "not-an-asset", "12345678-1234-1234-1234-123456789abc", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := setup(t, nil)
			if _, err := s.Complete(context.Background(), "valid-code", s.state, s.meta.Issuer); err != nil {
				t.Fatal(err)
			}
			listener := bufconn.Listen(1 << 20)
			defer listener.Close()
			server := grpc.NewServer()
			defer server.Stop()
			svc := assetInventoryServer{asset: &cloudpbv2.Asset{Id: test.id, OrganizationId: test.tenant}, online: test.online}
			cloudpbv2.RegisterOrganizationServiceServer(server, svc)
			cloudpbv2.RegisterAssetServiceServer(server, svc)
			go server.Serve(listener)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			assets, err := s.DiscoverFiltered(ctx, func(ctx context.Context, target string) (net.Conn, error) {
				if target != "api.dev.wendy.sh:443" {
					t.Error("unpinned Cloud authority", target)
				}
				return listener.DialContext(ctx)
			}, test.online)
			if (err == nil) != test.valid || (test.valid && (len(assets) != 1 || assets[0].GetId() != test.id)) {
				t.Fatalf("inventory %v error %v", assets, err)
			}
		})
	}
}
