package services

import (
	"context"
	"io"
	"net"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
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
