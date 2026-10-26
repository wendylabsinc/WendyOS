package services

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/agent/oshealth"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestAgentServerCleanupWaitsForUpdaterRestore(t *testing.T) {
	original := systemctlFn
	restoreEntered := make(chan struct{})
	restoreReleased := make(chan struct{})
	restoreFinished := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(restoreReleased) }) }
	cleanup := func() {}
	t.Cleanup(func() {
		release()
		cleanup()
		systemctlFn = original
	})
	systemctlFn = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "start" {
			close(restoreEntered)
			<-restoreReleased
			close(restoreFinished)
		}
		return nil, nil
	}
	client, stopServer := startAgentServer(t, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{}, func(svc *AgentService) {
		svc.isWendyOSHost = func() bool { return true }
		svc.osUpdateStateDir = t.TempDir()
	})
	cleanup = stopServer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.UpdateOS(ctx, &agentpb.UpdateOSRequest{
		ArtifactUrl: "http://example.invalid/update.wendy", UpdaterBackend: "unrecognized-test-backend",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil || response.GetFailed() == nil {
		t.Fatalf("expected backend rejection, got %v: %v", response, err)
	}
	select {
	case <-restoreEntered:
	case <-ctx.Done():
		t.Fatal("updater restore was not reached")
	}
	stopped := make(chan struct{})
	go func() { cleanup(); close(stopped) }()
	returnedEarly := false
	select {
	case <-stopped:
		returnedEarly = true
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-restoreFinished
	<-stopped
	if returnedEarly {
		t.Fatal("test server cleanup returned while the RPC still used the systemctl hook")
	}
}

// countSystemctl stubs systemctlFn and counts calls, so a test can prove a
// refused update never reached inhibitAutoUpdater.
func countSystemctl(t *testing.T) *atomic.Int32 {
	t.Helper()
	orig := systemctlFn
	t.Cleanup(func() { systemctlFn = orig })
	var calls atomic.Int32
	systemctlFn = func(context.Context, ...string) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	}
	return &calls
}

// writePendingMarker records an installed update as if it happened in the boot
// identified by bootID.
func writePendingMarker(t *testing.T, dir, bootID string) {
	t.Helper()
	if err := oshealth.WritePendingMarker(dir, oshealth.PendingMarker{
		CreatedAt: time.Now(),
		BootID:    bootID,
		Backend:   updaterNameWendyOS,
	}); err != nil {
		t.Fatal(err)
	}
}

func currentBootIDOrSkip(t *testing.T) string {
	t.Helper()
	id := oshealth.CurrentBootID()
	if id == "" {
		t.Skip("no kernel boot ID on this host")
	}
	return id
}

func updateOSV1(t *testing.T, opt func(*AgentService)) string {
	t.Helper()
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
		opt,
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
	if resp.GetFailed() == nil {
		t.Fatalf("UpdateOS response = %T, want failed", resp.GetResponseType())
	}
	return resp.GetFailed().GetErrorMessage()
}

func updateOSV2(t *testing.T, svc *OSUpdateService) string {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer(grpc.WaitForHandlers(true))
	agentpbv2.RegisterWendyOSUpdateServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	stream, err := agentpbv2.NewWendyOSUpdateServiceClient(conn).UpdateOS(context.Background(),
		&agentpbv2.UpdateOSRequest{ArtifactUrl: "http://example.invalid/update.wendy"})
	if err != nil {
		t.Fatalf("UpdateOS: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("UpdateOS Recv: %v", err)
	}
	if resp.GetFailed() == nil {
		t.Fatalf("UpdateOS response = %T, want failed", resp.GetResponseType())
	}
	return resp.GetFailed().GetErrorMessage()
}

func TestUpdateOS_RefusesWhileUpdateInProgress(t *testing.T) {
	calls := countSystemctl(t)
	var hostChecked atomic.Bool
	msg := updateOSV1(t, func(svc *AgentService) {
		svc.isWendyOSHost = func() bool { hostChecked.Store(true); return true }
		svc.installer.TryLock()
	})

	if msg != updateInProgressMessage {
		t.Fatalf("error message = %q, want %q", msg, updateInProgressMessage)
	}
	if hostChecked.Load() || calls.Load() != 0 {
		t.Error("a refused update must not probe the host or touch the auto-updater")
	}
}

func TestUpdateOSV2_RefusesWhileUpdateInProgress(t *testing.T) {
	calls := countSystemctl(t)
	var hostChecked atomic.Bool
	installer := &AgentInstaller{}
	installer.TryLock()
	svc := NewOSUpdateService(zap.NewNop(), installer)
	svc.isWendyOSHost = func() bool { hostChecked.Store(true); return true }

	if msg := updateOSV2(t, svc); msg != updateInProgressMessage {
		t.Fatalf("error message = %q, want %q", msg, updateInProgressMessage)
	}
	if hostChecked.Load() || calls.Load() != 0 {
		t.Error("a refused update must not probe the host or touch the auto-updater")
	}
}

func TestUpdateOS_RefusesWhileUpdateAwaitsReboot(t *testing.T) {
	calls := countSystemctl(t)
	dir := t.TempDir()
	writePendingMarker(t, dir, currentBootIDOrSkip(t))

	msg := updateOSV1(t, func(svc *AgentService) {
		svc.isWendyOSHost = func() bool { return true }
		svc.osUpdateStateDir = dir
	})
	if msg != osUpdateAwaitingRebootMessage {
		t.Fatalf("error message = %q, want %q", msg, osUpdateAwaitingRebootMessage)
	}
	if calls.Load() != 0 {
		t.Error("a refused update must not touch the auto-updater")
	}
}

func TestUpdateOSV2_RefusesWhileUpdateAwaitsReboot(t *testing.T) {
	countSystemctl(t)
	dir := t.TempDir()
	writePendingMarker(t, dir, currentBootIDOrSkip(t))

	svc := NewOSUpdateService(zap.NewNop(), &AgentInstaller{})
	svc.isWendyOSHost = func() bool { return true }
	svc.stateDir = dir

	if msg := updateOSV2(t, svc); msg != osUpdateAwaitingRebootMessage {
		t.Fatalf("error message = %q, want %q", msg, osUpdateAwaitingRebootMessage)
	}
}

func TestUpdateOS_PendingMarkerFromEarlierBootDoesNotBlock(t *testing.T) {
	countSystemctl(t)
	dir := t.TempDir()
	writePendingMarker(t, dir, "earlier-boot")

	msg := updateOSV1(t, func(svc *AgentService) {
		svc.isWendyOSHost = func() bool { return true }
		svc.osUpdateStateDir = dir
	})
	if msg == osUpdateAwaitingRebootMessage || msg == updateInProgressMessage {
		t.Fatalf("update was refused by a marker from an earlier boot: %q", msg)
	}
}
