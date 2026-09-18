package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// ---------- mock containerd client ----------

type mockContainerdClient struct {
	containers            []*agentpb.AppContainer
	listErr               error
	stopErr               error
	deleteErr             error
	layers                []*agentpb.LayerHeader
	listLayersErr         error
	writeLayerErr         error
	writtenDigest         string
	writtenData           []byte
	createErr             error
	progressPhases        []agentpb.CreateContainerProgress_Phase
	startOutputCh         chan ContainerOutput
	startErr              error
	statsResult           []*agentpb.ContainerStats
	statsErr              error
	mcpPort               uint32
	mcpPortErr            error
	restartPolicyLabel    string
	restartPolicyLabelErr error
	resourceStats         []*agentpb.ResourceContainerStats
	resourceStatsErr      error
	listeningPorts        []*agentpb.PortEntry
	listeningPortsErr     error
	stoppedByUserCalls    []stoppedByUserCall
	stopCalls             []string
	resolveCalls          []string
	resolveErr            error
	declaredVolumes       map[string][]string
	declaredVolumesErr    error
	assembleImageCalls    int
	createReqs            []*agentpb.CreateContainerRequest
}

type stoppedByUserCall struct {
	containerID string
	stopped     bool
}

func (m *mockContainerdClient) ListBootContainers(_ context.Context) ([]BootContainer, error) {
	return nil, nil
}

func (m *mockContainerdClient) SetStoppedByUser(_ context.Context, containerID string, stopped bool) error {
	m.stoppedByUserCalls = append(m.stoppedByUserCalls, stoppedByUserCall{containerID, stopped})
	return nil
}

func (m *mockContainerdClient) MigrateStoppedByUserOnce(_ context.Context) error {
	return nil
}

