package services

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ---------- mock containerd client ----------

type mockContainerdClient struct {
	containers     []*agentpb.AppContainer
	listErr        error
	stopErr        error
	deleteErr      error
	layers         []*agentpb.LayerHeader
	listLayersErr  error
	writeLayerErr  error
	writtenDigest  string
	writtenData    []byte
	createErr      error
	progressPhases []agentpb.CreateContainerProgress_Phase
	startOutputCh  chan ContainerOutput
	startErr       error
	statsResult    []*agentpb.ContainerStats
	statsErr       error
	mcpPort        uint32
	mcpPortErr     error
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
func (m *mockContainerdClient) CreateContainerWithProgress(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig, onProgress ProgressFunc) error {
	if onProgress != nil {
		for _, phase := range m.progressPhases {
			onProgress(&agentpb.CreateContainerProgress{Phase: phase})
		}
	}
	return m.CreateContainer(ctx, req, appCfg)
}
func (m *mockContainerdClient) StartContainer(_ context.Context, _ string, _ string, _ *agentpb.RestartPolicy) (<-chan ContainerOutput, error) {
	if m.startErr != nil {
		return nil, m.startErr
	}
	return m.startOutputCh, nil
}

func (m *mockContainerdClient) StartContainerWithStdin(_ context.Context, _ string, _ io.Reader, _ string, _ *agentpb.RestartPolicy) (<-chan ContainerOutput, error) {
	if m.startErr != nil {
		return nil, m.startErr
	}
	return m.startOutputCh, nil
}
func (m *mockContainerdClient) GetContainerStats(_ context.Context) ([]*agentpb.ContainerStats, error) {
	return m.statsResult, m.statsErr
}

func (m *mockContainerdClient) GetContainerMetrics(_ context.Context, _ string) (ContainerMetrics, error) {
	return ContainerMetrics{}, nil
}

func (m *mockContainerdClient) GetContainerMCPPort(_ context.Context, _ string) (uint32, error) {
	return m.mcpPort, m.mcpPortErr
}

// attachTestMock embeds mockContainerdClient and overrides StartContainerWithStdin
// so tests can capture the appName and stdin reader passed by AttachContainer.
type attachTestMock struct {
	mockContainerdClient
	onStartWithStdin func(appName string, stdin io.Reader, postStartAgentCommand string) (<-chan ContainerOutput, error)
}

func (m *attachTestMock) StartContainerWithStdin(ctx context.Context, appName string, stdin io.Reader, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan ContainerOutput, error) {
	if m.onStartWithStdin != nil {
		return m.onStartWithStdin(appName, stdin, postStartAgentCommand)
	}
	return m.mockContainerdClient.StartContainerWithStdin(ctx, appName, stdin, postStartAgentCommand, restartPolicy)
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

func TestPostStartAgentHookFromContext(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		appconfig.PostStartAgentHookMetadataKey,
		"wendy-agent utils open-browser http://localhost:3000",
	))

	got := postStartAgentHookFromContext(ctx)
	if got != "wendy-agent utils open-browser http://localhost:3000" {
		t.Fatalf("postStartAgentHookFromContext = %q", got)
	}
}

func TestPostStartAgentHookFromContextEmpty(t *testing.T) {
	got := postStartAgentHookFromContext(context.Background())
	if got != "" {
		t.Fatalf("postStartAgentHookFromContext empty = %q", got)
	}
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

func TestCreateContainerWithProgress(t *testing.T) {
	phases := []agentpb.CreateContainerProgress_Phase{
		agentpb.CreateContainerProgress_UNPACKING,
		agentpb.CreateContainerProgress_CREATING_CONTAINER,
		agentpb.CreateContainerProgress_COMPLETE,
	}
	mock := &mockContainerdClient{progressPhases: phases}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	stream, err := client.CreateContainerWithProgress(context.Background(), &agentpb.CreateContainerRequest{
		ImageName: "test-image:latest",
		AppName:   "test-app",
	})
	if err != nil {
		t.Fatalf("CreateContainerWithProgress: %v", err)
	}

	var receivedPhases []agentpb.CreateContainerProgress_Phase
	gotCompleted := false

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}

		switch r := resp.GetResponseType().(type) {
		case *agentpb.CreateContainerProgressResponse_Progress:
			receivedPhases = append(receivedPhases, r.Progress.GetPhase())
		case *agentpb.CreateContainerProgressResponse_Completed:
			gotCompleted = true
		}
	}

	if len(receivedPhases) != len(phases) {
		t.Fatalf("received %d progress phases; want %d", len(receivedPhases), len(phases))
	}
	for i, p := range receivedPhases {
		if p != phases[i] {
			t.Errorf("phase[%d] = %v; want %v", i, p, phases[i])
		}
	}
	if !gotCompleted {
		t.Error("did not receive Completed response")
	}
}

