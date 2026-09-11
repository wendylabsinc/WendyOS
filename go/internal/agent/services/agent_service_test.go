package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/agent/oshealth"
	"github.com/wendylabsinc/wendy/go/internal/shared/sigverify"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// ---------- mock implementations ----------

type mockNetworkManager struct {
	networks   []*agentpb.ListWiFiNetworksResponse_WiFiNetwork
	listErr    error
	connectErr error
	connected  bool
	ssid       string
	statusErr  error
	disconnErr error
}

func (m *mockNetworkManager) ListWiFiNetworks(_ context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	return m.networks, m.listErr
}
func (m *mockNetworkManager) ConnectToWiFi(_ context.Context, _ *agentpb.ConnectToWiFiRequest) error {
	return m.connectErr
}
func (m *mockNetworkManager) GetWiFiStatus(_ context.Context) (bool, string, error) {
	return m.connected, m.ssid, m.statusErr
}
func (m *mockNetworkManager) DisconnectWiFi(_ context.Context) error {
	return m.disconnErr
}
func (m *mockNetworkManager) ListKnownWiFiNetworks(_ context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error) {
	return nil, nil
}
func (m *mockNetworkManager) SetWiFiNetworkPriority(_ context.Context, _ string, _ int32) error {
	return nil
}
func (m *mockNetworkManager) ReorderKnownWiFiNetworks(_ context.Context, _ []string) error {
	return nil
}
func (m *mockNetworkManager) ForgetWiFiNetwork(_ context.Context, _ string) error {
	return nil
}

type mockHardwareDiscoverer struct {
	caps []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability
	err  error
}

func (m *mockHardwareDiscoverer) Discover(_ context.Context, _ string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error) {
	return m.caps, m.err
}

type mockBluetoothManager struct{}

func (m *mockBluetoothManager) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral)
	close(ch)
	return ch, nil
}
func (m *mockBluetoothManager) Connect(_ context.Context, _ string, _, _ bool) (bool, error) {
	return true, nil
}
func (m *mockBluetoothManager) Disconnect(_ context.Context, _ string) error { return nil }
func (m *mockBluetoothManager) Forget(_ context.Context, _ string) error     { return nil }

// ---------- bufconn helper ----------

const bufSize = 1024 * 1024

func startAgentServer(t *testing.T, nm NetworkManager, hd HardwareDiscoverer, bm BluetoothManager, opts ...func(*AgentService)) (agentpb.WendyAgentServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	logger := zap.NewNop()
	svc := NewAgentService(logger, nm, hd, bm, &AgentInstaller{})
	for _, opt := range opts {
		opt(svc)
	}
	agentpb.RegisterWendyAgentServiceServer(srv, svc)

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

	client := agentpb.NewWendyAgentServiceClient(conn)
	cleanup := func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
	return client, cleanup
}

// ---------- tests ----------

func TestGetAgentVersion(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
	if resp.Version != version.Version {
		t.Errorf("version = %q; want %q", resp.Version, version.Version)
	}
	if resp.Os == "" {
		t.Errorf("os is empty")
	}
	if resp.CpuArchitecture != runtime.GOARCH {
		t.Errorf("arch = %q; want %q", resp.CpuArchitecture, runtime.GOARCH)
	}
	// RAM size and CPU core count come from /proc, so they are only
	// guaranteed on Linux hosts (WDY-1809). Zero means "unknown".
	if runtime.GOOS == "linux" {
		if resp.MemTotalBytes <= 0 {
			t.Errorf("memTotalBytes = %d, want > 0 on linux", resp.MemTotalBytes)
		}
		if resp.CpuCount == 0 {
			t.Errorf("cpuCount = 0, want > 0 on linux")
		}
	}
}

func TestGetAgentVersionReportsHostname(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}
	if resp.GetHostname() != host {
		t.Fatalf("Hostname = %q, want %q", resp.GetHostname(), host)
	}
}

