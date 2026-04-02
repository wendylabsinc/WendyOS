package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
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
func (m *mockNetworkManager) ConnectToWiFi(_ context.Context, _, _ string) error {
	return m.connectErr
}
func (m *mockNetworkManager) GetWiFiStatus(_ context.Context) (bool, string, error) {
	return m.connected, m.ssid, m.statusErr
}
func (m *mockNetworkManager) DisconnectWiFi(_ context.Context) error {
	return m.disconnErr
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
func (m *mockBluetoothManager) Connect(_ context.Context, _ string, _, _ bool) error { return nil }
func (m *mockBluetoothManager) Disconnect(_ context.Context, _ string) error         { return nil }
func (m *mockBluetoothManager) Forget(_ context.Context, _ string) error             { return nil }

// ---------- bufconn helper ----------

const bufSize = 1024 * 1024

func startAgentServer(t *testing.T, nm NetworkManager, hd HardwareDiscoverer, bm BluetoothManager) (agentpb.WendyAgentServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	logger := zap.NewNop()
	svc := NewAgentService(logger, nm, hd, bm)
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
	if resp.Os != runtime.GOOS {
		t.Errorf("os = %q; want %q", resp.Os, runtime.GOOS)
	}
	if resp.CpuArchitecture != runtime.GOARCH {
		t.Errorf("arch = %q; want %q", resp.CpuArchitecture, runtime.GOARCH)
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
	logger := zap.NewNop()
	svc := NewAgentService(logger, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{})

	// Simulate the lock being held.
	svc.updateMu.Lock()
	svc.isUpdating = true
	svc.updateMu.Unlock()

	// Verify the state is set.
	svc.updateMu.Lock()
	if !svc.isUpdating {
		t.Error("expected isUpdating = true after manual set")
	}
	svc.isUpdating = false
	svc.updateMu.Unlock()

	// Verify we can acquire the lock again when not updating.
	svc.updateMu.Lock()
	if svc.isUpdating {
		t.Error("expected isUpdating = false after reset")
	}
	svc.updateMu.Unlock()
}

func TestUpdateAgent_ConcurrentLock(t *testing.T) {
	logger := zap.NewNop()
	svc := NewAgentService(logger, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{})

	// First "update" acquires the lock.
	svc.updateMu.Lock()
	svc.isUpdating = true
	svc.updateMu.Unlock()

	// Second attempt must see that isUpdating is true.
	var wg sync.WaitGroup
	wg.Add(1)
	var blocked bool
	go func() {
		defer wg.Done()
		svc.updateMu.Lock()
		blocked = svc.isUpdating
		svc.updateMu.Unlock()
	}()
	wg.Wait()

	if !blocked {
		t.Error("expected second caller to see isUpdating = true")
	}

	// Cleanup
	svc.updateMu.Lock()
	svc.isUpdating = false
	svc.updateMu.Unlock()
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

// ---- streaming test infrastructure ----

// fakeUpdateServerStream implements grpc.BidiStreamingServer for testing
// receiveAndInstallUpdate. Pre-populate msgs with the message sequence the
// stream should deliver; responses sent by the server accumulate in sent.
type fakeUpdateServerStream struct {
	ctx  context.Context
	msgs []*agentpb.UpdateAgentRequest
	pos  int
	sent []*agentpb.UpdateAgentResponse
}

func (f *fakeUpdateServerStream) Recv() (*agentpb.UpdateAgentRequest, error) {
	if f.pos >= len(f.msgs) {
		return nil, io.EOF
	}
	msg := f.msgs[f.pos]
	f.pos++
	return msg, nil
}

func (f *fakeUpdateServerStream) Send(r *agentpb.UpdateAgentResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func (f *fakeUpdateServerStream) Context() context.Context     { return f.ctx }
func (f *fakeUpdateServerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeUpdateServerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeUpdateServerStream) SetTrailer(metadata.MD)       {}
func (f *fakeUpdateServerStream) SendMsg(any) error            { return nil }
func (f *fakeUpdateServerStream) RecvMsg(any) error            { return nil }

// chunkMsg wraps data in an UpdateAgentRequest chunk message.
func chunkMsg(data []byte) *agentpb.UpdateAgentRequest {
	return &agentpb.UpdateAgentRequest{
		RequestType: &agentpb.UpdateAgentRequest_Chunk_{
			Chunk: &agentpb.UpdateAgentRequest_Chunk{Data: data},
		},
	}
}

// commitMsg wraps a SHA256 hex string in an UpdateAgentRequest commit control message.
func commitMsg(sha256Hash string) *agentpb.UpdateAgentRequest {
	return &agentpb.UpdateAgentRequest{
		RequestType: &agentpb.UpdateAgentRequest_Control{
			Control: &agentpb.UpdateAgentRequest_ControlCommand{
				Command: &agentpb.UpdateAgentRequest_ControlCommand_Update_{
					Update: &agentpb.UpdateAgentRequest_ControlCommand_Update{
						Sha256: sha256Hash,
					},
				},
			},
		},
	}
}

// ---- UpdateAgent streaming-to-disk tests (Iteration 1) ----

// TestCleanupPartialFiles_RemovesAllPartialFilesInBinaryDirectory verifies
// that cleanupPartialFiles unconditionally removes every file matching
// <execName>.partial.* in the binary directory, leaving all other files intact.
func TestCleanupPartialFiles_RemovesAllPartialFilesInBinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "wendy-agent")

	// The binary itself, a backup, and an unrelated file must survive.
	keep := []string{execPath, execPath + ".backup", filepath.Join(dir, "unrelated")}
	for _, name := range keep {
		if err := os.WriteFile(name, []byte("keep"), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", name, err)
		}
	}

	// Two partial files must be removed.
	partials := []string{
		execPath + ".partial.1111111111",
		execPath + ".partial.2222222222",
	}
	for _, p := range partials {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", p, err)
		}
	}

	cleanupPartialFiles(zap.NewNop(), execPath)

	for _, p := range partials {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("partial file %q should have been removed", p)
		}
	}
	for _, k := range keep {
		if _, err := os.Stat(k); err != nil {
			t.Errorf("file %q should still exist: %v", k, err)
		}
	}
}

