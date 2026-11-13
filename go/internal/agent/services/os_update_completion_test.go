package services

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// scriptedUpdater installs by running fn, so a test controls what the stream sees.
type scriptedUpdater struct {
	fakeUpdater
	fn func(ctx context.Context, onProgress func(string, int32)) error
}

func (u scriptedUpdater) install(ctx context.Context, _ string, onProgress func(string, int32)) error {
	return u.fn(ctx, onProgress)
}

// heldAckWait outlasts every wait in these tests, so only a client cancel or a
// dropped connection can end the hold, yet a regression fails instead of hanging.
const heldAckWait = 30 * time.Second

func installsCleanly(_ context.Context, onProgress func(string, int32)) error {
	onProgress("write", 100)
	onProgress("verify", -1)
	onProgress("done", 100)
	return nil
}

// systemctlLog records systemctl calls so a test can order them against the reboot.
type systemctlLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *systemctlLog) record(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *systemctlLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type osUpdateHarness struct {
	client  agentpb.WendyAgentServiceClient
	stop    func()
	reboots chan time.Time
	log     *systemctlLog
}

// startOSUpdateHarness serves UpdateOS with a scripted install, a recorded
// reboot and a systemctl stub that reports the auto-updater as installed.
func startOSUpdateHarness(t *testing.T, install func(context.Context, func(string, int32)) error, ackWait time.Duration, rebootErr error) *osUpdateHarness {
	t.Helper()
	h := &osUpdateHarness{reboots: make(chan time.Time, 4), log: &systemctlLog{}}
	orig := systemctlFn
	systemctlFn = func(_ context.Context, args ...string) ([]byte, error) {
		h.log.record(args[0])
		return nil, nil
	}
	client, cleanup := startAgentServer(t, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{}, func(svc *AgentService) {
		svc.isWendyOSHost = func() bool { return true }
		svc.osUpdateStateDir = t.TempDir()
		svc.osUpdateAckWait = ackWait
		svc.selectOSUpdater = func(string, string) (osUpdater, error) {
			return scriptedUpdater{fakeUpdater: fakeUpdater{nameVal: updaterNameWendyOS}, fn: install}, nil
		}
		svc.osUpdateReboot = func(string) error {
			h.log.record("reboot")
			h.reboots <- time.Now()
			return rebootErr
		}
	})
	t.Cleanup(func() {
		cleanup()
		systemctlFn = orig
	})
	h.client = client
	h.stop = cleanup
	return h
}

// recvUntilCompleted reads progress frames until Completed and returns when it arrived.
func recvUntilCompleted(t *testing.T, stream agentpb.WendyAgentService_UpdateOSClient) time.Time {
	t.Helper()
	for {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv before Completed: %v", err)
		}
		if resp.GetFailed() != nil {
			t.Fatalf("update failed: %s", resp.GetFailed().GetErrorMessage())
		}
		if resp.GetCompleted() != nil {
			return time.Now()
		}
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitForReboot(t *testing.T, h *osUpdateHarness, within time.Duration) time.Time {
	t.Helper()
	select {
	case at := <-h.reboots:
		return at
	case <-time.After(within):
		t.Fatalf("no reboot within %v", within)
		return time.Time{}
	}
}

func assertNoReboot(t *testing.T, h *osUpdateHarness, during time.Duration) {
	t.Helper()
	select {
	case <-h.reboots:
		t.Fatal("rebooted unexpectedly")
	case <-time.After(during):
	}
}

func TestUpdateOS_RebootsAsSoonAsClientCancelsAfterCompleted(t *testing.T) {
	h := startOSUpdateHarness(t, installsCleanly, heldAckWait, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := h.client.UpdateOS(ctx, &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	recvUntilCompleted(t, stream)
	// Completed must reach the client before anything restarts the device.
	assertNoReboot(t, h, 200*time.Millisecond)
	cancel()
	waitForReboot(t, h, 5*time.Second)
}

func TestUpdateOS_RebootsAfterAckWaitWhenClientNeverCancels(t *testing.T) {
	const ackWait = 400 * time.Millisecond
	h := startOSUpdateHarness(t, installsCleanly, ackWait, nil)
	stream, err := h.client.UpdateOS(testCtx(t), &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	completedAt := recvUntilCompleted(t, stream)
	rebootAt := waitForReboot(t, h, 5*time.Second)
	// The hold starts when the agent queues Completed, slightly before the client reads it.
	if held := rebootAt.Sub(completedAt); held < ackWait-100*time.Millisecond {
		t.Fatalf("rebooted %v after Completed, want about %v", held, ackWait)
	}
	// Old clients read on after Completed; the stream must end cleanly for them.
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after Completed = %v, want EOF", err)
	}
}

func TestUpdateOS_RebootsWhenClientLeavesBeforeCompleted(t *testing.T) {
	seenDone := make(chan struct{})
	install := func(ctx context.Context, onProgress func(string, int32)) error {
		onProgress("done", 100)
		close(seenDone)
		// The install has finished; the client disconnects before Completed.
		<-ctx.Done()
		return nil
	}
	h := startOSUpdateHarness(t, install, heldAckWait, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	stream, err := h.client.UpdateOS(ctx, &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	<-seenDone
	cancel()
	// A staged update left unbooted would block every later update.
	waitForReboot(t, h, 5*time.Second)
}

func TestUpdateOS_FailedInstallDoesNotReboot(t *testing.T) {
	install := func(_ context.Context, onProgress func(string, int32)) error {
		onProgress("write", 40)
		return errors.New("checksum mismatch")
	}
	h := startOSUpdateHarness(t, install, heldAckWait, nil)
	stream, err := h.client.UpdateOS(testCtx(t), &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if f := resp.GetFailed(); f != nil {
			if f.GetErrorMessage() != "checksum mismatch" {
				t.Fatalf("failure = %q", f.GetErrorMessage())
			}
			break
		}
		if resp.GetCompleted() != nil {
			t.Fatal("Completed after a failed install")
		}
	}
	assertNoReboot(t, h, 300*time.Millisecond)
}

func TestUpdateOS_KeepsLockAndUpdaterStoppedUntilReboot(t *testing.T) {
	h := startOSUpdateHarness(t, installsCleanly, heldAckWait, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := h.client.UpdateOS(ctx, &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	recvUntilCompleted(t, stream)

	// While the reboot is held, a second update must still be refused.
	second, err := h.client.UpdateOS(testCtx(t), &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := second.Recv()
	if err != nil || resp.GetFailed().GetErrorMessage() != updateInProgressMessage {
		t.Fatalf("second UpdateOS = %v, %v; want refusal %q", resp, err, updateInProgressMessage)
	}
	for _, call := range h.log.snapshot() {
		if call == "start" {
			t.Fatal("auto-updater restarted while the reboot was still pending")
		}
	}

	cancel()
	waitForReboot(t, h, 5*time.Second)
	// On hardware the reboot never returns; here the handler unwinds after it,
	// and only then may the deferred restore run.
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls := h.log.snapshot()
		rebootIdx, startIdx := -1, -1
		for i, c := range calls {
			if c == "reboot" && rebootIdx < 0 {
				rebootIdx = i
			}
			if c == "start" && startIdx < 0 {
				startIdx = i
			}
		}
		if startIdx >= 0 {
			if startIdx < rebootIdx {
				t.Fatalf("updater restored before the reboot: %v", calls)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deferred updater restore never ran: %v", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpdateOS_AgentStoppingDuringHoldStillReboots(t *testing.T) {
	h := startOSUpdateHarness(t, installsCleanly, heldAckWait, nil)
	stream, err := h.client.UpdateOS(testCtx(t), &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	recvUntilCompleted(t, stream)
	// Shutting the server down mid-hold must not strand the installed update.
	go h.stop()
	waitForReboot(t, h, 5*time.Second)
}

func TestUpdateOS_RebootFailureStillEndsStream(t *testing.T) {
	h := startOSUpdateHarness(t, installsCleanly, 50*time.Millisecond, errors.New("operation not permitted"))
	stream, err := h.client.UpdateOS(testCtx(t), &agentpb.UpdateOSRequest{ArtifactUrl: "http://example.invalid/u.wendy"})
	if err != nil {
		t.Fatal(err)
	}
	recvUntilCompleted(t, stream)
	waitForReboot(t, h, 5*time.Second)
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after a failed reboot = %v, want EOF", err)
	}
}

func TestNewAgentServiceHoldsRebootForAckWait(t *testing.T) {
	svc := NewAgentService(nil, nil, nil, nil, &AgentInstaller{})
	if svc.osUpdateAckWait != osUpdateAckWait || svc.osUpdateReboot == nil || svc.selectOSUpdater == nil {
		t.Fatalf("UpdateOS hooks not wired: ackWait=%v reboot=%v select=%v",
			svc.osUpdateAckWait, svc.osUpdateReboot != nil, svc.selectOSUpdater != nil)
	}
}