func (m *mockContainerdClient) ListContainers(_ context.Context) ([]*agentpb.AppContainer, error) {
	return m.containers, m.listErr
}
func (m *mockContainerdClient) AppDeclaredVolumes(_ context.Context) (map[string][]string, error) {
	return m.declaredVolumes, m.declaredVolumesErr
}
func (m *mockContainerdClient) StopContainer(_ context.Context, name string) error {
	m.stopCalls = append(m.stopCalls, name)
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
func (m *mockContainerdClient) AssembleImage(_ context.Context, _ string, _ []*agentpb.RunContainerLayerHeader, _ []byte) error {
	m.assembleImageCalls++
	return nil
}
func (m *mockContainerdClient) CreateContainer(_ context.Context, req *agentpb.CreateContainerRequest, _ *appconfig.AppConfig) error {
	m.createReqs = append(m.createReqs, req)
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

func (m *mockContainerdClient) GetResourceStats(_ context.Context) ([]*agentpb.ResourceContainerStats, error) {
	return m.resourceStats, m.resourceStatsErr
}
func (m *mockContainerdClient) GetListeningPorts(_ context.Context, _ string) ([]*agentpb.PortEntry, error) {
	return m.listeningPorts, m.listeningPortsErr
}

func (m *mockContainerdClient) GetContainerMetrics(_ context.Context, _ string) (ContainerMetrics, error) {
	return ContainerMetrics{}, nil
}

func (m *mockContainerdClient) GetContainerMCPPort(_ context.Context, _ string) (uint32, error) {
	return m.mcpPort, m.mcpPortErr
}

func (m *mockContainerdClient) GetContainerRestartPolicyLabel(_ context.Context, _ string) (string, error) {
	return m.restartPolicyLabel, m.restartPolicyLabelErr
}

func (m *mockContainerdClient) ContainerIDsForApp(_ context.Context, appID string) ([]string, error) {
	return []string{appID}, nil
}

// mockContainerIDs mirrors appconfig.AppConfig.ContainerName.
func mockContainerIDs(c *agentpb.AppContainer) []string {
	svcs := c.GetServices()
	if len(svcs) <= 1 {
		return []string{c.GetAppName()}
	}
	ids := make([]string, 0, len(svcs))
	for _, s := range svcs {
		ids = append(ids, c.GetAppName()+"_"+s.GetName())
	}
	return ids
}

// ResolveAppContainerIDs mirrors the real resolver: an exact container ID
// first, then the app-id, then NotFound. Faithful on purpose — a mock that
// accepts any name cannot observe a name that resolves to nothing.
func (m *mockContainerdClient) ResolveAppContainerIDs(_ context.Context, name string) ([]string, error) {
	m.resolveCalls = append(m.resolveCalls, name)
	if m.resolveErr != nil {
		return nil, m.resolveErr
	}
	// Rule 1 wins globally, so scan all apps for an ID match first.
	for _, c := range m.containers {
		for _, id := range mockContainerIDs(c) {
			if id == name {
				return []string{id}, nil
			}
		}
	}
	for _, c := range m.containers {
		if c.GetAppName() == name {
			return mockContainerIDs(c), nil
		}
	}
	return nil, fmt.Errorf("%w: no app or service named %q", errdefs.ErrNotFound, name)
}

func (m *mockContainerdClient) MissingChunks(_ context.Context, hashes [][32]byte) ([][32]byte, error) {
	return hashes, nil
}

func (m *mockContainerdClient) PresentLayers(_ context.Context, _ []string) (map[string]int64, error) {
	return nil, nil
}

func (m *mockContainerdClient) StageChunk(_ context.Context, _ [32]byte, _ []byte) error {
	return nil
}

func (m *mockContainerdClient) AssembleLayerFromChunks(_ context.Context, _ string, _ [][32]byte) error {
	return nil
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
	return startContainerServerWithOptions(t, client)
}

// startContainerServerWithOptions is startContainerServer plus the ability to
// pass ContainerServiceOptions (e.g. WithContainerStorageGate) through to
// NewContainerService, for tests that need to configure the service under
// test beyond its defaults.
func startContainerServerWithOptions(t *testing.T, client ContainerdClient, opts ...ContainerServiceOption) (agentpb.WendyContainerServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	logger := zap.NewNop()
	svc := NewContainerService(logger, client, opts...)
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

// degradedContainerStorageGateForTest returns a *ContainerStorageGate that
// reports Degraded() == true unconditionally, for tests asserting that
// ingestion RPCs are refused (WDY-3127).
func degradedContainerStorageGateForTest() *ContainerStorageGate {
	return newContainerStorageGate(zap.NewNop(),
		func() bool { return true },                                      // isWendyOS
		func(string) bool { return true },                                // unitLoaded
		func() bool { return true },                                      // probe
		func() (partitionUsage, bool) { return partitionUsage{}, false }, // describeUsage
	)
}

// healthyContainerStorageGateForTest returns a *ContainerStorageGate that is
// genuinely expected (WendyOS host, bind unit loaded) but never degraded
// (probe always reports containerd off root), for tests asserting that a
// healthy gate never blocks ingestion. Using isWendyOS=false here would only
// re-test the permanent no-op path (see TestContainerStorageGateNoopOffWendyOS)
// instead of the expected=true && !degraded branch this helper is meant to
// exercise (WDY-3127 M-D2).
func healthyContainerStorageGateForTest() *ContainerStorageGate {
	return newContainerStorageGate(zap.NewNop(),
		func() bool { return true },
		func(string) bool { return true },
		func() bool { return false },
		func() (partitionUsage, bool) { return partitionUsage{}, false },
	)
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
	client, cleanup := startContainerServer(t, &mockContainerdClient{
		containers: []*agentpb.AppContainer{{AppName: "test-app"}},
	})
	defer cleanup()

	_, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{
		AppName: "test-app",
	})
	if err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
}

// TestStopContainer_PersistsStoppedByUser verifies a deliberate stop is
// recorded as a container label so the boot reconcile won't undo it on reboot.
func TestStopContainer_PersistsStoppedByUser(t *testing.T) {
	mock := &mockContainerdClient{
		containers: []*agentpb.AppContainer{{AppName: "test-app"}},
	}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	if _, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{AppName: "test-app"}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	found := false
	for _, c := range mock.stoppedByUserCalls {
		if c.containerID == "test-app" && c.stopped {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected SetStoppedByUser(test-app, true); calls = %+v", mock.stoppedByUserCalls)
	}
}

func TestStopContainer_RollsBackStoppedByUserWhenStopFails(t *testing.T) {
	mock := &mockContainerdClient{
		containers: []*agentpb.AppContainer{{AppName: "test-app"}},
		stopErr:    errors.New("stop failed"),
	}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	if _, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{AppName: "test-app"}); err == nil {
		t.Fatal("StopContainer error = nil; want failure")
	}
	want := []stoppedByUserCall{{containerID: "test-app", stopped: true}, {containerID: "test-app", stopped: false}}
	if !reflect.DeepEqual(mock.stoppedByUserCalls, want) {
		t.Fatalf("SetStoppedByUser calls = %+v; want %+v", mock.stoppedByUserCalls, want)
	}
}

func TestDeleteContainer(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{
		containers: []*agentpb.AppContainer{{AppName: "test-app"}},
	})
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
	client, cleanup := startContainerServer(t, &mockContainerdClient{
		containers: []*agentpb.AppContainer{{AppName: "test-app"}},
	})
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

	if err := stream.Send(&agentpb.WriteLayerRequest{Digest: digest, Data: data}); err != nil {
		t.Fatalf("send chunk1: %v", err)
	}
	if err := stream.Send(&agentpb.WriteLayerRequest{Data: data2}); err != nil {
		t.Fatalf("send chunk2: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	if _, err = stream.Recv(); err != nil {
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

// TestWriteLayerAlreadyExists verifies that WriteLayer succeeds and drains the
// gRPC stream even when the content store returns without consuming the reader
// (e.g. the blob already exists).
func TestWriteLayerAlreadyExists(t *testing.T) {
	// drainlessMock returns nil without reading from the reader, simulating
	// the AlreadyExists fast-path in the containerd content store.
	mock := &drainlessMockContainerdClient{mockContainerdClient: mockContainerdClient{}}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.WriteLayer(ctx)
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	digest := "sha256:alreadyexists"
	if err := stream.Send(&agentpb.WriteLayerRequest{Digest: digest, Data: []byte("chunk1")}); err != nil {
		t.Fatalf("send chunk1: %v", err)
	}
	if err := stream.Send(&agentpb.WriteLayerRequest{Data: []byte("chunk2")}); err != nil {
		t.Fatalf("send chunk2: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	// RPC must succeed without deadlock even though the reader was not consumed.
	if _, err = stream.Recv(); err != nil {
		t.Fatalf("recv response: %v", err)
	}
}

// drainlessMockContainerdClient overrides WriteLayer to return without reading
// the reader, simulating the content-store AlreadyExists path.
type drainlessMockContainerdClient struct {
	mockContainerdClient
}

func (m *drainlessMockContainerdClient) WriteLayer(_ context.Context, digest string, _ io.Reader, _ int64) error {
	m.writtenDigest = digest
	return nil
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
	clearStopCalls    []string
	unregisterCalls   []string
}

type registerCall struct {
	appName    string
	policy     int
	maxRetries int
}

func (m *mockMonitorRegistrar) Register(appName string, policy int, maxRetries int) {
	m.registerCalls = append(m.registerCalls, registerCall{appName, policy, maxRetries})
}

func (m *mockMonitorRegistrar) Unregister(appName string) {
	m.unregisterCalls = append(m.unregisterCalls, appName)
}

func (m *mockMonitorRegistrar) MarkExplicitStop(appName string) {
	m.explicitStopCalls = append(m.explicitStopCalls, appName)
}

func (m *mockMonitorRegistrar) ClearExplicitStop(appName string) {
	m.clearStopCalls = append(m.clearStopCalls, appName)
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
			t.Fatalf("unexpected recv error: %v", err)
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
	mc := &mockContainerdClient{
		containers: []*agentpb.AppContainer{{AppName: "my-app"}},
	}
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

// multiServiceMock deploys one app with containers "myapp_alpha"/"myapp_beta".
func multiServiceMock() *mockContainerdClient {
	return &mockContainerdClient{
		containers: []*agentpb.AppContainer{{
			AppName: "myapp",
			Services: []*agentpb.ServiceEntry{
				{Name: "alpha"},
				{Name: "beta"},
			},
		}},
	}
}

// A service-qualified name matches no app-id label, so label-only resolution
// found nothing, invented the ID back, and reported a stop it never issued.
func TestStopContainer_ServiceQualifiedName_StopsOnlyThatService(t *testing.T) {
	mc := multiServiceMock()
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	if _, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{
		AppName: "myapp_alpha",
	}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	// The assertion the old code fails: it resolved via ContainerIDsForApp and
	// fell back to the name itself, leaving everything below looking correct.
	if len(mc.resolveCalls) != 1 || mc.resolveCalls[0] != "myapp_alpha" {
		t.Errorf("ResolveAppContainerIDs calls = %v; want [myapp_alpha]", mc.resolveCalls)
	}
	if len(mc.stopCalls) != 1 || mc.stopCalls[0] != "myapp_alpha" {
		t.Errorf("stopCalls = %v; want [myapp_alpha]", mc.stopCalls)
	}
	if len(mon.explicitStopCalls) != 1 || mon.explicitStopCalls[0] != "myapp_alpha" {
		t.Errorf("MarkExplicitStop = %v; want [myapp_alpha]", mon.explicitStopCalls)
	}
	if len(mc.stoppedByUserCalls) != 1 {
		t.Fatalf("stoppedByUserCalls = %v; want exactly one", mc.stoppedByUserCalls)
	}
	if got := mc.stoppedByUserCalls[0]; got.containerID != "myapp_alpha" || !got.stopped {
		t.Errorf("stoppedByUser = %+v; want {myapp_alpha true}", got)
	}
}

// The group form must still fan out to every service.
func TestStopContainer_BareAppID_StopsEveryService(t *testing.T) {
	mc := multiServiceMock()
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	if _, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{
		AppName: "myapp",
	}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	if len(mon.explicitStopCalls) != 2 {
		t.Errorf("MarkExplicitStop = %v; want both services", mon.explicitStopCalls)
	}
	marked := map[string]bool{}
	for _, c := range mc.stoppedByUserCalls {
		marked[c.containerID] = c.stopped
	}
	if !marked["myapp_alpha"] || !marked["myapp_beta"] {
		t.Errorf("stoppedByUser = %+v; want both services marked", mc.stoppedByUserCalls)
	}
}

// A name that resolves to nothing must error and leave no trace.
func TestStopContainer_UnknownName_NotFound(t *testing.T) {
	mc := multiServiceMock()
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	_, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{
		AppName: "no-such-app",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("StopContainer error = %v; want NotFound", err)
	}
	if len(mc.stopCalls) != 0 {
		t.Errorf("stopCalls = %v; want none", mc.stopCalls)
	}
	if len(mon.explicitStopCalls) != 0 {
		t.Errorf("MarkExplicitStop = %v; want none", mon.explicitStopCalls)
	}
	if len(mc.stoppedByUserCalls) != 0 {
		t.Errorf("stoppedByUserCalls = %v; want none", mc.stoppedByUserCalls)
	}
}

// Delete must resolve through the shared resolver and unregister only the
// named service.
func TestDeleteContainer_ServiceQualifiedName_ResolvesOneService(t *testing.T) {
	mc := multiServiceMock()
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	if _, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName: "myapp_alpha",
	}); err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}

	if len(mc.resolveCalls) != 1 || mc.resolveCalls[0] != "myapp_alpha" {
		t.Errorf("ResolveAppContainerIDs calls = %v; want [myapp_alpha]", mc.resolveCalls)
	}
	if len(mon.unregisterCalls) != 1 || mon.unregisterCalls[0] != "myapp_alpha" {
		t.Errorf("Unregister = %v; want [myapp_alpha]", mon.unregisterCalls)
	}
	if len(mon.explicitStopCalls) != 0 {
		t.Errorf("MarkExplicitStop = %v; want none on the delete path", mon.explicitStopCalls)
	}
}

// Delete carried the same invented-ID fallback as stop.
func TestDeleteContainer_UnknownName_NotFound(t *testing.T) {
	mc := multiServiceMock()
	client, cleanup := startContainerServer(t, mc)
	defer cleanup()

	_, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName: "no-such-app",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("DeleteContainer error = %v; want NotFound", err)
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

// TestStartContainer_RegistersMonitor_FromPersistedLabel verifies that when
// StartContainer is called without a restart policy, the service reads the
// persisted policy label from containerd and still registers with the monitor.
func TestStartContainer_RegistersMonitor_FromPersistedLabel(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	outputCh <- ContainerOutput{Done: true}
	close(outputCh)

	mc := &mockContainerdClient{
		startOutputCh:      outputCh,
		restartPolicyLabel: "unless-stopped",
	}
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	// StartContainer with no restart policy — should fall back to the label.
	stream, err := client.StartContainer(context.Background(), &agentpb.StartContainerRequest{
		AppName: "my-app",
		// RestartPolicy intentionally omitted.
	})
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected recv error: %v", err)
		}
	}

	if len(mon.registerCalls) != 1 {
		t.Fatalf("Register call count = %d; want 1 (from persisted label)", len(mon.registerCalls))
	}
	got := mon.registerCalls[0]
	if got.appName != "my-app" {
		t.Errorf("Register appName = %q; want my-app", got.appName)
	}
	if got.policy != RestartPolicyUnlessStopped {
		t.Errorf("Register policy = %d; want %d (UnlessStopped)", got.policy, RestartPolicyUnlessStopped)
	}
}

// TestStartContainer_ClearsExplicitStop verifies that a successful StartContainer
// call clears any prior ExplicitStop mark, re-enabling automatic restarts.
func TestStartContainer_ClearsExplicitStop(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	outputCh <- ContainerOutput{Done: true}
	close(outputCh)

	mc := &mockContainerdClient{startOutputCh: outputCh}
	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	stream, err := client.StartContainer(context.Background(), &agentpb.StartContainerRequest{
		AppName: "my-app",
	})
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected recv error: %v", err)
		}
	}

	// ClearExplicitStop must have been called once.
	if len(mon.clearStopCalls) != 1 {
		t.Fatalf("ClearExplicitStop call count = %d; want 1", len(mon.clearStopCalls))
	}
	if mon.clearStopCalls[0] != "my-app" {
		t.Errorf("ClearExplicitStop appName = %q; want my-app", mon.clearStopCalls[0])
	}
}

// TestMonitorPolicyIntFromLabel covers the label-based policy parser.
func TestMonitorPolicyIntFromLabel(t *testing.T) {
	cases := []struct {
		label       string
		wantPolicy  int
		wantOK      bool
		wantRetries int
	}{
		{"", RestartPolicyNo, false, 0},
		{"no", RestartPolicyNo, false, 0},
		{"unless-stopped", RestartPolicyUnlessStopped, true, 0},
		{"always", RestartPolicyAlways, true, 0},
		{"on-failure", RestartPolicyOnFailure, true, 0},
		{"on-failure:3", RestartPolicyOnFailure, true, 3},
		{"on-failure:foo", 0, false, 0},
		{"on-failure:-1", 0, false, 0},
		{"unknown-policy", 0, false, 0},
	}
	for _, tc := range cases {
		policy, maxRetries, ok := monitorPolicyIntFromLabel(tc.label)
		if ok != tc.wantOK {
			t.Errorf("label=%q ok=%v want %v", tc.label, ok, tc.wantOK)
		}
		if ok && policy != tc.wantPolicy {
			t.Errorf("label=%q policy=%d want %d", tc.label, policy, tc.wantPolicy)
		}
		if ok && maxRetries != tc.wantRetries {
			t.Errorf("label=%q maxRetries=%d want %d", tc.label, maxRetries, tc.wantRetries)
		}
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

// TestListVolumes_UsedByFromDeclaredVolumes verifies that usedBy comes from
// declared persist entitlements, not name prefixes (WDY-1807): a declared
// volume is attributed to its app regardless of its name, a foreign volume
// whose name merely starts with the app ID is not, and a volume declared by
// two apps lists both.
func TestListVolumes_UsedByFromDeclaredVolumes(t *testing.T) {
	tmp := t.TempDir()
	old := volumesDir
	volumesDir = tmp
	t.Cleanup(func() { volumesDir = old })

	for _, dir := range []string{"autotest-churn-data", "autotest-orphanvol", "hellovlm-models", "shared-cache", "leaked-vol"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mock := &mockContainerdClient{
		declaredVolumes: map[string][]string{
			"autotest-churn":             {"autotest-churn-data", "autotest-orphanvol", "shared-cache"},
			"sh.wendy.examples.hellovlm": {"hellovlm-models", "shared-cache"},
		},
	}
	cl, cleanup := startContainerServer(t, mock)
	defer cleanup()

	resp, err := cl.ListVolumes(context.Background(), &agentpb.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}

	usedBy := make(map[string][]string)
	for _, v := range resp.GetVolumes() {
		usedBy[v.GetName()] = v.GetUsedBy()
	}

	wants := map[string][]string{
		"autotest-churn-data": {"autotest-churn"},
		// Declared but unprefixed: must still be attributed (was invisible before).
		"autotest-orphanvol": {"autotest-churn"},
		// Prefix collision with app "hellovlm": owned by the declaring app only.
		"hellovlm-models": {"sh.wendy.examples.hellovlm"},
		// Shared by name across two apps.
		"shared-cache": {"autotest-churn", "sh.wendy.examples.hellovlm"},
		// Declared by nobody: orphan, empty usedBy.
		"leaked-vol": nil,
	}
	for vol, want := range wants {
		got := usedBy[vol]
		if len(got) != len(want) {
			t.Errorf("usedBy[%q] = %v, want %v", vol, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("usedBy[%q] = %v, want %v", vol, got, want)
				break
			}
		}
	}
}

// deleteVolumesTestSetup creates volume dirs and starts a server whose mock
// reports the given declared-volumes map.
// deployedApps names the apps that exist on the fake device; a delete must
// resolve before it reaches the volume logic.
func deleteVolumesTestSetup(t *testing.T, dirs []string, declared map[string][]string, declaredErr error, deployedApps ...string) (agentpb.WendyContainerServiceClient, string) {
	t.Helper()
	tmp := t.TempDir()
	old := volumesDir
	volumesDir = tmp
	t.Cleanup(func() { volumesDir = old })

	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctrs := make([]*agentpb.AppContainer, 0, len(deployedApps))
	for _, app := range deployedApps {
		ctrs = append(ctrs, &agentpb.AppContainer{AppName: app})
	}
	mock := &mockContainerdClient{containers: ctrs, declaredVolumes: declared, declaredVolumesErr: declaredErr}
	cl, cleanup := startContainerServer(t, mock)
	t.Cleanup(cleanup)
	return cl, tmp
}

func mustExist(t *testing.T, tmp string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(tmp, dir)); err != nil {
			t.Errorf("volume %q should still exist: %v", dir, err)
		}
	}
}

func mustNotExist(t *testing.T, tmp string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(tmp, dir)); !os.IsNotExist(err) {
			t.Errorf("volume %q should have been deleted (err=%v)", dir, err)
		}
	}
}