func TestReadWendyOSVersionFromPrefersCurrentPath(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "etc", "wendyos", "version.txt")
	legacy := filepath.Join(dir, "etc", "wendy", "version.txt")

	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		t.Fatalf("mkdir current: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(current, []byte("WendyOS-0.13.2\n"), 0o644); err != nil {
		t.Fatalf("write current: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("WendyOS-0.10.4\n"), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	got, ok := readWendyOSVersionFrom(current, legacy)
	if !ok {
		t.Fatal("expected WendyOS version")
	}
	if got != "WendyOS-0.13.2" {
		t.Fatalf("version = %q, want current version", got)
	}
}

func TestReadWendyOSVersionFromFallsBackToLegacyPath(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "etc", "wendyos", "version.txt")
	legacy := filepath.Join(dir, "etc", "wendy", "version.txt")

	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("WendyOS-0.10.4\n"), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	got, ok := readWendyOSVersionFrom(current, legacy)
	if !ok {
		t.Fatal("expected WendyOS version")
	}
	if got != "WendyOS-0.10.4" {
		t.Fatalf("version = %q, want legacy version", got)
	}
}

func TestListWiFiNetworks(t *testing.T) {
	nets := []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "HomeWiFi"},
		{Ssid: "OfficeWiFi"},
	}
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{networks: nets},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ListWiFiNetworks(context.Background(), &agentpb.ListWiFiNetworksRequest{})
	if err != nil {
		t.Fatalf("ListWiFiNetworks: %v", err)
	}
	if len(resp.Networks) != 2 {
		t.Fatalf("len(networks) = %d; want 2", len(resp.Networks))
	}
	if resp.Networks[0].Ssid != "HomeWiFi" {
		t.Errorf("networks[0].ssid = %q; want HomeWiFi", resp.Networks[0].Ssid)
	}
}

func TestConnectToWiFi_Success(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{
		Ssid:     "TestNet",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("ConnectToWiFi: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
}

func TestConnectToWiFi_Failure(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{connectErr: fmt.Errorf("bad password")},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{
		Ssid: "TestNet",
	})
	if err != nil {
		t.Fatalf("ConnectToWiFi: %v", err)
	}
	if resp.Success {
		t.Error("expected failure")
	}
	if resp.GetErrorMessage() == "" {
		t.Error("expected error message")
	}
}

func TestGetWiFiStatus_Connected(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{connected: true, ssid: "MyNet"},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.GetWiFiStatus(context.Background(), &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		t.Fatalf("GetWiFiStatus: %v", err)
	}
	if !resp.Connected {
		t.Error("expected connected = true")
	}
	if resp.GetSsid() != "MyNet" {
		t.Errorf("ssid = %q; want MyNet", resp.GetSsid())
	}
}

func TestGetWiFiStatus_Disconnected(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{connected: false, ssid: ""},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.GetWiFiStatus(context.Background(), &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		t.Fatalf("GetWiFiStatus: %v", err)
	}
	if resp.Connected {
		t.Error("expected connected = false")
	}
}

func TestDisconnectWiFi(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.DisconnectWiFi(context.Background(), &agentpb.DisconnectWiFiRequest{})
	if err != nil {
		t.Fatalf("DisconnectWiFi: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
}

func TestListHardwareCapabilities(t *testing.T) {
	caps := []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability{
		{Category: "gpu", DevicePath: "/dev/nvidia0", Description: "NVIDIA GPU"},
		{Category: "audio", DevicePath: "/dev/snd/controlC0", Description: "HDA Audio"},
	}
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{caps: caps},
		&mockBluetoothManager{},
	)
	defer cleanup()

	resp, err := client.ListHardwareCapabilities(context.Background(), &agentpb.ListHardwareCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ListHardwareCapabilities: %v", err)
	}
	if len(resp.Capabilities) != 2 {
		t.Fatalf("len(caps) = %d; want 2", len(resp.Capabilities))
	}
	if resp.Capabilities[0].Category != "gpu" {
		t.Errorf("cap[0].Category = %q; want gpu", resp.Capabilities[0].Category)
	}
}

func TestUpdateAgent_LockExclusion(t *testing.T) {
	installer := &AgentInstaller{}

	// TryLock should succeed the first time.
	if !installer.TryLock() {
		t.Fatal("expected TryLock to succeed when not updating")
	}

	// TryLock should fail while the lock is held.
	if installer.TryLock() {
		t.Error("expected TryLock to fail while update is in progress")
		installer.Unlock()
	}

	// After Unlock, TryLock should succeed again.
	installer.Unlock()
	if !installer.TryLock() {
		t.Error("expected TryLock to succeed after unlock")
	}
	installer.Unlock()
}

func TestUpdateOS_NonWendyOSFailsBeforeUpdate(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) { svc.isWendyOSHost = func() bool { return false } },
	)
	defer cleanup()

	stream, err := client.UpdateOS(context.Background(), &agentpb.UpdateOSRequest{
		ArtifactUrl: "http://example.invalid/update.wendy",
	})
	if err != nil {
		t.Fatalf("UpdateOS: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("UpdateOS Recv: %v", err)
	}
	failed := resp.GetFailed()
	if failed == nil {
		t.Fatalf("UpdateOS response = %T, want failed", resp.GetResponseType())
	}
	if failed.GetErrorMessage() != osUpdateUnsupportedForHostMessage {
		t.Fatalf("error message = %q, want %q", failed.GetErrorMessage(), osUpdateUnsupportedForHostMessage)
	}
}

func TestUpdateAgent_ConcurrentLock(t *testing.T) {
	installer := &AgentInstaller{}

	// First caller acquires the lock.
	if !installer.TryLock() {
		t.Fatal("first TryLock should succeed")
	}

	// Concurrent attempt must be rejected.
	var wg sync.WaitGroup
	wg.Add(1)
	var blocked bool
	go func() {
		defer wg.Done()
		blocked = !installer.TryLock()
	}()
	wg.Wait()

	if !blocked {
		t.Error("expected concurrent TryLock to be rejected while update is in progress")
		installer.Unlock() // clean up if somehow succeeded
	}

	installer.Unlock()
}

// startAgentServerForUpdate starts an AgentService (the v1 WendyAgentService
// handler) over bufconn, targeting a temp file as the "executable" so
// commitBinaryUpdate's rename lands somewhere inspectable, and with the
// restart exit stubbed out so a successful install doesn't kill the test
// process. Mirrors startAgentUpdateServer in agent_update_service_test.go for
// the v2 sibling handler.
func startAgentServerForUpdate(t *testing.T, verifier *sigverify.Verifier) (agentpb.WendyAgentServiceClient, string, func()) {
	t.Helper()

	dir := t.TempDir()
	execPath := filepath.Join(dir, "wendy-agent")
	if err := os.WriteFile(execPath, []byte("original-binary-contents"), 0o755); err != nil {
		t.Fatal(err)
	}

	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) {
			// The real resolveExecPath() resolves os.Executable() (the running
			// test binary itself); overriding it here points
			// commitBinaryUpdate's rename at a disposable temp file instead so
			// a "valid signature" test case doesn't clobber the test binary
			// mid-run.
			svc.execPathResolver = func() (string, os.FileMode, error) {
				return execPath, 0o755, nil
			}
			// The real scheduleAgentRestartExit calls os.Exit(0) shortly after
			// a successful commit; a no-op keeps the "valid signature
			// installs" case from killing the test process.
			svc.restartFn = func() {}
			if verifier != nil {
				svc.verifier = verifier
			}
		},
	)
	return client, execPath, cleanup
}

