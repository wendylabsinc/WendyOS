package commands

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// clouddefaults.FindAssetByNameOrID resolves a device by name with a
// first-match scan, and cloud scopes device-name uniqueness to COMPUTE devices
// (WDY-3016) — a building or vehicle asset may legally carry a device's name.
// So the lists that reach that scan have to be compute-only, which holds only
// because this request asks for it. Dropping the filter would silently let a
// non-device asset shadow a device; nothing else in the CLI would go red.
//
// The mcp half of the same precondition is pinned by
// TestCloudDiscover_ReturnsConfiguredCloudDevices over there.
func TestFetchCloudAssetsRequestsComputeDevicesOnly(t *testing.T) {
	fake := &captureAssetServer{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterAssetServiceServer(srv, fake)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	// A non-":443" address keeps the dial insecure, and an auth entry with no
	// OAuth issuer and no API key needs no token.
	auth := &config.AuthConfig{
		CloudGRPC:    ln.Addr().String(),
		Certificates: []config.CertificateInfo{{OrganizationID: 7}},
	}
	if _, err := fetchCloudAssetsFiltered(context.Background(), auth, false); err != nil {
		t.Fatalf("fetchCloudAssetsFiltered: %v", err)
	}

	if fake.req == nil {
		t.Fatal("no ListAssets request reached the server")
	}
	if !fake.req.GetIsComputeDevice() {
		t.Error("ListAssets did not restrict the listing to compute devices")
	}
	if got := fake.req.GetOrganizationId(); got != 7 {
		t.Errorf("ListAssets organization_id = %d, want 7", got)
	}
}

type captureAssetServer struct {
	cloudpb.UnimplementedAssetServiceServer
	req *cloudpb.ListAssetsRequest
}

// Records the request and returns an empty page, which ends the caller's
// pagination loop on its no-progress guard.
func (s *captureAssetServer) ListAssets(req *cloudpb.ListAssetsRequest, _ grpc.ServerStreamingServer[cloudpb.ListAssetsResponse]) error {
	s.req = req
	return nil
}
