package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/agent/services"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

// ---------- mocks for integration test ----------

type integrationNetworkManager struct{}

func (m *integrationNetworkManager) ListWiFiNetworks(_ context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	return []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "IntegrationNet"},
	}, nil
}
func (m *integrationNetworkManager) ConnectToWiFi(_ context.Context, _ *agentpb.ConnectToWiFiRequest) error {
	return nil
}
func (m *integrationNetworkManager) GetWiFiStatus(_ context.Context) (bool, string, error) {
	return true, "IntegrationNet", nil
}
func (m *integrationNetworkManager) DisconnectWiFi(_ context.Context) error {
	return nil
}
func (m *integrationNetworkManager) ListKnownWiFiNetworks(_ context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error) {
	return nil, nil
}
func (m *integrationNetworkManager) SetWiFiNetworkPriority(_ context.Context, _ string, _ int32) error {
	return nil
}
func (m *integrationNetworkManager) ReorderKnownWiFiNetworks(_ context.Context, _ []string) error {
	return nil
}
func (m *integrationNetworkManager) ForgetWiFiNetwork(_ context.Context, _ string) error {
	return nil
}

type integrationHardwareDiscoverer struct{}

func (m *integrationHardwareDiscoverer) Discover(_ context.Context, _ string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error) {
	return []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
		{Category: "gpu", DevicePath: "/dev/nvidia0", Description: "Test GPU"},
	}, nil
}

type integrationBluetoothManager struct{}

func (m *integrationBluetoothManager) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral)
	close(ch)
	return ch, nil
}
func (m *integrationBluetoothManager) Connect(_ context.Context, _ string, _, _ bool) error {
	return nil
}
func (m *integrationBluetoothManager) Disconnect(_ context.Context, _ string) error { return nil }
func (m *integrationBluetoothManager) Forget(_ context.Context, _ string) error     { return nil }

// statefulContainerdClient is a realistic mock that tracks layers, containers, and their state.
type statefulContainerdClient struct {
	mu         sync.Mutex
	layers     map[string][]byte // digest -> accumulated data
	containers map[string]bool   // appName -> running
	images     map[string]string // appName -> imageName
}

func newStatefulContainerdClient() *statefulContainerdClient {
	return &statefulContainerdClient{
		layers:     make(map[string][]byte),
		containers: make(map[string]bool),
		images:     make(map[string]string),
	}
}

func (m *statefulContainerdClient) ListContainers(_ context.Context) ([]*agentpb.AppContainer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*agentpb.AppContainer
	for name, running := range m.containers {
		state := agentpb.AppRunningState_STOPPED
		if running {
			state = agentpb.AppRunningState_RUNNING
		}
		result = append(result, &agentpb.AppContainer{
			AppName:      name,
			RunningState: state,
		})
	}
	return result, nil
}

func (m *statefulContainerdClient) StopContainer(_ context.Context, appName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.containers[appName]; !ok {
		return fmt.Errorf("container %q not found", appName)
	}
	m.containers[appName] = false
	return nil
}

func (m *statefulContainerdClient) DeleteContainer(_ context.Context, appName string, deleteImage bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.containers[appName]; !ok {
		return fmt.Errorf("container %q not found", appName)
	}
	delete(m.containers, appName)
	if deleteImage {
		delete(m.images, appName)
	}
	return nil
}

func (m *statefulContainerdClient) ListLayers(_ context.Context) ([]*agentpb.LayerHeader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*agentpb.LayerHeader
	for digest, data := range m.layers {
		result = append(result, &agentpb.LayerHeader{
			Digest: digest,
			Size:   int64(len(data)),
		})
	}
	return result, nil
}

func (m *statefulContainerdClient) WriteLayer(_ context.Context, digest string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.layers[digest] = data
	return nil
}

func (m *statefulContainerdClient) AssembleImage(_ context.Context, _ string, _ []*agentpb.RunContainerLayerHeader) error {
	return nil
}

func (m *statefulContainerdClient) CreateContainer(_ context.Context, req *agentpb.CreateContainerRequest, _ *appconfig.AppConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers[req.GetAppName()] = false
	m.images[req.GetAppName()] = req.GetImageName()
	return nil
}

func (m *statefulContainerdClient) CreateContainerWithProgress(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig, _ services.ProgressFunc) error {
	return m.CreateContainer(ctx, req, appCfg)
}