// sendV1UpdateStream drives an AgentService.UpdateAgent (v1) bidi stream: a
// single chunk carrying binaryData, followed by an Update control command
// carrying sha256Hash and signature. It returns the terminal error from
// Recv (nil on success). Mirrors sendUpdateStream in
// agent_update_service_test.go for the v2 sibling handler.
func sendV1UpdateStream(t *testing.T, stream grpc.BidiStreamingClient[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse], binaryData []byte, sha256Hash string, signature []byte) error {
	t.Helper()

	if err := stream.Send(&agentpb.UpdateAgentRequest{
		RequestType: &agentpb.UpdateAgentRequest_Chunk_{
			Chunk: &agentpb.UpdateAgentRequest_Chunk{Data: binaryData},
		},
	}); err != nil {
		t.Fatalf("send chunk: %v", err)
	}

	if err := stream.Send(&agentpb.UpdateAgentRequest{
		RequestType: &agentpb.UpdateAgentRequest_Control{
			Control: &agentpb.UpdateAgentRequest_ControlCommand{
				Command: &agentpb.UpdateAgentRequest_ControlCommand_Update_{
					Update: &agentpb.UpdateAgentRequest_ControlCommand_Update{
						Sha256:    sha256Hash,
						Signature: signature,
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send control: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	_, err := stream.Recv()
	return err
}

func TestUpdateAgentV1_SignatureVerification(t *testing.T) {
	verifier, sign := mldsaKeypairForTest(t)

	binaryData := []byte("new-agent-binary-contents-for-v1-signature-test")
	sum := sha256.Sum256(binaryData)
	digest := sum[:]
	sha256Hash := hex.EncodeToString(digest)

	t.Run("valid signature installs", func(t *testing.T) {
		client, execPath, cleanup := startAgentServerForUpdate(t, verifier)
		defer cleanup()

		stream, err := client.UpdateAgent(context.Background())
		if err != nil {
			t.Fatalf("UpdateAgent: %v", err)
		}

		sig := sign(digest)
		if err := sendV1UpdateStream(t, stream, binaryData, sha256Hash, sig); err != nil {
			t.Fatalf("expected success, got: %v", err)
		}

		installed, err := os.ReadFile(execPath)
		if err != nil {
			t.Fatalf("reading installed binary: %v", err)
		}
		if string(installed) != string(binaryData) {
			t.Fatalf("installed binary = %q, want %q", installed, binaryData)
		}
	})

	t.Run("empty signature is rejected and not installed", func(t *testing.T) {
		client, execPath, cleanup := startAgentServerForUpdate(t, verifier)
		defer cleanup()

		stream, err := client.UpdateAgent(context.Background())
		if err != nil {
			t.Fatalf("UpdateAgent: %v", err)
		}

		err = sendV1UpdateStream(t, stream, binaryData, sha256Hash, nil)
		if err == nil {
			t.Fatal("expected error for empty signature while verifier enabled")
		}
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %v, want FailedPrecondition; err = %v", status.Code(err), err)
		}

		installed, readErr := os.ReadFile(execPath)
		if readErr != nil {
			t.Fatalf("reading exec path: %v", readErr)
		}
		if string(installed) == string(binaryData) {
			t.Fatal("binary was installed despite missing signature")
		}
	})

	t.Run("tampered signature is rejected and not installed", func(t *testing.T) {
		client, execPath, cleanup := startAgentServerForUpdate(t, verifier)
		defer cleanup()

		stream, err := client.UpdateAgent(context.Background())
		if err != nil {
			t.Fatalf("UpdateAgent: %v", err)
		}

		sig := sign(digest)
		sig[0] ^= 0xFF // tamper

		err = sendV1UpdateStream(t, stream, binaryData, sha256Hash, sig)
		if err == nil {
			t.Fatal("expected error for tampered signature")
		}
		if status.Code(err) != codes.DataLoss {
			t.Fatalf("code = %v, want DataLoss; err = %v", status.Code(err), err)
		}

		installed, readErr := os.ReadFile(execPath)
		if readErr != nil {
			t.Fatalf("reading exec path: %v", readErr)
		}
		if string(installed) == string(binaryData) {
			t.Fatal("binary was installed despite tampered signature")
		}
	})
}

func TestRunContainer_Deprecated(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	ctx := context.Background()
	stream, err := client.RunContainer(ctx)
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	// The deprecated RunContainer should return Unimplemented on Recv.
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error from deprecated RunContainer")
	}

	// Verify it is an Unimplemented status error.
	if !strings.Contains(err.Error(), "deprecated") && !strings.Contains(err.Error(), "Unimplemented") {
		t.Fatalf("expected Unimplemented/deprecated error, got: %v", err)
	}
}

func TestParseDeviceTypePrefersBoard(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantType    string
		wantStorage string
	}{
		{
			name:        "board wins over machine and storage inferred from machine",
			content:     "BOARD=jetson-orin-nano\nMACHINE=jetson-orin-nano-devkit-nvme-wendyos\n",
			wantType:    "jetson-orin-nano",
			wantStorage: "nvme",
		},
		{
			name:        "disk storage",
			content:     "BOARD=vm-arm64\nMACHINE=vm-arm64-wendyos\nSTORAGE=disk\n",
			wantType:    "vm-arm64",
			wantStorage: "disk",
		},
		{
			name:        "board wins when listed after machine",
			content:     "MACHINE=jetson-orin-nano-devkit-nvme-wendyos\nBOARD=jetson-orin-nano\n",
			wantType:    "jetson-orin-nano",
			wantStorage: "nvme",
		},
		{
			name:        "emmc inferred from machine",
			content:     "BOARD=jetson-agx-orin-emmc\nMACHINE=jetson-agx-orin-devkit-emmc-wendyos\n",
			wantType:    "jetson-agx-orin-emmc",
			wantStorage: "emmc",
		},
		{
			name:     "non-nvme machine infers no storage",
			content:  "BOARD=jetson-orin-nano\nMACHINE=jetson-orin-nano-devkit-wendyos\n",
			wantType: "jetson-orin-nano",
		},
		{
			name:     "machine used only when board absent",
			content:  "MACHINE=raspberrypi5-wendyos\n",
			wantType: "raspberrypi5-wendyos",
		},
		{
			name:        "explicit storage wins over machine inference",
			content:     "BOARD=jetson-orin-nano\nMACHINE=jetson-orin-nano-devkit-nvme-wendyos\nSTORAGE=sd\n",
			wantType:    "jetson-orin-nano",
			wantStorage: "sd",
		},
		{
			name:        "explicit storage parsed alongside board",
			content:     "BOARD=jetson-orin-nano\nSTORAGE=nvme\n",
			wantType:    "jetson-orin-nano",
			wantStorage: "nvme",
		},
		{
			name:     "legacy plain string passthrough",
			content:  "raspberry-pi-5",
			wantType: "raspberry-pi-5",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotStorage := parseDeviceType(tc.content)
			if gotType != tc.wantType || gotStorage != tc.wantStorage {
				t.Fatalf("parseDeviceType(%q) = (%q, %q), want (%q, %q)",
					tc.content, gotType, gotStorage, tc.wantType, tc.wantStorage)
			}
		})
	}
}

func TestGetOSUpdateStatus_NoRecord(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) { svc.osUpdateStateDir = t.TempDir() },
	)
	defer cleanup()

	resp, err := client.GetOSUpdateStatus(context.Background(), &agentpb.GetOSUpdateStatusRequest{})
	if err != nil {
		t.Fatalf("GetOSUpdateStatus: %v", err)
	}
	if resp.GetHasResult() {
		t.Error("HasResult = true, want false when no record exists")
	}
}

