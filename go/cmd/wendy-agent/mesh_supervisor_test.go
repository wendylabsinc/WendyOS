package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Exercise the real admin persistence and participation reader together: an
// absent configuration must not permanently stop the enrolled supervisor.
func TestMeshSupervisorFirstConfigurationAfterEnrollment(t *testing.T) {
	for _, enrolledAtBoot := range []bool{true, false} {
		t.Run(map[bool]string{true: "enrolled-at-boot", false: "first-enrollment"}[enrolledAtBoot], func(t *testing.T) {
			dir := t.TempDir()
			tenant, err := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
			if err != nil {
				t.Fatal(err)
			}
			device, err := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
			if err != nil {
				t.Fatal(err)
			}
			var current meshid.Device
			svc := services.NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) {
				if !current.IsValid() {
					return meshid.Tenant{}, meshid.Device{}
				}
				return tenant, current
			})
			uri, err := url.Parse(certs.UserSPIFFEURI(tenant.UUID().String(), "operator"))
			if err != nil {
				t.Fatal(err)
			}
			caller := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{uri}}}}}})
			request := &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), Ethernet: proto.Bool(true)}
			if !enrolledAtBoot {
				if _, err := svc.ConfigureLocalMesh(caller, request); status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("unenrolled configure: %v", err)
				}
			}
			// Both startup and the first enrollment callback start the same supervisor;
			// identity replacement is separately covered by the restart tracker tests.
			current = device
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempted, started, done := make(chan struct{}, 16), make(chan struct{}, 1), make(chan struct{})
			go func() {
				defer close(done)
				runMeshSupervisor(ctx, time.Millisecond, func(ctx context.Context) error {
					select {
					case attempted <- struct{}{}:
					default:
					}
					_, err := runWhileMeshParticipating(ctx, filepath.Join(dir, "nan-mesh.json"), time.Millisecond, func(ctx context.Context) error {
						started <- struct{}{}
						<-ctx.Done()
						return ctx.Err()
					})
					return err
				}, func(err error) { t.Errorf("supervisor: %v", err) })
			}()
			for range 2 {
				select {
				case <-attempted:
				case <-time.After(time.Second):
					t.Fatal("supervisor stopped before first configuration")
				}
			}
			select {
			case <-started:
				t.Fatal("carriers started without opt-in")
			default:
			}
			if _, err := svc.ConfigureLocalMesh(caller, request); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("first configure required restart")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("supervisor did not stop")
			}
			saved, err := meshsharing.LoadConfig(filepath.Join(dir, "nan-mesh.json"))
			if err != nil || !saved.Participate {
				t.Fatalf("saved policy: %+v %v", saved, err)
			}
		})
	}
}