func (m *statefulContainerdClient) StartContainer(_ context.Context, appName, _ string) (<-chan services.ContainerOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.containers[appName]; !ok {
		return nil, fmt.Errorf("container %q not found", appName)
	}
	m.containers[appName] = true

	ch := make(chan services.ContainerOutput, 4)
	go func() {
		ch <- services.ContainerOutput{Stdout: []byte("hello from stdout\n")}
		ch <- services.ContainerOutput{Stderr: []byte("warning from stderr\n")}
		ch <- services.ContainerOutput{Stdout: []byte("more output\n")}
		ch <- services.ContainerOutput{Done: true}
		close(ch)
	}()
	return ch, nil
}

func (m *statefulContainerdClient) StartContainerWithStdin(_ context.Context, appName string, _ io.Reader, postStartAgentCommand string) (<-chan services.ContainerOutput, error) {
	return m.StartContainer(context.Background(), appName, postStartAgentCommand)
}

func (m *statefulContainerdClient) GetContainerStats(_ context.Context) ([]*agentpb.ContainerStats, error) {
	return nil, nil
}

func (m *statefulContainerdClient) GetContainerMetrics(_ context.Context, _ string) (services.ContainerMetrics, error) {
	return services.ContainerMetrics{}, nil
}

func (s *statefulContainerdClient) GetContainerMCPPort(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}

// getLayerData returns the data stored for a given digest, for test assertions.
func (m *statefulContainerdClient) getLayerData(digest string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.layers[digest]
	return data, ok
}

// isRunning returns whether a container is running, for test assertions.
func (m *statefulContainerdClient) isRunning(appName string) (running bool, exists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.containers[appName]
	return r, ok
}

// containerCount returns the number of tracked containers.
func (m *statefulContainerdClient) containerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.containers)
}

// ---------- fake cloud certificate service ----------

type integrationFakeCertService struct {
	cloudpb.UnimplementedCertificateServiceServer
	certPEM  string
	chainPEM string
}

func (f *integrationFakeCertService) IssueCertificate(_ context.Context, _ *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
	return &cloudpb.IssueCertificateResponse{
		Certificate: &cloudpb.Certificate{
			PemCertificate:      f.certPEM,
			PemCertificateChain: f.chainPEM,
		},
	}, nil
}

// ---------- integration test ----------

const integrationBufSize = 1024 * 1024