// TestDeleteContainer_DeleteVolumes_DeclaredNotPrefixed verifies the leak
// direction of WDY-1807: --delete-volumes removes exactly what the app
// declared, including volumes whose names don't start with the app ID.
func TestDeleteContainer_DeleteVolumes_DeclaredNotPrefixed(t *testing.T) {
	cl, tmp := deleteVolumesTestSetup(t,
		[]string{"autotest-churn-data", "autotest-orphanvol", "hellovlm-models"},
		map[string][]string{
			"autotest-churn":             {"autotest-churn-data", "autotest-orphanvol"},
			"sh.wendy.examples.hellovlm": {"hellovlm-models"},
		}, nil, "autotest-churn")

	_, err := cl.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName:       "autotest-churn",
		DeleteVolumes: true,
	})
	if err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}

	mustNotExist(t, tmp, "autotest-churn-data", "autotest-orphanvol")
	mustExist(t, tmp, "hellovlm-models")
}

// TestDeleteContainer_DeleteVolumes_PrefixCollision verifies the cross-delete
// direction of WDY-1807: removing app "hellovlm" must not delete
// "hellovlm-models", which belongs to a different app that happens to share
// the name prefix.
func TestDeleteContainer_DeleteVolumes_PrefixCollision(t *testing.T) {
	cl, tmp := deleteVolumesTestSetup(t,
		[]string{"hellovlm-data", "hellovlm-models"},
		map[string][]string{
			"hellovlm":                   {"hellovlm-data"},
			"sh.wendy.examples.hellovlm": {"hellovlm-models"},
		}, nil, "hellovlm")

	_, err := cl.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName:       "hellovlm",
		DeleteVolumes: true,
	})
	if err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}

	mustNotExist(t, tmp, "hellovlm-data")
	mustExist(t, tmp, "hellovlm-models")
}