func TestGetOSUpdateStatus_MapsRecord(t *testing.T) {
	dir := t.TempDir()
	created := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	finalized := created.Add(3 * time.Minute)
	record := oshealth.UpdateResult{
		Outcome:        oshealth.OutcomeRolledBack,
		OldOSVersion:   "WendyOS-0.10.4",
		NewOSVersion:   "WendyOS-0.11.0",
		CreatedAt:      created,
		FinalizedAt:    finalized,
		FinalOSVersion: "WendyOS-0.10.4",
		Services: []oshealth.ServiceResult{
			{Unit: "avahi-daemon.service", Status: oshealth.StatusFailed, Reason: "timed out after 30s"},
			{Unit: "containerd.service", Status: oshealth.StatusHealthy},
			{Unit: "NetworkManager.service", Status: oshealth.StatusSkipped, Reason: "unit not present on this device"},
		},
		RollbackError: "rollback exploded",
	}
	if err := oshealth.WriteUpdateResult(dir, record); err != nil {
		t.Fatal(err)
	}

	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) { svc.osUpdateStateDir = dir },
	)
	defer cleanup()

	resp, err := client.GetOSUpdateStatus(context.Background(), &agentpb.GetOSUpdateStatusRequest{})
	if err != nil {
		t.Fatalf("GetOSUpdateStatus: %v", err)
	}
	if !resp.GetHasResult() {
		t.Fatal("HasResult = false, want true")
	}
	if resp.GetOutcome() != agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK {
		t.Errorf("Outcome = %v, want OUTCOME_ROLLED_BACK", resp.GetOutcome())
	}
	if resp.GetOldOsVersion() != "WendyOS-0.10.4" || resp.GetNewOsVersion() != "WendyOS-0.11.0" {
		t.Errorf("versions = %q/%q", resp.GetOldOsVersion(), resp.GetNewOsVersion())
	}
	if resp.GetCreatedAtUnix() != created.Unix() {
		t.Errorf("CreatedAtUnix = %d, want %d", resp.GetCreatedAtUnix(), created.Unix())
	}
	if resp.GetFinalizedAtUnix() != finalized.Unix() {
		t.Errorf("FinalizedAtUnix = %d, want %d", resp.GetFinalizedAtUnix(), finalized.Unix())
	}
	if resp.GetRollbackError() != "rollback exploded" {
		t.Errorf("RollbackError = %q", resp.GetRollbackError())
	}

	services := resp.GetServices()
	if len(services) != 3 {
		t.Fatalf("len(Services) = %d, want 3", len(services))
	}
	wantStatuses := []agentpb.GetOSUpdateStatusResponse_ServiceResult_Status{
		agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED,
		agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_HEALTHY,
		agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_SKIPPED,
	}
	for i, want := range wantStatuses {
		if services[i].GetStatus() != want {
			t.Errorf("Services[%d].Status = %v, want %v", i, services[i].GetStatus(), want)
		}
	}
	if services[0].GetUnit() != "avahi-daemon.service" || services[0].GetReason() != "timed out after 30s" {
		t.Errorf("Services[0] = %+v", services[0])
	}
}