func TestAttachContainer(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	capturedAppCh := make(chan string, 1)
	stdinDataCh := make(chan string, 1)

	mock := &attachTestMock{
		onStartWithStdin: func(appName string, stdin io.Reader, _ string) (<-chan ContainerOutput, error) {
			capturedAppCh <- appName
			go func() {
				// Read all stdin bytes, then produce output so the server
				// doesn't close until we've verified what was forwarded.
				data, _ := io.ReadAll(stdin)
				stdinDataCh <- string(data)
				outputCh <- ContainerOutput{Stdout: []byte("pong")}
				close(outputCh)
			}()
			return outputCh, nil
		},
	}

	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	stream, err := client.AttachContainer(context.Background())
	if err != nil {
		t.Fatalf("AttachContainer: %v", err)
	}

	// First message must be app_name.
	if err := stream.Send(&agentpb.AttachContainerRequest{
		RequestType: &agentpb.AttachContainerRequest_AppName{AppName: "echo-app"},
	}); err != nil {
		t.Fatalf("send app_name: %v", err)
	}

	// Verify StartContainerWithStdin was called with the correct app name.
	if got := <-capturedAppCh; got != "echo-app" {
		t.Errorf("appName = %q; want echo-app", got)
	}

	// Forward stdin data.
	if err := stream.Send(&agentpb.AttachContainerRequest{
		RequestType: &agentpb.AttachContainerRequest_StdinData{StdinData: []byte("ping")},
	}); err != nil {
		t.Fatalf("send stdin_data: %v", err)
	}

	// Close client send so the server's stdin reader reaches EOF, which unblocks
	// the mock goroutine's io.ReadAll and lets it produce output.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	// Expect Started response.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv started: %v", err)
	}
	if resp.GetStarted() == nil {
		t.Error("expected Started response")
	}

	// Expect stdout containing "pong".
	resp, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv stdout: %v", err)
	}
	if string(resp.GetStdoutOutput().GetData()) != "pong" {
		t.Errorf("stdout = %q; want pong", resp.GetStdoutOutput().GetData())
	}

	// Stream should end with EOF.
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("expected EOF; got %v", err)
	}

	// Confirm stdin bytes reached the container's stdin reader.
	if got := <-stdinDataCh; got != "ping" {
		t.Errorf("stdin data = %q; want ping", got)
	}
}

// ---------- mock monitor registrar ----------

type mockMonitorRegistrar struct {
	registerCalls     []registerCall
	explicitStopCalls []string
}

type registerCall struct {
	appName    string
	policy     int
	maxRetries int
}

func (m *mockMonitorRegistrar) Register(appName string, policy int, maxRetries int) {
	m.registerCalls = append(m.registerCalls, registerCall{appName, policy, maxRetries})
}

func (m *mockMonitorRegistrar) Unregister(_ string) {}

func (m *mockMonitorRegistrar) MarkExplicitStop(appName string) {
	m.explicitStopCalls = append(m.explicitStopCalls, appName)
}

// startContainerServerWithMonitor starts a gRPC bufconn server for ContainerService
// configured with the given ContainerdClient and ContainerMonitorRegistrar.
func startContainerServerWithMonitor(t *testing.T, client ContainerdClient, mon ContainerMonitorRegistrar) (agentpb.WendyContainerServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	logger := zap.NewNop()
	svc := NewContainerService(logger, client, WithMonitor(mon))
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

// TestStartContainer_RegistersMonitor_UnlessStopped verifies that starting a
// container with an UNLESS_STOPPED restart policy calls Register on the monitor.
func TestStartContainer_RegistersMonitor_UnlessStopped(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	outputCh <- ContainerOutput{Done: true}
	close(outputCh)

	mc := &mockContainerdClient{startOutputCh: outputCh}
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	stream, err := client.StartContainer(context.Background(), &agentpb.StartContainerRequest{
		AppName: "my-app",
		RestartPolicy: &agentpb.RestartPolicy{
			Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED,
		},
	})
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	// Drain the stream.
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}

	if len(mon.registerCalls) != 1 {
		t.Fatalf("Register call count = %d; want 1", len(mon.registerCalls))
	}
	got := mon.registerCalls[0]
	if got.appName != "my-app" {
		t.Errorf("Register appName = %q; want my-app", got.appName)
	}
	if got.policy != RestartPolicyUnlessStopped {
		t.Errorf("Register policy = %d; want %d (UnlessStopped)", got.policy, RestartPolicyUnlessStopped)
	}
}