// TestDeleteContainer_DeleteVolumes_SharedVolumeKept: a volume declared by
// another deployed app too is shared by name and must survive.
func TestDeleteContainer_DeleteVolumes_SharedVolumeKept(t *testing.T) {
	cl, tmp := deleteVolumesTestSetup(t,
		[]string{"app-a-data", "shared-cache"},
		map[string][]string{
			"app-a": {"app-a-data", "shared-cache"},
			"app-b": {"shared-cache"},
		}, nil, "app-a")

	_, err := cl.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName:       "app-a",
		DeleteVolumes: true,
	})
	if err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}

	mustNotExist(t, tmp, "app-a-data")
	mustExist(t, tmp, "shared-cache")
}

// TestDeleteContainer_DeleteVolumes_OwnershipUnknown: when ownership cannot be
// resolved (lookup error, or an app deployed before entitlement labels
// existed), nothing is deleted — fail safe, even for prefix-matching names.
func TestDeleteContainer_DeleteVolumes_OwnershipUnknown(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		cl, tmp := deleteVolumesTestSetup(t,
			[]string{"legacy-app-data"},
			nil, fmt.Errorf("containerd unavailable"), "legacy-app")

		_, err := cl.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
			AppName:       "legacy-app",
			DeleteVolumes: true,
		})
		if err != nil {
			t.Fatalf("DeleteContainer: %v", err)
		}
		mustExist(t, tmp, "legacy-app-data")
	})

	t.Run("no persist labels", func(t *testing.T) {
		cl, tmp := deleteVolumesTestSetup(t,
			[]string{"legacy-app-data"},
			map[string][]string{}, nil, "legacy-app")

		_, err := cl.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
			AppName:       "legacy-app",
			DeleteVolumes: true,
		})
		if err != nil {
			t.Fatalf("DeleteContainer: %v", err)
		}
		mustExist(t, tmp, "legacy-app-data")
	})
}