func TestGetOSUpdateStatusV2_MirrorsRecord(t *testing.T) {
	dir := t.TempDir()
	record := oshealth.UpdateResult{
		Outcome:   oshealth.OutcomeRolledBack,
		CreatedAt: time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC),
		Services: []oshealth.ServiceResult{
			{Unit: "avahi-daemon.service", Status: oshealth.StatusFailed, Reason: "timed out"},
		},
	}
	if err := oshealth.WriteUpdateResult(dir, record); err != nil {
		t.Fatal(err)
	}

	svc := NewOSUpdateService(zap.NewNop())
	svc.stateDir = dir

	resp, err := svc.GetOSUpdateStatus(context.Background(), &agentpbv2.GetOSUpdateStatusRequest{})
	if err != nil {
		t.Fatalf("GetOSUpdateStatus: %v", err)
	}
	if !resp.GetHasResult() {
		t.Fatal("HasResult = false, want true")
	}
	if resp.GetOutcome() != agentpbv2.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK {
		t.Errorf("Outcome = %v, want OUTCOME_ROLLED_BACK", resp.GetOutcome())
	}
	if len(resp.GetServices()) != 1 ||
		resp.GetServices()[0].GetStatus() != agentpbv2.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED {
		t.Errorf("Services = %+v", resp.GetServices())
	}

	empty := NewOSUpdateService(zap.NewNop())
	empty.stateDir = t.TempDir()
	emptyResp, err := empty.GetOSUpdateStatus(context.Background(), &agentpbv2.GetOSUpdateStatusRequest{})
	if err != nil {
		t.Fatalf("GetOSUpdateStatus (empty): %v", err)
	}
	if emptyResp.GetHasResult() {
		t.Error("HasResult = true, want false when no record exists")
	}
}

