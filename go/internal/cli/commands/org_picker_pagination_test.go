//go:build darwin || linux || windows

package commands

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// fakeOrgService mimics the cloud's ListOrganizations: it pages, and applies a
// small default page size when the request omits a limit — the behaviour that
// silently truncated the org picker to the 20 oldest orgs.
type fakeOrgService struct {
	cloudpb.UnimplementedOrganizationServiceServer

	total          int
	defaultLimit   int32
	maxLimit       int32
	requestedPages []struct{ offset, limit int32 }
}

func (s *fakeOrgService) ListOrganizations(req *cloudpb.ListOrganizationsRequest, stream grpc.ServerStreamingServer[cloudpb.ListOrganizationsResponse]) error {
	offset := req.GetOffset()
	limit := req.GetLimit()
	if limit <= 0 {
		limit = s.defaultLimit
	}
	if limit > s.maxLimit {
		limit = s.maxLimit
	}
	s.requestedPages = append(s.requestedPages, struct{ offset, limit int32 }{offset, limit})

	for i := offset; i < offset+limit && int(i) < s.total; i++ {
		// Org IDs start at 2, matching production (1 is reserved).
		id := i + 2
		err := stream.Send(&cloudpb.ListOrganizationsResponse{
			Organization: &cloudpb.Organization{Id: id, Name: fmt.Sprintf("Org %d", id)},
			Total:        int32(s.total),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func startFakeOrgServer(t *testing.T, svc *fakeOrgService) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterOrganizationServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// TestListOrgsFromCloudPaginates guards against the picker showing only the
// first server page: orgs past the default page size must still be returned.
func TestListOrgsFromCloudPaginates(t *testing.T) {
	svc := &fakeOrgService{total: 51, defaultLimit: 20, maxLimit: 1000}
	addr := startFakeOrgServer(t, svc)

	auth := &config.AuthConfig{
		CloudGRPC:    addr, // plaintext via WENDY_CLOUD_INSECURE (TestMain), no certs needed on the wire
		Certificates: []config.CertificateInfo{{OrganizationID: 2, UserID: "test-user"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	orgs, err := listOrgsFromCloudImpl(ctx, auth)
	if err != nil {
		t.Fatalf("listOrgsFromCloudImpl: %v", err)
	}
	if len(orgs) != svc.total {
		t.Fatalf("got %d orgs, want %d (pagination dropped orgs past the first page)", len(orgs), svc.total)
	}
	if got := orgs[len(orgs)-1].GetId(); got != int32(svc.total+1) {
		t.Errorf("last org id = %d, want %d", got, svc.total+1)
	}
	for _, p := range svc.requestedPages {
		if p.limit <= 0 {
			t.Errorf("request sent no limit (offset=%d); the server default would truncate the list", p.offset)
		}
	}
}

// TestListOrgsFromCloudCapsPageRequests ensures a server that keeps returning
// full pages cannot spin the pagination loop forever.
func TestListOrgsFromCloudCapsPageRequests(t *testing.T) {
	// maxLimit below the client page size makes every page look "full" to a
	// naive loop; the cap must stop it.
	svc := &fakeOrgService{total: 1_000_000, defaultLimit: 20, maxLimit: orgPageSize}
	addr := startFakeOrgServer(t, svc)

	auth := &config.AuthConfig{
		CloudGRPC:    addr,
		Certificates: []config.CertificateInfo{{OrganizationID: 2, UserID: "test-user"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	orgs, err := listOrgsFromCloudImpl(ctx, auth)
	if err != nil {
		t.Fatalf("listOrgsFromCloudImpl: %v", err)
	}
	if want := orgPageSize * orgPageCap; len(orgs) != want {
		t.Fatalf("got %d orgs, want %d (page cap not enforced)", len(orgs), want)
	}
	if len(svc.requestedPages) != orgPageCap {
		t.Errorf("made %d page requests, want cap of %d", len(svc.requestedPages), orgPageCap)
	}
}