func TestFullAgentLifecycle(t *testing.T) {
	logger := zap.NewNop()
	lis := bufconn.Listen(integrationBufSize)

	// Create all services.
	nm := &integrationNetworkManager{}
	hd := &integrationHardwareDiscoverer{}
	bm := &integrationBluetoothManager{}
	cc := newStatefulContainerdClient()

	agentSvc := services.NewAgentService(logger, nm, hd, bm)
	containerSvc := services.NewContainerService(logger, cc)
	broadcaster := services.NewTelemetryBroadcaster()
	telemetrySvc := services.NewTelemetryService(logger, broadcaster)
	otelLogs := services.NewOTELLogsReceiver(broadcaster)

	// Register all services on a single gRPC server.
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, agentSvc)
	agentpb.RegisterWendyContainerServiceServer(srv, containerSvc)
	agentpb.RegisterWendyTelemetryServiceServer(srv, telemetrySvc)
	otelpb.RegisterLogsServiceServer(srv, otelLogs)

	go func() { _ = srv.Serve(lis) }()
	defer func() {
		srv.Stop()
		lis.Close()
	}()

	// Connect client.
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
	defer conn.Close()

	agentClient := agentpb.NewWendyAgentServiceClient(conn)
	containerClient := agentpb.NewWendyContainerServiceClient(conn)
	telemetryClient := agentpb.NewWendyTelemetryServiceClient(conn)
	otelLogsClient := otelpb.NewLogsServiceClient(conn)

	ctx := context.Background()

	// Step 1: GetAgentVersion
	t.Run("GetAgentVersion", func(t *testing.T) {
		resp, err := agentClient.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			t.Fatalf("GetAgentVersion: %v", err)
		}
		if resp.Version != version.Version {
			t.Errorf("version = %q; want %q", resp.Version, version.Version)
		}
		if resp.Os != runtime.GOOS {
			t.Errorf("os = %q; want %q", resp.Os, runtime.GOOS)
		}
		if resp.CpuArchitecture != runtime.GOARCH {
			t.Errorf("arch = %q; want %q", resp.CpuArchitecture, runtime.GOARCH)
		}
		t.Logf("Agent version: %s (%s/%s)", resp.Version, resp.Os, resp.CpuArchitecture)
	})

	// Step 2: ListContainers (empty)
	t.Run("ListContainers_Empty", func(t *testing.T) {
		stream, err := containerClient.ListContainers(ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			t.Fatalf("ListContainers: %v", err)
		}

		var containers []*agentpb.AppContainer
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("recv: %v", err)
			}
			containers = append(containers, resp.Container)
		}

		if len(containers) != 0 {
			t.Errorf("expected 0 containers, got %d", len(containers))
		}
	})

	// Step 3: ListHardwareCapabilities
	t.Run("ListHardwareCapabilities", func(t *testing.T) {
		resp, err := agentClient.ListHardwareCapabilities(ctx, &agentpb.ListHardwareCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("ListHardwareCapabilities: %v", err)
		}
		if len(resp.Capabilities) != 1 {
			t.Fatalf("expected 1 capability, got %d", len(resp.Capabilities))
		}
		if resp.Capabilities[0].Category != "gpu" {
			t.Errorf("category = %q; want gpu", resp.Capabilities[0].Category)
		}
	})

	// Step 4: StreamLogs - subscribe and receive
	t.Run("StreamLogs", func(t *testing.T) {
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		stream, err := telemetryClient.StreamLogs(streamCtx, &agentpb.StreamLogsRequest{})
		if err != nil {
			t.Fatalf("StreamLogs: %v", err)
		}

		// Give server time to register subscriber.
		time.Sleep(50 * time.Millisecond)

		// Publish a log via OTEL receiver.
		_, err = otelLogsClient.Export(ctx, &otelpb.ExportLogsServiceRequest{})
		if err != nil {
			t.Fatalf("OTEL Export: %v", err)
		}

		// Receive the log on the telemetry stream.
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv log: %v", err)
		}
		if resp.Logs == nil {
			t.Error("expected non-nil logs")
		}

		// Cancel and confirm stream ends.
		cancel()
	})

	// Step 5: WiFi operations
	t.Run("WiFiOperations", func(t *testing.T) {
		nets, err := agentClient.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
		if err != nil {
			t.Fatalf("ListWiFiNetworks: %v", err)
		}
		if len(nets.Networks) != 1 {
			t.Errorf("expected 1 network, got %d", len(nets.Networks))
		}

		status, err := agentClient.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
		if err != nil {
			t.Fatalf("GetWiFiStatus: %v", err)
		}
		if !status.Connected {
			t.Error("expected connected")
		}

		connectResp, err := agentClient.ConnectToWiFi(ctx, &agentpb.ConnectToWiFiRequest{
			Ssid:     "IntegrationNet",
			Password: "pass",
		})
		if err != nil {
			t.Fatalf("ConnectToWiFi: %v", err)
		}
		if !connectResp.Success {
			t.Error("expected success")
		}

		disconnResp, err := agentClient.DisconnectWiFi(ctx, &agentpb.DisconnectWiFiRequest{})
		if err != nil {
			t.Fatalf("DisconnectWiFi: %v", err)
		}
		if !disconnResp.Success {
			t.Error("expected disconnect success")
		}
	})

	fmt.Println("Full agent lifecycle integration test passed")
}