// TestStopContainer_CallsMarkExplicitStop verifies that StopContainer calls
// MarkExplicitStop on the monitor before stopping the container.
func TestStopContainer_CallsMarkExplicitStop(t *testing.T) {
	mc := &mockContainerdClient{}
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	_, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{
		AppName: "my-app",
	})
	if err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	if len(mon.explicitStopCalls) != 1 {
		t.Fatalf("MarkExplicitStop call count = %d; want 1", len(mon.explicitStopCalls))
	}
	if mon.explicitStopCalls[0] != "my-app" {
		t.Errorf("MarkExplicitStop appName = %q; want my-app", mon.explicitStopCalls[0])
	}
}

// TestMonitorPolicyInt_NegativeRetries verifies that a negative OnFailureMaxRetries
// value is clamped to zero and does not result in a negative maxRetries.
func TestMonitorPolicyInt_NegativeRetries(t *testing.T) {
	rp := &agentpb.RestartPolicy{
		Mode:                agentpb.RestartPolicyMode_ON_FAILURE,
		OnFailureMaxRetries: -5,
	}
	policy, maxRetries, ok := monitorPolicyInt(rp)
	if !ok {
		t.Fatal("monitorPolicyInt ok = false; want true")
	}
	if policy != RestartPolicyOnFailure {
		t.Errorf("policy = %d; want %d (OnFailure)", policy, RestartPolicyOnFailure)
	}
	if maxRetries != 0 {
		t.Errorf("maxRetries = %d; want 0 (clamped from -5)", maxRetries)
	}
}

// ---------- volume tests ----------