// midTransferPauseStream wraps fakeUpdateServerStream and blocks on the
// second Recv call (after the first chunk has been processed) so the test
// goroutine can inspect the filesystem before the transfer completes.
type midTransferPauseStream struct {
	fakeUpdateServerStream
	paused chan struct{} // closed when the stream is about to block
	resume chan struct{} // close to unblock
}

func (m *midTransferPauseStream) Recv() (*agentpb.UpdateAgentRequest, error) {
	if m.pos == 1 {
		// First chunk has been delivered and processed; signal the test goroutine
		// and wait for it to finish inspecting the filesystem.
		close(m.paused)
		<-m.resume
	}
	return m.fakeUpdateServerStream.Recv()
}

// TestUpdateAgent_TempFileExistsDuringTransfer verifies that a .partial.*
// temp file is created on disk as soon as the first chunk arrives and is
// still present while subsequent chunks are in-flight, before the commit
// control message is sent.
func TestUpdateAgent_TempFileExistsDuringTransfer(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "wendy-agent")
	if err := os.WriteFile(execPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	content := bytes.Repeat([]byte("y"), 64*1024)
	h := sha256.Sum256(content)
	hash := hex.EncodeToString(h[:])

	paused := make(chan struct{})
	resume := make(chan struct{})
	stream := &midTransferPauseStream{
		fakeUpdateServerStream: fakeUpdateServerStream{
			ctx: context.Background(),
			msgs: []*agentpb.UpdateAgentRequest{
				chunkMsg(content),
				commitMsg(hash),
			},
		},
		paused: paused,
		resume: resume,
	}

	svc := NewAgentService(zap.NewNop(), nil, nil, nil)
	errCh := make(chan error, 1)
	go func() { errCh <- svc.receiveAndInstallUpdate(stream, execPath) }()

	// Wait until the first chunk has been processed and the stream is paused.
	<-paused

	matches, err := filepath.Glob(execPath + ".partial.*")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) == 0 {
		t.Error("no .partial.* temp file found while transfer is in-flight")
	}

	close(resume) // let the transfer finish

	if err := <-errCh; err != nil {
		t.Fatalf("receiveAndInstallUpdate: %v", err)
	}
}

