package commands

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fleetOrderServer struct {
	discoveryV2Server
	mu                          sync.Mutex
	calls                       []bool
	inventoryDone               bool
	failInventory, failPresence bool
}

func (s *fleetOrderServer) ListAssets(req *pb.ListAssetsRequest, stream grpc.ServerStreamingServer[pb.ListAssetsResponse]) error {
	s.mu.Lock()
	s.calls = append(s.calls, req.GetOnlineOnly())
	done := s.inventoryDone
	s.mu.Unlock()
	if req.GetOnlineOnly() {
		if !done {
			s.t.Error("CLI presence started before inventory completed")
		}
		if s.failPresence {
			return status.Error(codes.Unavailable, "presence unavailable")
		}
		return nil
	}
	defer func() { s.mu.Lock(); s.inventoryDone = true; s.mu.Unlock() }()
	if s.failInventory {
		return status.Error(codes.PermissionDenied, "inventory denied")
	}
	return s.discoveryV2Server.ListAssets(req, stream)
}

func TestDesktopFleetSequentialAndPartialFailure(t *testing.T) {
	for _, mode := range []string{"success", "presence failure", "inventory failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := &fleetOrderServer{discoveryV2Server: discoveryV2Server{t: t, count: 1, organization: testOperatorTenant}, failPresence: mode == "presence failure", failInventory: mode == "inventory failure"}
			srv := grpc.NewServer()
			pb.RegisterAssetServiceServer(srv, server)
			go srv.Serve(lis)
			t.Cleanup(srv.Stop)
			auth := oidcEnrollmentAuth(t)
			auth.Name = "fleet"
			auth.CloudGRPC = lis.Addr().String()
			if err := config.Save(&config.Config{Auth: []config.AuthConfig{*auth}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := DesktopFleetOverview(ctx, "fleet", testOperatorTenant)
			server.mu.Lock()
			defer server.mu.Unlock()
			if mode == "inventory failure" {
				if err == nil || len(server.calls) != 1 {
					t.Fatalf("inventory error=%v calls=%v", err, server.calls)
				}
				return
			}
			if err != nil || len(result.Devices) != 1 || result.Devices[0].Online || len(server.calls) != 2 || server.calls[0] || !server.calls[1] {
				t.Fatalf("result=%+v err=%v calls=%v", result, err, server.calls)
			}
			if (mode == "presence failure") != strings.Contains(result.PresenceError, "presence unavailable") {
				t.Fatalf("presence error=%q", result.PresenceError)
			}
			if !strings.Contains(result.Devices[0].Selector, testOperatorTenant) {
				t.Fatalf("wrong tenant selector %q", result.Devices[0].Selector)
			}
		})
	}
}

func TestDesktopFleetReaderRejectsWrongScope(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	const org = "11111111-1111-4111-8111-111111111111"
	cfg := &config.Config{Auth: []config.AuthConfig{{Name: "selected", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + org + "/operator/one"}}}}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDesktopFleetReader("selected", org); err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][2]string{{"selected", ""}, {"missing", org}, {"selected", "22222222-2222-4222-8222-222222222222"}} {
		if _, err := NewDesktopFleetReader(scope[0], scope[1]); err == nil {
			t.Fatalf("accepted wrong scope %v", scope)
		}
	}
}