func TestListVolumes_Empty(t *testing.T) {
	tmp := t.TempDir()
	old := volumesDir
	volumesDir = tmp
	t.Cleanup(func() { volumesDir = old })

	cl, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	resp, err := cl.ListVolumes(context.Background(), &agentpb.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(resp.GetVolumes()) != 0 {
		t.Errorf("expected 0 volumes, got %d", len(resp.GetVolumes()))
	}
}

func TestListVolumes_WithVolumes(t *testing.T) {
	tmp := t.TempDir()
	old := volumesDir
	volumesDir = tmp
	t.Cleanup(func() { volumesDir = old })

	// Create two volume directories, one with a file inside.
	if err := os.MkdirAll(filepath.Join(tmp, "app-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "app-data", "test.db"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "other-vol"), 0o755); err != nil {
		t.Fatal(err)
	}

	cl, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	resp, err := cl.ListVolumes(context.Background(), &agentpb.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(resp.GetVolumes()) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(resp.GetVolumes()))
	}

	// Find the volume with data.
	var found bool
	for _, v := range resp.GetVolumes() {
		if v.GetName() == "app-data" {
			found = true
			if v.GetSizeBytes() != 5 {
				t.Errorf("app-data size = %d, want 5", v.GetSizeBytes())
			}
			if v.GetPath() != filepath.Join(tmp, "app-data") {
				t.Errorf("app-data path = %q", v.GetPath())
			}
			if v.GetCreatedAt() == "" {
				t.Error("app-data created_at should not be empty")
			}
		}
	}
	if !found {
		t.Error("app-data volume not found in response")
	}
}

func TestRemoveVolume_Success(t *testing.T) {
	tmp := t.TempDir()
	old := volumesDir
	volumesDir = tmp
	t.Cleanup(func() { volumesDir = old })

	volPath := filepath.Join(tmp, "my-vol")
	if err := os.MkdirAll(volPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cl, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := cl.RemoveVolume(context.Background(), &agentpb.RemoveVolumeRequest{Name: "my-vol"})
	if err != nil {
		t.Fatalf("RemoveVolume: %v", err)
	}

	if _, err := os.Stat(volPath); !os.IsNotExist(err) {
		t.Error("volume directory should have been removed")
	}
}

func TestRemoveVolume_NotFound(t *testing.T) {
	tmp := t.TempDir()
	old := volumesDir
	volumesDir = tmp
	t.Cleanup(func() { volumesDir = old })

	cl, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := cl.RemoveVolume(context.Background(), &agentpb.RemoveVolumeRequest{Name: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent volume")
	}
}

func TestRemoveVolume_InvalidName(t *testing.T) {
	cl, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	for _, name := range []string{"", ".", "..", "/"} {
		_, err := cl.RemoveVolume(context.Background(), &agentpb.RemoveVolumeRequest{Name: name})
		if err == nil {
			t.Errorf("expected error for invalid name %q", name)
		}
	}
}

func TestDirSize(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "sub", "b.txt"), []byte("world!"), 0o644); err != nil {
		t.Fatal(err)
	}

	size := dirSize(tmp)
	if size != 11 { // "hello" (5) + "world!" (6)
		t.Errorf("dirSize = %d, want 11", size)
	}
}

func TestListContainerStats(t *testing.T) {
	stats := []*agentpb.ContainerStats{
		{AppName: "app-one", MemoryBytes: 42_000_000, StorageBytes: 128_000_000},
		{AppName: "app-two", MemoryBytes: 18_000_000, StorageBytes: 96_000_000},
	}
	mock := &mockContainerdClient{}
	mock.statsResult = stats
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	resp, err := client.ListContainerStats(context.Background(), &agentpb.ListContainerStatsRequest{})
	if err != nil {
		t.Fatalf("ListContainerStats: %v", err)
	}
	if len(resp.Stats) != 2 {
		t.Fatalf("len(Stats) = %d, want 2", len(resp.Stats))
	}
	if resp.Stats[0].AppName != "app-one" {
		t.Errorf("Stats[0].AppName = %q, want app-one", resp.Stats[0].AppName)
	}
	if resp.Stats[0].MemoryBytes != 42_000_000 {
		t.Errorf("Stats[0].MemoryBytes = %d, want 42000000", resp.Stats[0].MemoryBytes)
	}
	if resp.Stats[1].StorageBytes != 96_000_000 {
		t.Errorf("Stats[1].StorageBytes = %d, want 96000000", resp.Stats[1].StorageBytes)
	}
}

func TestListContainerStats_Error(t *testing.T) {
	mock := &mockContainerdClient{statsErr: fmt.Errorf("cgroup unavailable")}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	_, err := client.ListContainerStats(context.Background(), &agentpb.ListContainerStatsRequest{})
	if err == nil {
		t.Fatal("expected error from ListContainerStats")
	}
}

// ---------- StreamMCP tests ----------

func TestStreamMCP(t *testing.T) {
	// Start a real TCP server that echoes bytes back, acting as a fake MCP server.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting echo listener: %v", err)
	}
	defer echoLn.Close()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port

	go func() {
		conn, err := echoLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	mock := &mockContainerdClient{
		mcpPort: uint32(echoPort),
		containers: []*agentpb.AppContainer{
			{AppName: "test-app", RunningState: agentpb.AppRunningState_RUNNING, McpPort: uint32(echoPort)},
		},
	}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	md := metadata.Pairs("app-name", "test-app")
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := client.StreamMCP(ctx)
	if err != nil {
		t.Fatalf("StreamMCP: %v", err)
	}

	payload := []byte("hello mcp")
	if err := stream.Send(&agentpb.MCPChunk{Data: payload}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if string(resp.Data) != string(payload) {
		t.Fatalf("expected %q, got %q", payload, resp.Data)
	}
	_ = stream.CloseSend()
}

func TestStreamMCP_NoEntitlement(t *testing.T) {
	mock := &mockContainerdClient{mcpPort: 0}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	md := metadata.Pairs("app-name", "test-app")
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := client.StreamMCP(ctx)
	if err != nil {
		t.Fatalf("StreamMCP: %v", err)
	}

	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for container with no MCP entitlement")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestStreamMCP_ContainerNotRunning(t *testing.T) {
	mock := &mockContainerdClient{
		mcpPort: 3000,
		containers: []*agentpb.AppContainer{
			{AppName: "test-app", RunningState: agentpb.AppRunningState_STOPPED, McpPort: 3000},
		},
	}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	md := metadata.Pairs("app-name", "test-app")
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := client.StreamMCP(ctx)
	if err != nil {
		t.Fatalf("StreamMCP: %v", err)
	}

	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for stopped container")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}
