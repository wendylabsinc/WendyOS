package commands

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"testing"
)

type legacyReleaseRecorder struct {
	pb.UnimplementedDeploymentServiceServer
	created *pb.CreateAppReleaseRequest
	updated *pb.UpdateAppReleaseRequest
}

func (s *legacyReleaseRecorder) CreateAppRelease(_ context.Context, req *pb.CreateAppReleaseRequest) (*pb.AppRelease, error) {
	s.created = req
	return &pb.AppRelease{Id: 123}, nil
}
func (s *legacyReleaseRecorder) UpdateAppRelease(_ context.Context, req *pb.UpdateAppReleaseRequest) (*pb.AppRelease, error) {
	s.updated = req
	return &pb.AppRelease{Id: req.Id}, nil
}
func TestLegacyReleaseUsesNumericV1Contract(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &legacyReleaseRecorder{}
	server := grpc.NewServer()
	pb.RegisterDeploymentServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{OrganizationID: 64, UserID: "operator"}}}
	id, err := createAppRelease(context.Background(), conn, auth, "64", "app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "123" || recorder.created.OrganizationId != 64 {
		t.Fatalf("id=%s request=%v", id, recorder.created)
	}
	if err := setAppReleaseDigest(context.Background(), conn, auth, id, "v1", "sha256:test"); err != nil {
		t.Fatal(err)
	}
	if recorder.updated.Id != 123 || recorder.updated.Name != "v1" || recorder.updated.ImageDigest != "sha256:test" {
		t.Fatalf("update=%v", recorder.updated)
	}
	if _, err := createAppRelease(context.Background(), conn, auth, "65", "app", "v1"); err == nil {
		t.Fatal("accepted another organization")
	}
	if err := setAppReleaseDigest(context.Background(), conn, auth, "11111111-1111-4111-8111-111111111111", "v1", "digest"); err == nil {
		t.Fatal("sent UUID to legacy service")
	}
}