// TestContainerDeployStartStopDelete tests the full container lifecycle via gRPC:
// WriteLayer -> CreateContainer -> StartContainer -> ListContainers -> StopContainer -> DeleteContainer
func TestContainerDeployStartStopDelete(t *testing.T) {
	logger := zap.NewNop()
	lis := bufconn.Listen(integrationBufSize)
	cc := newStatefulContainerdClient()

	containerSvc := services.NewContainerService(logger, cc)

	srv := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(srv, containerSvc)

	go func() { _ = srv.Serve(lis) }()
	defer func() {
		srv.Stop()
		lis.Close()
	}()

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
	defer conn.Close()

	client := agentpb.NewWendyContainerServiceClient(conn)
	ctx := context.Background()

	const testDigest = "sha256:abc123"
	const testAppName = "my-test-app"
	const testImageName = "test-image:latest"

	// Step 1: WriteLayer - stream layer data in multiple chunks
	t.Run("WriteLayer", func(t *testing.T) {
		stream, err := client.WriteLayer(ctx)
		if err != nil {
			t.Fatalf("WriteLayer: %v", err)
		}

		// Send first chunk with digest.
		chunk1 := []byte("first chunk of layer data")
		if err := stream.Send(&agentpb.WriteLayerRequest{
			Digest: testDigest,
			Data:   chunk1,
		}); err != nil {
			t.Fatalf("send chunk 1: %v", err)
		}

		// Send additional chunks without digest (digest is only on first message).
		chunk2 := []byte(" second chunk")
		if err := stream.Send(&agentpb.WriteLayerRequest{
			Data: chunk2,
		}); err != nil {
			t.Fatalf("send chunk 2: %v", err)
		}

		chunk3 := []byte(" third chunk")
		if err := stream.Send(&agentpb.WriteLayerRequest{
			Data: chunk3,
		}); err != nil {
			t.Fatalf("send chunk 3: %v", err)
		}

		// Close the send side and receive the confirmation response.
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv after CloseSend: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil WriteLayerResponse")
		}

		// Verify the mock received all data.
		data, ok := cc.getLayerData(testDigest)
		if !ok {
			t.Fatal("layer not found in mock containerd")
		}
		expected := append(append(chunk1, chunk2...), chunk3...)
		if !bytes.Equal(data, expected) {
			t.Errorf("layer data mismatch: got %d bytes, want %d bytes", len(data), len(expected))
		}
		t.Logf("WriteLayer stored %d bytes for digest %s", len(data), testDigest)
	})

	// Step 2: CreateContainer
	t.Run("CreateContainer", func(t *testing.T) {
		_, err := client.CreateContainer(ctx, &agentpb.CreateContainerRequest{
			ImageName: testImageName,
			AppName:   testAppName,
			Cmd:       "python main.py",
			AppConfig: []byte(`{"appId":"test","entitlements":[{"type":"gpu"},{"type":"network"}]}`),
			RestartPolicy: &agentpb.RestartPolicy{
				Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED,
			},
		})
		if err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}

		// Verify the container exists in the mock (not yet running).
		running, exists := cc.isRunning(testAppName)
		if !exists {
			t.Fatal("container not found in mock after CreateContainer")
		}
		if running {
			t.Error("container should not be running after CreateContainer (before StartContainer)")
		}
	})

	// Step 3: StartContainer - start and receive output
	t.Run("StartContainer", func(t *testing.T) {
		stream, err := client.StartContainer(ctx, &agentpb.StartContainerRequest{
			AppName: testAppName,
		})
		if err != nil {
			t.Fatalf("StartContainer: %v", err)
		}

		var gotStarted bool
		var stdoutData []byte
		var stderrData []byte

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("recv: %v", err)
			}

			switch rt := resp.GetResponseType().(type) {
			case *agentpb.RunContainerLayersResponse_Started_:
				gotStarted = true
			case *agentpb.RunContainerLayersResponse_StdoutOutput:
				stdoutData = append(stdoutData, rt.StdoutOutput.Data...)
			case *agentpb.RunContainerLayersResponse_StderrOutput:
				stderrData = append(stderrData, rt.StderrOutput.Data...)
			}
		}

		if !gotStarted {
			t.Error("expected to receive Started response")
		}
		if len(stdoutData) == 0 {
			t.Error("expected stdout output")
		}
		if len(stderrData) == 0 {
			t.Error("expected stderr output")
		}
		if !bytes.Contains(stdoutData, []byte("hello from stdout")) {
			t.Errorf("stdout = %q; want to contain 'hello from stdout'", stdoutData)
		}
		if !bytes.Contains(stderrData, []byte("warning from stderr")) {
			t.Errorf("stderr = %q; want to contain 'warning from stderr'", stderrData)
		}
		t.Logf("StartContainer stdout: %s", stdoutData)
		t.Logf("StartContainer stderr: %s", stderrData)
	})

	// Step 4: ListContainers - verify the container appears with correct state
	t.Run("ListContainers_AfterStart", func(t *testing.T) {
		stream, err := client.ListContainers(ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			t.Fatalf("ListContainers: %v", err)
		}

		var containers []*agentpb.AppContainer
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("recv: %v", err)
			}
			containers = append(containers, resp.Container)
		}

		if len(containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(containers))
		}
		if containers[0].AppName != testAppName {
			t.Errorf("app_name = %q; want %q", containers[0].AppName, testAppName)
		}
		// The container was started (and the mock marks it as running).
		if containers[0].RunningState != agentpb.AppRunningState_RUNNING {
			t.Errorf("running_state = %v; want RUNNING", containers[0].RunningState)
		}
	})

	// Step 5: StopContainer
	t.Run("StopContainer", func(t *testing.T) {
		_, err := client.StopContainer(ctx, &agentpb.StopContainerRequest{
			AppName: testAppName,
		})
		if err != nil {
			t.Fatalf("StopContainer: %v", err)
		}

		// Verify container is stopped in the mock.
		running, exists := cc.isRunning(testAppName)
		if !exists {
			t.Fatal("container should still exist after stop")
		}
		if running {
			t.Error("container should be stopped after StopContainer")
		}
	})

	// Step 6: ListContainers - verify the container is now stopped
	t.Run("ListContainers_AfterStop", func(t *testing.T) {
		stream, err := client.ListContainers(ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			t.Fatalf("ListContainers: %v", err)
		}

		var containers []*agentpb.AppContainer
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("recv: %v", err)
			}
			containers = append(containers, resp.Container)
		}

		if len(containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(containers))
		}
		if containers[0].RunningState != agentpb.AppRunningState_STOPPED {
			t.Errorf("running_state = %v; want STOPPED", containers[0].RunningState)
		}
	})

	// Step 7: DeleteContainer
	t.Run("DeleteContainer", func(t *testing.T) {
		_, err := client.DeleteContainer(ctx, &agentpb.DeleteContainerRequest{
			AppName:     testAppName,
			DeleteImage: true,
		})
		if err != nil {
			t.Fatalf("DeleteContainer: %v", err)
		}

		// Verify container is removed.
		_, exists := cc.isRunning(testAppName)
		if exists {
			t.Error("container should not exist after DeleteContainer")
		}
		if cc.containerCount() != 0 {
			t.Errorf("expected 0 containers, got %d", cc.containerCount())
		}
	})

	// Step 8: ListContainers after delete should be empty
	t.Run("ListContainers_AfterDelete", func(t *testing.T) {
		stream, err := client.ListContainers(ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			t.Fatalf("ListContainers: %v", err)
		}

		var containers []*agentpb.AppContainer
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("recv: %v", err)
			}
			containers = append(containers, resp.Container)
		}

		if len(containers) != 0 {
			t.Errorf("expected 0 containers after delete, got %d", len(containers))
		}
	})
}

