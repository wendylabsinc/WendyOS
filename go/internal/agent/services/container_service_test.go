package services

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ---------- mock containerd client ----------

type mockContainerdClient struct {
	containers    []*agentpb.AppContainer
	listErr       error
	stopErr       error
	deleteErr     error
	layers        []*agentpb.LayerHeader
	listLayersErr error
	writeLayerErr error
	writtenDigest string
	writtenData   []byte
	createErr     error
	startOutputCh chan ContainerOutput
	startErr      error
}

func (m *mockContainerdClient) ListContainers(_ context.Context) ([]*agentpb.AppContainer, error) {
	return m.containers, m.listErr
}
func (m *mockContainerdClient) StopContainer(_ context.Context, _ string) error {
	return m.stopErr
}
func (m *mockContainerdClient) DeleteContainer(_ context.Context, _ string, _ bool) error {
	return m.deleteErr
}
func (m *mockContainerdClient) ListLayers(_ context.Context) ([]*agentpb.LayerHeader, error) {
	return m.layers, m.listLayersErr
}
func (m *mockContainerdClient) WriteLayer(_ context.Context, digest string, reader io.Reader, _ int64) error {
	m.writtenDigest = digest
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.writtenData = data
	return m.writeLayerErr
}
func (m *mockContainerdClient) AssembleImage(_ context.Context, _ string, _ []*agentpb.RunContainerLayerHeader) error {
	return nil
}
func (m *mockContainerdClient) CreateContainer(_ context.Context, _ *agentpb.CreateContainerRequest, _ *appconfig.AppConfig) error {
	return m.createErr
}
func (m *mockContainerdClient) StartContainer(_ context.Context, _ string) (<-chan ContainerOutput, error) {
	if m.startErr != nil {
		return nil, m.startErr
	}
	return m.startOutputCh, nil
}

// ---------- bufconn helper ----------

func startContainerServer(t *testing.T, client ContainerdClient) (agentpb.WendyContainerServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	logger := zap.NewNop()
	svc := NewContainerService(logger, client)
	agentpb.RegisterWendyContainerServiceServer(srv, svc)

	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	cl := agentpb.NewWendyContainerServiceClient(conn)
	cleanup := func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
	return cl, cleanup
}

// ---------- tests ----------

func TestListContainers(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{AppName: "app-one", AppVersion: "1.0"},
		{AppName: "app-two", AppVersion: "2.0"},
	}
	client, cleanup := startContainerServer(t, &mockContainerdClient{containers: containers})
	defer cleanup()

	stream, err := client.ListContainers(context.Background(), &agentpb.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}

	var received []*agentpb.AppContainer
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		received = append(received, resp.Container)
	}

	if len(received) != 2 {
		t.Fatalf("len(containers) = %d; want 2", len(received))
	}
	if received[0].AppName != "app-one" {
		t.Errorf("containers[0].AppName = %q; want app-one", received[0].AppName)
	}
}

func TestStopContainer(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{
		AppName: "test-app",
	})
	if err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
}

func TestDeleteContainer(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName:     "test-app",
		DeleteImage: false,
	})
	if err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}
}

func TestDeleteContainer_WithImage(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName:     "test-app",
		DeleteImage: true,
	})
	if err != nil {
		t.Fatalf("DeleteContainer with image: %v", err)
	}
}

func TestDeleteContainer_Error(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{
		deleteErr: fmt.Errorf("container not found"),
	})
	defer cleanup()

	_, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName: "missing-app",
	})
	if err == nil {
		t.Fatal("expected error from DeleteContainer")
	}
}

func TestListLayers(t *testing.T) {
	layers := []*agentpb.LayerHeader{
		{Digest: "sha256:abc123", Size: 1024},
		{Digest: "sha256:def456", Size: 2048},
	}
	client, cleanup := startContainerServer(t, &mockContainerdClient{layers: layers})
	defer cleanup()

	stream, err := client.ListLayers(context.Background(), &agentpb.ListLayersRequest{})
	if err != nil {
		t.Fatalf("ListLayers: %v", err)
	}

	var received []*agentpb.LayerHeader
	for {
		layer, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		received = append(received, layer)
	}

	if len(received) != 2 {
		t.Fatalf("len(layers) = %d; want 2", len(received))
	}
	if received[0].Digest != "sha256:abc123" {
		t.Errorf("layer[0].Digest = %q; want sha256:abc123", received[0].Digest)
	}
	if received[1].Size != 2048 {
		t.Errorf("layer[1].Size = %d; want 2048", received[1].Size)
	}
}

func TestWriteLayer(t *testing.T) {
	mock := &mockContainerdClient{}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	stream, err := client.WriteLayer(context.Background())
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	digest := "sha256:testdigest"
	data := []byte("layer-data-part1")
	data2 := []byte("layer-data-part2")

	// Send first chunk with digest.
	if err := stream.Send(&agentpb.WriteLayerRequest{
		Digest: digest,
		Data:   data,
	}); err != nil {
		t.Fatalf("send chunk1: %v", err)
	}

	// Send second chunk.
	if err := stream.Send(&agentpb.WriteLayerRequest{
		Digest: digest,
		Data:   data2,
	}); err != nil {
		t.Fatalf("send chunk2: %v", err)
	}

	// Close send and receive response.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	_, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv response: %v", err)
	}

	if mock.writtenDigest != digest {
		t.Errorf("writtenDigest = %q; want %q", mock.writtenDigest, digest)
	}
	expectedData := append(data, data2...)
	if string(mock.writtenData) != string(expectedData) {
		t.Errorf("writtenData = %q; want %q", mock.writtenData, expectedData)
	}
}