func TestGetOSUpdateStatus_ZeroFinalizedAt(t *testing.T) {
	dir := t.TempDir()
	record := oshealth.UpdateResult{
		Outcome:   oshealth.OutcomeCommitted,
		CreatedAt: time.Now(),
	}
	if err := oshealth.WriteUpdateResult(dir, record); err != nil {
		t.Fatal(err)
	}

	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) { svc.osUpdateStateDir = dir },
	)
	defer cleanup()

	resp, err := client.GetOSUpdateStatus(context.Background(), &agentpb.GetOSUpdateStatusRequest{})
	if err != nil {
		t.Fatalf("GetOSUpdateStatus: %v", err)
	}
	if resp.GetOutcome() != agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED {
		t.Errorf("Outcome = %v, want OUTCOME_COMMITTED", resp.GetOutcome())
	}
	if resp.GetFinalizedAtUnix() != 0 {
		t.Errorf("FinalizedAtUnix = %d, want 0 for unfinalized record", resp.GetFinalizedAtUnix())
	}
}

// finishCommittedUpdate must schedule the restart before (and regardless of)
// the ack: once the binary is committed, a client that already dropped its
// transport must not leave the old agent running with the installer lock held
// — that wedges every retry on "an update is already in progress" until a
// manual reboot.
func TestFinishCommittedUpdateRestartsEvenWhenAckFails(t *testing.T) {
	logger := zap.NewNop()

	t.Run("ack failure still schedules the restart and reports success", func(t *testing.T) {
		exitScheduled := false
		err := finishCommittedUpdate(logger,
			func() error { return fmt.Errorf("transport is closing") },
			func() { exitScheduled = true })
		if err != nil {
			t.Fatalf("finishCommittedUpdate = %v, want nil (the update is committed)", err)
		}
		if !exitScheduled {
			t.Fatal("restart exit was not scheduled after a failed ack")
		}
	})

	t.Run("restart is scheduled before the ack is attempted", func(t *testing.T) {
		order := []string{}
		err := finishCommittedUpdate(logger,
			func() error { order = append(order, "ack"); return nil },
			func() { order = append(order, "exit") })
		if err != nil {
			t.Fatalf("finishCommittedUpdate = %v, want nil", err)
		}
		if len(order) != 2 || order[0] != "exit" || order[1] != "ack" {
			t.Fatalf("call order = %v, want [exit ack]", order)
		}
	})
}