// TestStreamMetrics verifies that metrics published via the OTEL receiver
// are received by a StreamMetrics subscriber.
func TestStreamMetrics(t *testing.T) {
	logger := zap.NewNop()
	lis := bufconn.Listen(integrationBufSize)

	broadcaster := services.NewTelemetryBroadcaster()
	telemetrySvc := services.NewTelemetryService(logger, broadcaster)
	otelMetrics := services.NewOTELMetricsReceiver(broadcaster)

	srv := grpc.NewServer()
	agentpb.RegisterWendyTelemetryServiceServer(srv, telemetrySvc)
	otelpb.RegisterMetricsServiceServer(srv, otelMetrics)

	go func() { _ = srv.Serve(lis) }()
	defer func() {
		srv.Stop()
		lis.Close()
	}()

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
	defer conn.Close()

	telemetryClient := agentpb.NewWendyTelemetryServiceClient(conn)
	otelMetricsClient := otelpb.NewMetricsServiceClient(conn)

	ctx := context.Background()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := telemetryClient.StreamMetrics(streamCtx, &agentpb.StreamMetricsRequest{})
	if err != nil {
		t.Fatalf("StreamMetrics: %v", err)
	}

	// Give server time to register subscriber.
	time.Sleep(50 * time.Millisecond)

	// Publish metrics via OTEL receiver.
	_, err = otelMetricsClient.Export(ctx, &otelpb.ExportMetricsServiceRequest{})
	if err != nil {
		t.Fatalf("OTEL Metrics Export: %v", err)
	}

	// Receive the metrics on the telemetry stream.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv metrics: %v", err)
	}
	if resp.Metrics == nil {
		t.Error("expected non-nil metrics")
	}

	cancel()
	t.Log("StreamMetrics test passed")
}