// TestDeleteVolumes_UnsafeNames: names that would escape the volumes dir are
// skipped even if they somehow appear in a declared-volumes list.
func TestDeleteVolumes_UnsafeNames(t *testing.T) {
	tmp := t.TempDir()
	old := volumesDir
	volumesDir = tmp
	t.Cleanup(func() { volumesDir = old })

	if err := os.MkdirAll(filepath.Join(tmp, "safe-vol"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "..", "outside-"+filepath.Base(tmp))
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	svc := NewContainerService(zap.NewNop(), &mockContainerdClient{})
	svc.deleteVolumes([]string{"../" + filepath.Base(outside), ".", "..", "", "/", "safe-vol"})

	if _, err := os.Stat(outside); err != nil {
		t.Errorf("path outside volumes dir should not have been touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "safe-vol")); !os.IsNotExist(err) {
		t.Error("safe-vol should have been deleted")
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

// groupMock overrides ContainerIDsForApp to simulate a multi-service app.
type groupMock struct {
	mockContainerdClient
	containerIDs []string
	startedNames []string
}

func (m *groupMock) ContainerIDsForApp(_ context.Context, _ string) ([]string, error) {
	return m.containerIDs, nil
}

func (m *groupMock) StartContainer(_ context.Context, name, _ string, _ *agentpb.RestartPolicy) (<-chan ContainerOutput, error) {
	m.startedNames = append(m.startedNames, name)
	ch := make(chan ContainerOutput, 1)
	ch <- ContainerOutput{Done: true}
	close(ch)
	return ch, nil
}

func TestStartContainer_GroupStartsAllServices(t *testing.T) {
	mc := &groupMock{
		containerIDs: []string{"com.example.robot_camera", "com.example.robot_detector"},
	}
	mc.startOutputCh = make(chan ContainerOutput)
	close(mc.startOutputCh)

	mon := &mockMonitorRegistrar{}
	client, cleanup := startContainerServerWithMonitor(t, mc, mon)
	defer cleanup()

	stream, err := client.StartContainer(context.Background(), &agentpb.StartContainerRequest{
		AppName: "com.example.robot",
	})
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	// Expect a single Started response followed by EOF.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if resp.GetStarted() == nil {
		t.Fatalf("expected Started response, got %T", resp.GetResponseType())
	}
	if _, err = stream.Recv(); err != io.EOF {
		t.Fatalf("expected EOF after Started, got %v", err)
	}

	// Both service containers must have been started individually.
	if len(mc.startedNames) != 2 {
		t.Fatalf("expected 2 StartContainer calls, got %d: %v", len(mc.startedNames), mc.startedNames)
	}
	if mc.startedNames[0] != "com.example.robot_camera" {
		t.Errorf("first started = %q; want com.example.robot_camera", mc.startedNames[0])
	}
	if mc.startedNames[1] != "com.example.robot_detector" {
		t.Errorf("second started = %q; want com.example.robot_detector", mc.startedNames[1])
	}

	// ClearExplicitStop must be called once per service.
	if len(mon.clearStopCalls) != 2 {
		t.Fatalf("ClearExplicitStop call count = %d; want 2", len(mon.clearStopCalls))
	}
}

func TestParseAppConfigRejectsUnsafeAppID(t *testing.T) {
	// A comma in appId would corrupt OTEL_RESOURCE_ATTRIBUTES once injected.
	_, err := parseAppConfig([]byte(`{"appId":"bad,id"}`))
	if err == nil {
		t.Fatal("expected error for unsafe appId, got nil")
	}
}

func TestParseAppConfigAcceptsValidAppID(t *testing.T) {
	cfg, err := parseAppConfig([]byte(`{"appId":"com.example.app"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AppID != "com.example.app" {
		t.Fatalf("appId = %q, want %q", cfg.AppID, "com.example.app")
	}
}

func TestGetResourceStatsHandler(t *testing.T) {
	svc := &ContainerService{
		logger: zap.NewNop(),
		containerd: &mockContainerdClient{
			resourceStats: []*agentpb.ResourceContainerStats{
				{AppName: "myapp", CpuUsageNanos: 5000, MemoryBytes: 2048},
			},
		},
	}
	resp, err := svc.GetResourceStats(context.Background(), &agentpb.GetResourceStatsRequest{})
	if err != nil {
		t.Fatalf("GetResourceStats: %v", err)
	}
	if len(resp.GetContainers()) != 1 || resp.GetContainers()[0].GetAppName() != "myapp" {
		t.Fatalf("unexpected containers: %+v", resp.GetContainers())
	}
	if resp.GetContainers()[0].GetCpuUsageNanos() != 5000 {
		t.Errorf("cpu = %d, want 5000", resp.GetContainers()[0].GetCpuUsageNanos())
	}
	// Host is populated best-effort; on a non-Linux test host the /proc reads may
	// fail, so we only assert the host message is present (non-nil).
	if resp.GetHost() == nil {
		t.Errorf("host stats missing")
	}
}

func TestGetContainerPortsHandler(t *testing.T) {
	svc := &ContainerService{
		logger: zap.NewNop(),
		containerd: &mockContainerdClient{
			listeningPorts: []*agentpb.PortEntry{
				{Protocol: "tcp", Port: 8080, Address: "0.0.0.0"},
			},
		},
	}
	resp, err := svc.GetContainerPorts(context.Background(), &agentpb.GetContainerPortsRequest{AppName: "myapp"})
	if err != nil {
		t.Fatalf("GetContainerPorts: %v", err)
	}
	if len(resp.GetPorts()) != 1 || resp.GetPorts()[0].GetPort() != 8080 {
		t.Fatalf("unexpected ports: %+v", resp.GetPorts())
	}

	// Empty app name is rejected.
	if _, err := svc.GetContainerPorts(context.Background(), &agentpb.GetContainerPortsRequest{}); err == nil {
		t.Errorf("expected error for empty app_name")
	}
}

func TestParseAppConfigEmptyDataStillAllowed(t *testing.T) {
	// Empty config is a pre-existing path (no appId); must not start erroring.
	cfg, err := parseAppConfig(nil)
	if err != nil {
		t.Fatalf("unexpected error for empty config: %v", err)
	}
	if cfg.AppID != "" {
		t.Fatalf("appId = %q, want empty", cfg.AppID)
	}
}

// TestRunContainer_ForwardsEnv covers WDY-2040: the layer/chunk deploy path
// applies caller env, the same as the registry-push path that reaches
// CreateContainer directly.
func TestRunContainer_ForwardsEnv(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	outputCh <- ContainerOutput{Done: true}
	close(outputCh)
	mock := &mockContainerdClient{startOutputCh: outputCh}

	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	want := []string{"FOO=bar", "OTEL_LOGS_EXPORTER=console"}
	stream, err := client.RunContainer(context.Background(), &agentpb.RunContainerLayersRequest{
		ImageName: "test-image:latest",
		AppName:   "env-app",
		Layers: []*agentpb.RunContainerLayerHeader{
			{Digest: "sha256:layerdigest", Size: 10, DiffId: "sha256:layerdiffid"},
		},
		ImageConfig: []byte(`{}`),
		Env:         want,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if err := drainRunContainerStream(stream); err != nil {
		t.Fatalf("RunContainer stream: %v", err)
	}

	if len(mock.createReqs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(mock.createReqs))
	}
	got := mock.createReqs[0].GetEnv()
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("env[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// ---------- ContainerStorageGate refusal (WDY-3127) ----------

// TestContainerServiceWriteLayerRefusedWhenStorageDegraded asserts WriteLayer
// refuses the RPC before touching containerd when the storage gate reports
// the host degraded.
func TestContainerServiceWriteLayerRefusedWhenStorageDegraded(t *testing.T) {
	mock := &mockContainerdClient{}
	client, cleanup := startContainerServerWithOptions(t, mock, WithContainerStorageGate(degradedContainerStorageGateForTest()))
	defer cleanup()

	stream, err := client.WriteLayer(context.Background())
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition; err = %v", status.Code(err), err)
	}
	if mock.writtenDigest != "" {
		t.Errorf("writtenDigest = %q, want empty; WriteLayer must not write to containerd while storage is degraded", mock.writtenDigest)
	}
}

// TestContainerServiceWriteChunksRefusedWhenStorageDegraded asserts WriteChunks
// refuses the RPC before staging any chunk when the storage gate reports the
// host degraded.
func TestContainerServiceWriteChunksRefusedWhenStorageDegraded(t *testing.T) {
	fake := newFakeContainerd()
	client, cleanup := startContainerServerWithOptions(t, fake, WithContainerStorageGate(degradedContainerStorageGateForTest()))
	defer cleanup()

	stream, err := client.WriteChunks(context.Background())
	if err != nil {
		t.Fatalf("WriteChunks: %v", err)
	}
	if _, err = stream.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition; err = %v", status.Code(err), err)
	}
	if len(fake.stagedChunks) != 0 {
		t.Errorf("stagedChunks = %d, want 0; WriteChunks must not stage a chunk while storage is degraded", len(fake.stagedChunks))
	}
}

// TestContainerServicePrepareImageRefusedWhenStorageDegraded asserts
// PrepareImage refuses the RPC before preparing anything when the storage
// gate reports the host degraded.
func TestContainerServicePrepareImageRefusedWhenStorageDegraded(t *testing.T) {
	mock := &imagePrepareMock{}
	client, cleanup := startContainerServerWithOptions(t, mock, WithContainerStorageGate(degradedContainerStorageGateForTest()))
	defer cleanup()

	_, err := client.PrepareImage(context.Background(), &agentpb.RunContainerLayersRequest{
		ImageName:   "test-image:latest",
		ImageConfig: []byte(`{}`),
		Layers: []*agentpb.RunContainerLayerHeader{
			{Digest: "sha256:layerdigest", Size: 10, DiffId: "sha256:layerdiffid"},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition; err = %v", status.Code(err), err)
	}
	if mock.prepareCalls != 0 {
		t.Errorf("prepareCalls = %d, want 0; PrepareImage must not prepare while storage is degraded", mock.prepareCalls)
	}
}

// TestContainerServiceRunContainerRefusedWhenStorageDegraded asserts
// RunContainer refuses the RPC before assembling or creating anything when
// the storage gate reports the host degraded.
func TestContainerServiceRunContainerRefusedWhenStorageDegraded(t *testing.T) {
	mock := &mockContainerdClient{}
	client, cleanup := startContainerServerWithOptions(t, mock, WithContainerStorageGate(degradedContainerStorageGateForTest()))
	defer cleanup()

	stream, err := client.RunContainer(context.Background(), &agentpb.RunContainerLayersRequest{
		ImageName: "test-image:latest",
		AppName:   "storage-degraded-app",
		Layers: []*agentpb.RunContainerLayerHeader{
			{Digest: "sha256:layerdigest", Size: 10, DiffId: "sha256:layerdiffid"},
		},
		ImageConfig: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if err := drainRunContainerStream(stream); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition; err = %v", status.Code(err), err)
	}
	if mock.assembleImageCalls != 0 {
		t.Errorf("assembleImageCalls = %d, want 0; RunContainer must not assemble the image while storage is degraded", mock.assembleImageCalls)
	}
	if len(mock.createReqs) != 0 {
		t.Errorf("createReqs = %d, want 0; RunContainer must not create the container while storage is degraded", len(mock.createReqs))
	}
}

// TestContainerServiceCreateContainerRefusedWhenStorageDegraded asserts
// CreateContainer refuses the RPC before creating anything when the storage
// gate reports the host degraded.
func TestContainerServiceCreateContainerRefusedWhenStorageDegraded(t *testing.T) {
	mock := &mockContainerdClient{}
	client, cleanup := startContainerServerWithOptions(t, mock, WithContainerStorageGate(degradedContainerStorageGateForTest()))
	defer cleanup()

	_, err := client.CreateContainer(context.Background(), &agentpb.CreateContainerRequest{
		ImageName: "test-image:latest",
		AppName:   "storage-degraded-app",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition; err = %v", status.Code(err), err)
	}
	if len(mock.createReqs) != 0 {
		t.Errorf("createReqs = %d, want 0; CreateContainer must not create while storage is degraded", len(mock.createReqs))
	}
}

// TestContainerServiceCreateContainerWithProgressRefusedWhenStorageDegraded
// asserts CreateContainerWithProgress refuses the RPC before sending any
// progress update or creating anything when the storage gate reports the
// host degraded.
func TestContainerServiceCreateContainerWithProgressRefusedWhenStorageDegraded(t *testing.T) {
	mock := &mockContainerdClient{}
	client, cleanup := startContainerServerWithOptions(t, mock, WithContainerStorageGate(degradedContainerStorageGateForTest()))
	defer cleanup()

	stream, err := client.CreateContainerWithProgress(context.Background(), &agentpb.CreateContainerRequest{
		ImageName: "test-image:latest",
		AppName:   "storage-degraded-app",
	})
	if err != nil {
		t.Fatalf("CreateContainerWithProgress: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition; err = %v", status.Code(err), err)
	}
	if len(mock.createReqs) != 0 {
		t.Errorf("createReqs = %d, want 0; CreateContainerWithProgress must not create while storage is degraded", len(mock.createReqs))
	}
}

// TestContainerServiceAllowsIngestionWhenGateHealthy asserts ingestion
// proceeds normally both when a healthy (non-degraded) gate is configured
// and when no gate is configured at all (nil storageGate).
func TestContainerServiceAllowsIngestionWhenGateHealthy(t *testing.T) {
	t.Run("healthy gate", func(t *testing.T) {
		mock := &mockContainerdClient{}
		client, cleanup := startContainerServerWithOptions(t, mock, WithContainerStorageGate(healthyContainerStorageGateForTest()))
		defer cleanup()

		stream, err := client.WriteLayer(context.Background())
		if err != nil {
			t.Fatalf("WriteLayer: %v", err)
		}
		digest := "sha256:healthygate"
		if err := stream.Send(&agentpb.WriteLayerRequest{Digest: digest, Data: []byte("data")}); err != nil {
			t.Fatalf("send: %v", err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		if _, err = stream.Recv(); err != nil {
			t.Fatalf("recv response: %v", err)
		}
		if mock.writtenDigest != digest {
			t.Errorf("writtenDigest = %q, want %q; a healthy gate must not block ingestion", mock.writtenDigest, digest)
		}
	})

	t.Run("nil gate", func(t *testing.T) {
		mock := &mockContainerdClient{}
		client, cleanup := startContainerServer(t, mock) // no WithContainerStorageGate option -> nil storageGate
		defer cleanup()

		stream, err := client.WriteLayer(context.Background())
		if err != nil {
			t.Fatalf("WriteLayer: %v", err)
		}
		digest := "sha256:nilgate"
		if err := stream.Send(&agentpb.WriteLayerRequest{Digest: digest, Data: []byte("data")}); err != nil {
			t.Fatalf("send: %v", err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		if _, err = stream.Recv(); err != nil {
			t.Fatalf("recv response: %v", err)
		}
		if mock.writtenDigest != digest {
			t.Errorf("writtenDigest = %q, want %q; a nil gate must not block ingestion", mock.writtenDigest, digest)
		}
	})
}