// TestUpdateAgent_InstalledBinaryMatchesSourceContent verifies that after a
// successful update the file installed at the target path is byte-for-byte
// identical to the data that was streamed across all chunks.
func TestUpdateAgent_InstalledBinaryMatchesSourceContent(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "wendy-agent")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	content := bytes.Repeat([]byte("x"), 200*1024) // 200 KiB
	h := sha256.Sum256(content)
	hash := hex.EncodeToString(h[:])

	const chunkSize = 64 * 1024
	var msgs []*agentpb.UpdateAgentRequest
	for off := 0; off < len(content); off += chunkSize {
		end := off + chunkSize
		if end > len(content) {
			end = len(content)
		}
		msgs = append(msgs, chunkMsg(content[off:end]))
	}
	msgs = append(msgs, commitMsg(hash))

	stream := &fakeUpdateServerStream{ctx: context.Background(), msgs: msgs}
	svc := NewAgentService(zap.NewNop(), nil, nil, nil)

	if err := svc.receiveAndInstallUpdate(stream, execPath); err != nil {
		t.Fatalf("receiveAndInstallUpdate: %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("installed binary: got %d bytes, want %d", len(got), len(content))
	}
}

// TestUpdateAgent_SHA256MismatchReturnsErrorAndCleansUp verifies that when
// the SHA256 supplied in the commit control message does not match the hash
// of the received bytes, the RPC returns a DataLoss error and no .partial.*
// file is left behind in the binary directory.
func TestUpdateAgent_SHA256MismatchReturnsErrorAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "wendy-agent")
	original := []byte("original-binary")
	if err := os.WriteFile(execPath, original, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	msgs := []*agentpb.UpdateAgentRequest{
		chunkMsg([]byte("new-content")),
		commitMsg("0000000000000000000000000000000000000000000000000000000000000000"),
	}

	stream := &fakeUpdateServerStream{ctx: context.Background(), msgs: msgs}
	svc := NewAgentService(zap.NewNop(), nil, nil, nil)

	err := svc.receiveAndInstallUpdate(stream, execPath)
	if err == nil {
		t.Fatal("expected error on SHA256 mismatch, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.DataLoss {
		t.Errorf("expected DataLoss, got: %v", err)
	}

	// No .partial.* files must remain.
	matches, _ := filepath.Glob(execPath + ".partial.*")
	if len(matches) > 0 {
		t.Errorf(".partial.* files left behind: %v", matches)
	}

	// Original binary must be unchanged.
	got, _ := os.ReadFile(execPath)
	if !bytes.Equal(got, original) {
		t.Error("original binary was modified on hash mismatch")
	}
}

// TestUpdateAgent_InterruptedStreamDoesNotModifyTargetBinary verifies that
// if the stream terminates (e.g. connection drop) before the commit control
// message arrives, the original binary at the target path is untouched and
// the RPC returns an error.
func TestUpdateAgent_InterruptedStreamDoesNotModifyTargetBinary(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "wendy-agent")
	original := []byte("original-binary")
	if err := os.WriteFile(execPath, original, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Stream ends after some chunks but no commit — simulates a dropped connection.
	msgs := []*agentpb.UpdateAgentRequest{
		chunkMsg([]byte("chunk-1")),
		chunkMsg([]byte("chunk-2")),
	}

	stream := &fakeUpdateServerStream{ctx: context.Background(), msgs: msgs}
	svc := NewAgentService(zap.NewNop(), nil, nil, nil)

	if err := svc.receiveAndInstallUpdate(stream, execPath); err == nil {
		t.Fatal("expected error on interrupted stream, got nil")
	}

	// Original binary must be unchanged.
	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Error("original binary was modified by interrupted stream")
	}

	// No .partial.* files must remain.
	matches, _ := filepath.Glob(execPath + ".partial.*")
	if len(matches) > 0 {
		t.Errorf(".partial.* files left behind after interrupted stream: %v", matches)
	}
}