// TestStreamTraces verifies that traces published via the OTEL receiver
// are received by a StreamTraces subscriber.
func TestStreamTraces(t *testing.T) {
	logger := zap.NewNop()
	lis := bufconn.Listen(integrationBufSize)

	broadcaster := services.NewTelemetryBroadcaster()
	telemetrySvc := services.NewTelemetryService(logger, broadcaster)
	otelTraces := services.NewOTELTraceReceiver(broadcaster)

	srv := grpc.NewServer()
	agentpb.RegisterWendyTelemetryServiceServer(srv, telemetrySvc)
	otelpb.RegisterTraceServiceServer(srv, otelTraces)

	go func() { _ = srv.Serve(lis) }()
	defer func() {
		srv.Stop()
		lis.Close()
	}()

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
	defer conn.Close()

	telemetryClient := agentpb.NewWendyTelemetryServiceClient(conn)
	otelTraceClient := otelpb.NewTraceServiceClient(conn)

	ctx := context.Background()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := telemetryClient.StreamTraces(streamCtx, &agentpb.StreamTracesRequest{})
	if err != nil {
		t.Fatalf("StreamTraces: %v", err)
	}

	// Give server time to register subscriber.
	time.Sleep(50 * time.Millisecond)

	// Publish traces via OTEL receiver.
	_, err = otelTraceClient.Export(ctx, &otelpb.ExportTraceServiceRequest{})
	if err != nil {
		t.Fatalf("OTEL Trace Export: %v", err)
	}

	// Receive the traces on the telemetry stream.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv traces: %v", err)
	}
	if resp.Traces == nil {
		t.Error("expected non-nil traces")
	}

	cancel()
	t.Log("StreamTraces test passed")
}

// TestProvisioningFlow tests the full provisioning lifecycle via gRPC:
// IsProvisioned (not provisioned) -> StartProvisioning (with fake cloud) -> IsProvisioned (provisioned).
func TestProvisioningFlow(t *testing.T) {
	logger := zap.NewNop()
	lis := bufconn.Listen(integrationBufSize)

	// Create provisioning service with temp config dir.
	tmpDir, err := os.MkdirTemp("", "wendy-integ-prov-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	provSvc := services.NewProvisioningService(logger, tmpDir)

	// Start a fake cloud certificate server using a separate bufconn listener.
	cloudLis := bufconn.Listen(integrationBufSize)
	cloudSrv := grpc.NewServer()
	cloudpb.RegisterCertificateServiceServer(cloudSrv, &integrationFakeCertService{
		certPEM:  "fake-cert-pem-data",
		chainPEM: "fake-chain-pem-data",
	})
	go func() { _ = cloudSrv.Serve(cloudLis) }()
	defer func() {
		cloudSrv.Stop()
		cloudLis.Close()
	}()

	// Override the cloud dialer to connect to our fake cloud via bufconn.
	provSvc.CloudDialer = func(_ context.Context, _ string) (*grpc.ClientConn, error) {
		cloudDialer := func(context.Context, string) (net.Conn, error) {
			return cloudLis.Dial()
		}
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(cloudDialer),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}

	// Register the provisioning service on the agent gRPC server.
	srv := grpc.NewServer()
	agentpb.RegisterWendyProvisioningServiceServer(srv, provSvc)

	go func() { _ = srv.Serve(lis) }()
	defer func() {
		srv.Stop()
		lis.Close()
	}()

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
	defer conn.Close()

	provClient := agentpb.NewWendyProvisioningServiceClient(conn)
	ctx := context.Background()

	// Step 1: IsProvisioned should return not provisioned.
	t.Run("IsProvisioned_NotProvisioned", func(t *testing.T) {
		resp, err := provClient.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
		if err != nil {
			t.Fatalf("IsProvisioned: %v", err)
		}
		np := resp.GetNotProvisioned()
		if np == nil {
			t.Fatal("expected NotProvisioned response, got Provisioned")
		}
	})

	// Step 2: StartProvisioning.
	t.Run("StartProvisioning", func(t *testing.T) {
		_, err := provClient.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
			OrganizationId:  42,
			CloudHost:       "cloud.wendy.test",
			AssetId:         100,
			EnrollmentToken: "test-enrollment-token",
		})
		if err != nil {
			t.Fatalf("StartProvisioning: %v", err)
		}
	})

	// Step 3: IsProvisioned should now return provisioned with correct data.
	t.Run("IsProvisioned_Provisioned", func(t *testing.T) {
		resp, err := provClient.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
		if err != nil {
			t.Fatalf("IsProvisioned: %v", err)
		}
		prov := resp.GetProvisioned()
		if prov == nil {
			t.Fatal("expected Provisioned response, got NotProvisioned")
		}
		if prov.CloudHost != "cloud.wendy.test" {
			t.Errorf("CloudHost = %q; want cloud.wendy.test", prov.CloudHost)
		}
		if prov.OrganizationId != 42 {
			t.Errorf("OrganizationId = %d; want 42", prov.OrganizationId)
		}
		if prov.AssetId != 100 {
			t.Errorf("AssetId = %d; want 100", prov.AssetId)
		}
	})

	// Step 4: StartProvisioning again should fail (already provisioned).
	t.Run("StartProvisioning_AlreadyProvisioned", func(t *testing.T) {
		_, err := provClient.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
			OrganizationId:  99,
			CloudHost:       "other.wendy.test",
			AssetId:         200,
			EnrollmentToken: "another-token",
		})
		if err == nil {
			t.Fatal("expected error when provisioning an already-provisioned agent")
		}
	})
}