// ---------- jetPackVersionFromTegraRelease ----------

func TestJetPackVersionFromTegraRelease(t *testing.T) {
	tests := []struct {
		name    string
		release string
		want    string
	}{
		{
			name:    "JetPack 6.2.1",
			release: "# R36 (release), REVISION: 4.4, GCID: 41834240, BOARD: generic, EABI: aarch64",
			want:    "6.2.1",
		},
		{
			name:    "JetPack 6.2",
			release: "# R36 (release), REVISION: 4.3, GCID: 38968081, BOARD: generic, EABI: aarch64",
			want:    "6.2",
		},
		{
			name:    "JetPack 6.1",
			release: "# R36 (release), REVISION: 4.0, GCID: 37537400, BOARD: generic, EABI: aarch64",
			want:    "6.1",
		},
		{
			name:    "unknown patch in version-sensitive family",
			release: "# R36 (release), REVISION: 4.5, GCID: 50000000, BOARD: generic, EABI: aarch64",
			want:    "L4T-36.4.5",
		},
		{
			name:    "JetPack 7.2 family fallback",
			release: "# R39 (release), REVISION: 2.0, GCID: 50000000, BOARD: generic, EABI: aarch64",
			want:    "7.2",
		},
		{
			name:    "unknown L4T version",
			release: "# R37 (release), REVISION: 1.2, GCID: 40000000, BOARD: generic, EABI: aarch64",
			want:    "L4T-37.1.2",
		},
		{
			name:    "malformed release",
			release: "not an L4T release",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jetPackVersionFromTegraRelease([]byte(tt.release)); got != tt.want {
				t.Errorf("jetPackVersionFromTegraRelease() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------- detectCUDAVersionIn ----------

const nvccVersionOutput = `nvcc: NVIDIA (R) Cuda compiler driver
Copyright (c) 2005-2025 NVIDIA Corporation
Built on Tue_Oct_14_19:22:29_PDT_2025
Cuda compilation tools, release 13.2, V13.2.78
Build cuda_13.2.r13.2/compiler.36123456_0
`

func noLookPath(string) (string, error) {
	return "", fmt.Errorf("not found")
}

func noRunCmd(name string, _ ...string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected command %q", name)
}

func writeCUDAFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDetectCUDAVersion_LegacyVersionTxt(t *testing.T) {
	usrLocal := t.TempDir()
	writeCUDAFile(t, filepath.Join(usrLocal, "cuda", "version.txt"), "CUDA Version 10.2.89")

	if got := detectCUDAVersionIn(usrLocal, noLookPath, noRunCmd); got != "10.2.89" {
		t.Errorf("detectCUDAVersionIn = %q, want %q", got, "10.2.89")
	}
}

func TestDetectCUDAVersion_LegacyVersionJSON(t *testing.T) {
	usrLocal := t.TempDir()
	writeCUDAFile(t, filepath.Join(usrLocal, "cuda", "version.json"),
		`{"cuda": {"name": "CUDA SDK", "version": "12.6.68"}}`)

	if got := detectCUDAVersionIn(usrLocal, noLookPath, noRunCmd); got != "12.6.68" {
		t.Errorf("detectCUDAVersionIn = %q, want %q", got, "12.6.68")
	}
}

func TestDetectCUDAVersion_NvccOnPath(t *testing.T) {
	usrLocal := t.TempDir()
	lookPath := func(name string) (string, error) {
		if name == "nvcc" {
			return "/opt/bin/nvcc", nil
		}
		return "", fmt.Errorf("not found")
	}
	runCmd := func(name string, args ...string) ([]byte, error) {
		if name == "/opt/bin/nvcc" && len(args) == 1 && args[0] == "--version" {
			return []byte(nvccVersionOutput), nil
		}
		return nil, fmt.Errorf("unexpected command %q %v", name, args)
	}

	if got := detectCUDAVersionIn(usrLocal, lookPath, runCmd); got != "13.2" {
		t.Errorf("detectCUDAVersionIn = %q, want %q", got, "13.2")
	}
}

// JetPack 7 (Thor) layout: no /usr/local/cuda symlink, no version files, nvcc
// not on PATH — only /usr/local/cuda-13.2/bin/nvcc exists.
func TestDetectCUDAVersion_JetPack7VersionedDir(t *testing.T) {
	usrLocal := t.TempDir()
	nvcc := filepath.Join(usrLocal, "cuda-13.2", "bin", "nvcc")
	writeCUDAFile(t, nvcc, "")

	runCmd := func(name string, args ...string) ([]byte, error) {
		if name == nvcc && len(args) == 1 && args[0] == "--version" {
			return []byte(nvccVersionOutput), nil
		}
		return nil, fmt.Errorf("unexpected command %q %v", name, args)
	}

	if got := detectCUDAVersionIn(usrLocal, noLookPath, runCmd); got != "13.2" {
		t.Errorf("detectCUDAVersionIn = %q, want %q", got, "13.2")
	}
}

func TestDetectCUDAVersion_VersionFileWinsOverNvcc(t *testing.T) {
	usrLocal := t.TempDir()
	writeCUDAFile(t, filepath.Join(usrLocal, "cuda", "version.json"),
		`{"cuda": {"version": "12.6.68"}}`)
	writeCUDAFile(t, filepath.Join(usrLocal, "cuda-13.2", "bin", "nvcc"), "")

	if got := detectCUDAVersionIn(usrLocal, noLookPath, noRunCmd); got != "12.6.68" {
		t.Errorf("detectCUDAVersionIn = %q, want %q", got, "12.6.68")
	}
}

func TestDetectCUDAVersion_NothingFound(t *testing.T) {
	usrLocal := t.TempDir()

	if got := detectCUDAVersionIn(usrLocal, noLookPath, noRunCmd); got != "" {
		t.Errorf("detectCUDAVersionIn = %q, want empty", got)
	}
}

func TestGetAgentVersionReportsBinarySHA256(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "wendy-agent")
	content := []byte("fake agent binary contents")
	if err := os.WriteFile(binPath, content, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) {
			svc.execPathResolver = func() (string, os.FileMode, error) { return binPath, 0o755, nil }
		},
	)
	defer cleanup()

	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
	if resp.GetBinarySha256() != want {
		t.Errorf("binarySha256 = %q; want %q", resp.GetBinarySha256(), want)
	}

	// The hash means "the binary this process was started from": once
	// computed it must not change even if the file at the exec path is
	// replaced, as happens between a committed update and the restart.
	if err := os.WriteFile(binPath, []byte("replaced by an update"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	resp, err = client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatalf("GetAgentVersion after replace: %v", err)
	}
	if resp.GetBinarySha256() != want {
		t.Errorf("binarySha256 changed to %q after exec path was replaced; want cached %q", resp.GetBinarySha256(), want)
	}
}

func TestWarmBinaryHashPrimesTheCache(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "wendy-agent")
	content := []byte("the binary this process started from")
	if err := os.WriteFile(binPath, content, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	svc := NewAgentService(zap.NewNop(), &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{}, &AgentInstaller{})
	svc.execPathResolver = func() (string, os.FileMode, error) { return binPath, 0o755, nil }
	svc.WarmBinaryHash()

	// binarySHA256 shares the warm goroutine's sync.Once, so this read
	// synchronizes with (or performs) the computation.
	if got := svc.binarySHA256(); got != want {
		t.Fatalf("binarySHA256() = %q, want %q", got, want)
	}

	if err := os.WriteFile(binPath, []byte("replaced by an update"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := svc.binarySHA256(); got != want {
		t.Fatalf("binarySHA256() = %q after exec path replaced, want warmed %q", got, want)
	}
}

func TestGetAgentVersionBinarySHA256EmptyWhenUnreadable(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		func(svc *AgentService) {
			svc.execPathResolver = func() (string, os.FileMode, error) {
				return "", 0, fmt.Errorf("exec path unavailable")
			}
		},
	)
	defer cleanup()

	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatalf("GetAgentVersion must succeed even when hashing fails: %v", err)
	}
	if resp.GetBinarySha256() != "" {
		t.Errorf("binarySha256 = %q; want empty when the executable cannot be hashed", resp.GetBinarySha256())
	}
}