// TestRunContainer tests the RunContainer RPC which combines container creation + starting
// in a single call, and streams output back.
func TestRunContainer(t *testing.T) {
	logger := zap.NewNop()
	lis := bufconn.Listen(integrationBufSize)
	cc := newStatefulContainerdClient()

	containerSvc := services.NewContainerService(logger, cc)

	srv := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(srv, containerSvc)

	go func() { _ = srv.Serve(lis) }()
	defer func() {
		srv.Stop()
		lis.Close()
	}()

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
	defer conn.Close()

	client := agentpb.NewWendyContainerServiceClient(conn)
	ctx := context.Background()

	const appName = "run-test-app"
	const imageName = "run-test-image:v1"

	// RunContainer creates and starts in one shot.
	t.Run("RunContainer", func(t *testing.T) {
		stream, err := client.RunContainer(ctx, &agentpb.RunContainerLayersRequest{
			ImageName: imageName,
			AppName:   appName,
			Cmd:       "python app.py",
			AppConfig: []byte(`{}`),
			Layers: []*agentpb.RunContainerLayerHeader{
				{Digest: "sha256:layer1", Size: 1024},
			},
		})
		if err != nil {
			t.Fatalf("RunContainer: %v", err)
		}

		var gotStarted bool
		var stdoutData []byte
		var stderrData []byte

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("recv: %v", err)
			}

			switch rt := resp.GetResponseType().(type) {
			case *agentpb.RunContainerLayersResponse_Started_:
				gotStarted = true
			case *agentpb.RunContainerLayersResponse_StdoutOutput:
				stdoutData = append(stdoutData, rt.StdoutOutput.Data...)
			case *agentpb.RunContainerLayersResponse_StderrOutput:
				stderrData = append(stderrData, rt.StderrOutput.Data...)
			}
		}

		if !gotStarted {
			t.Error("expected to receive Started response from RunContainer")
		}
		if len(stdoutData) == 0 {
			t.Error("expected stdout output from RunContainer")
		}
		if len(stderrData) == 0 {
			t.Error("expected stderr output from RunContainer")
		}
		t.Logf("RunContainer stdout: %s", stdoutData)
		t.Logf("RunContainer stderr: %s", stderrData)
	})

	// Verify the container was created and is now running (started by RunContainer).
	t.Run("VerifyContainerRunning", func(t *testing.T) {
		stream, err := client.ListContainers(ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			t.Fatalf("ListContainers: %v", err)
		}

		var containers []*agentpb.AppContainer
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("recv: %v", err)
			}
			containers = append(containers, resp.Container)
		}

		if len(containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(containers))
		}
		if containers[0].AppName != appName {
			t.Errorf("app_name = %q; want %q", containers[0].AppName, appName)
		}
		if containers[0].RunningState != agentpb.AppRunningState_RUNNING {
			t.Errorf("running_state = %v; want RUNNING", containers[0].RunningState)
		}
	})
}
