package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc/metadata"
)

func TestCloudContextSupportsFreshTokenOnlySession(t *testing.T) {
	auth := &config.AuthConfig{
		APIKey:         "access-token",
		OAuthIssuer:    "https://auth.dev.wendy.sh/realms/acme",
		OAuthExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	ctx, err := cloudContext(context.Background(), auth)
	if err != nil {
		t.Fatalf("cloudContext: %v", err)
	}
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok || len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer access-token" {
		t.Fatalf("authorization metadata = %v", md.Get("authorization"))
	}
}

func TestDialCloudGRPCSupportsTokenOnlyPublicTLS(t *testing.T) {
	conn, err := dialCloudGRPC(&config.AuthConfig{CloudGRPC: "api.dev.wendy.sh:443", APIKey: "access-token"})
	if err != nil {
		t.Fatalf("dialCloudGRPC: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestParseTunnelArg(t *testing.T) {
	tests := []struct {
		arg        string
		wantLocal  uint32
		wantRemote uint32
		wantErr    bool
	}{
		{"8080", 8080, 8080, false},
		{"3000:8080", 3000, 8080, false},
		{"0", 0, 0, true},
		{"99999", 0, 0, true},
		{"abc", 0, 0, true},
		{"8080:abc", 0, 0, true},
		{"65535", 65535, 65535, false},
		{"1:65535", 1, 65535, false},
	}

	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			local, remote, _, err := parseTunnelArg(tt.arg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTunnelArg(%q) expected error, got none", tt.arg)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTunnelArg(%q) unexpected error: %v", tt.arg, err)
			}
			if local != tt.wantLocal || remote != tt.wantRemote {
				t.Errorf("parseTunnelArg(%q) = (%d, %d), want (%d, %d)", tt.arg, local, remote, tt.wantLocal, tt.wantRemote)
			}
		})
	}
}

func cloudAssetFixture(id int32, name string) *cloudpb.Asset {
	a := &cloudpb.Asset{Id: id}
	if name != "" {
		a.Name = name
	}
	return a
}

func TestResolveCloudAssetByNameAndID(t *testing.T) {
	assets := []*cloudpb.Asset{cloudAssetFixture(41, "playful-reed"), cloudAssetFixture(42, "")}
	got, err := resolveCloudAsset(assets, "playful-reed")
	if err != nil || got.GetId() != 41 {
		t.Fatalf("by name: got %v, err %v", got, err)
	}
	got, err = resolveCloudAsset(assets, "42")
	if err != nil || got.GetId() != 42 {
		t.Fatalf("by id: got %v, err %v", got, err)
	}
	if _, err = resolveCloudAsset(assets, "nope"); err == nil {
		t.Fatal("expected error for unknown device")
	}
}

func TestResolveCloudAssetAmbiguousListsIDs(t *testing.T) {
	assets := []*cloudpb.Asset{cloudAssetFixture(41, "a"), cloudAssetFixture(42, "b")}
	_, err := resolveCloudAsset(assets, "")
	if err == nil {
		t.Fatal("expected ambiguity error with no --device")
	}
	if !strings.Contains(err.Error(), "41") || !strings.Contains(err.Error(), "42") {
		t.Fatalf("error should list candidate IDs, got: %v", err)
	}
}

func TestResolveCloudAssetNotFoundIsTyped(t *testing.T) {
	t.Run("miss among non-empty assets", func(t *testing.T) {
		assets := []*cloudpb.Asset{cloudAssetFixture(41, "playful-reed")}
		_, err := resolveCloudAsset(assets, "nope")
		var notFound *errCloudDeviceNotFound
		if !errors.As(err, &notFound) {
			t.Fatalf("expected errCloudDeviceNotFound, got %T: %v", err, err)
		}
		wantMsg := `no device named or with id "nope" found; run 'wendy cloud discover --json' to list ids`
		if err.Error() != wantMsg {
			t.Fatalf("Error() = %q, want %q", err.Error(), wantMsg)
		}
	})

	t.Run("empty list with a device name", func(t *testing.T) {
		_, err := resolveCloudAsset(nil, "playful-reed")
		var notFound *errCloudDeviceNotFound
		if !errors.As(err, &notFound) {
			t.Fatalf("expected errCloudDeviceNotFound, got %T: %v", err, err)
		}
		wantMsg := `no device named or with id "playful-reed" found; run 'wendy cloud discover --json' to list ids`
		if err.Error() != wantMsg {
			t.Fatalf("Error() = %q, want %q", err.Error(), wantMsg)
		}
	})

	t.Run("empty list without a device name stays untyped", func(t *testing.T) {
		_, err := resolveCloudAsset(nil, "")
		var notFound *errCloudDeviceNotFound
		if errors.As(err, &notFound) {
			t.Fatalf("unnamed empty-list case should NOT be typed as errCloudDeviceNotFound, got %v", err)
		}
		wantMsg := "no enrolled devices found for this org; enroll a device with 'wendy device enroll' first"
		if err.Error() != wantMsg {
			t.Fatalf("Error() = %q, want %q", err.Error(), wantMsg)
		}
	})
}

// fetchAllStub is an injectable stand-in for the real
// fetchCloudAssetsFiltered(ctx, auth, false) call, so
// TestUpgradeOfflineResolveErr can run as a pure function test with no
// network access. called records whether upgradeOfflineResolveErr actually
// invoked it (used to assert passthrough cases short-circuit before the
// offline re-query).
type fetchAllStub struct {
	assets []*cloudpb.Asset
	err    error
	called bool
}

func (s *fetchAllStub) fetch() ([]*cloudpb.Asset, error) {
	s.called = true
	return s.assets, s.err
}

func TestUpgradeOfflineResolveErr(t *testing.T) {
	t.Run("offline hit upgrades the message", func(t *testing.T) {
		resolveErr := &errCloudDeviceNotFound{name: "playful-reed"}
		stub := &fetchAllStub{assets: []*cloudpb.Asset{cloudAssetFixture(41, "playful-reed")}}

		got := upgradeOfflineResolveErr(resolveErr, "playful-reed", stub.fetch)

		if !stub.called {
			t.Fatal("expected fetchAll to be called")
		}
		if !strings.Contains(got.Error(), "enrolled but currently reported offline") {
			t.Fatalf("got %q, want it to mention being enrolled but offline", got.Error())
		}
	})

	t.Run("truly missing device keeps the original error", func(t *testing.T) {
		resolveErr := &errCloudDeviceNotFound{name: "playful-reed"}
		stub := &fetchAllStub{assets: []*cloudpb.Asset{cloudAssetFixture(41, "other-device")}}

		got := upgradeOfflineResolveErr(resolveErr, "playful-reed", stub.fetch)

		if !stub.called {
			t.Fatal("expected fetchAll to be called")
		}
		if got != error(resolveErr) {
			t.Fatalf("got %v, want the original resolveErr unchanged", got)
		}
	})

	t.Run("fetchAll error keeps the original error", func(t *testing.T) {
		resolveErr := &errCloudDeviceNotFound{name: "playful-reed"}
		stub := &fetchAllStub{err: fmt.Errorf("network down")}

		got := upgradeOfflineResolveErr(resolveErr, "playful-reed", stub.fetch)

		if !stub.called {
			t.Fatal("expected fetchAll to be called")
		}
		if got != error(resolveErr) {
			t.Fatalf("got %v, want the original resolveErr unchanged", got)
		}
	})

	t.Run("ambiguity error passes through without calling fetchAll", func(t *testing.T) {
		resolveErr := fmt.Errorf("multiple cloud devices found; rerun with --device <id|name> (41=a, 42=b)")
		stub := &fetchAllStub{assets: []*cloudpb.Asset{cloudAssetFixture(41, "a"), cloudAssetFixture(42, "b")}}

		got := upgradeOfflineResolveErr(resolveErr, "", stub.fetch)

		if stub.called {
			t.Fatal("expected fetchAll NOT to be called for an ambiguity error")
		}
		if got != resolveErr {
			t.Fatalf("got %v, want the original resolveErr unchanged", got)
		}
	})

	t.Run("unnamed empty-list bonus: all enrolled devices offline", func(t *testing.T) {
		resolveErr := errNoCloudDevicesEnrolled
		stub := &fetchAllStub{assets: []*cloudpb.Asset{cloudAssetFixture(41, "a"), cloudAssetFixture(42, "b"), cloudAssetFixture(43, "c")}}

		got := upgradeOfflineResolveErr(resolveErr, "", stub.fetch)

		if !stub.called {
			t.Fatal("expected fetchAll to be called")
		}
		if !strings.Contains(got.Error(), "all 3 enrolled devices are currently reported offline") {
			t.Fatalf("got %q, want it to mention all 3 enrolled devices being offline", got.Error())
		}
	})
}

// TestTunnelUplinkAcceptsWritesWhileSendStalls is the core WDY-2433
// regression test: with send() gated shut (simulating stream.Send blocked on
// broker flow control during a bulk chunk upload), writes on the local pipe
// end must still complete promptly -- the bounded queue decouples remote.Read
// from send so a stalled Send never blocks the pipe (and, in the real
// tunneled transport, never starves keepalive PING/ACK frames behind it).
func TestTunnelUplinkAcceptsWritesWhileSendStalls(t *testing.T) {
	local, remote := net.Pipe()

	gate := make(chan struct{})
	var mu sync.Mutex
	var got [][]byte
	send := func(payload []byte, halfClose bool) error {
		<-gate
		if halfClose {
			return nil
		}
		mu.Lock()
		got = append(got, append([]byte(nil), payload...))
		mu.Unlock()
		return nil
	}
	var closeSendCalls atomic.Int32
	closeSend := func() error {
		closeSendCalls.Add(1)
		return nil
	}

	done := make(chan struct{})
	go func() {
		runTunnelUplink(remote, send, closeSend)
		close(done)
	}()

	const (
		numChunks = 64
		chunkSize = 4096
	)
	var chunks [][]byte
	for i := 0; i < numChunks; i++ {
		chunk := make([]byte, chunkSize)
		for j := range chunk {
			chunk[j] = byte(i)
		}
		chunks = append(chunks, chunk)
	}

	writeErr := make(chan error, 1)
	go func() {
		for _, chunk := range chunks {
			if _, err := local.Write(chunk); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("local.Write failed while send was stalled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writes did not complete within 2s while send was stalled -- reader blocked behind send?")
	}

	// Release the gated sends and let the pump drain to completion.
	close(gate)
	local.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTunnelUplink did not finish after send was unblocked")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != numChunks {
		t.Fatalf("got %d payloads, want %d", len(got), numChunks)
	}
	if !bytes.Equal(bytes.Join(got, nil), bytes.Join(chunks, nil)) {
		t.Fatal("concatenated payloads do not match writes byte-for-byte and in order")
	}
	if closeSendCalls.Load() != 1 {
		t.Fatalf("closeSend called %d times, want 1", closeSendCalls.Load())
	}
}

// TestTunnelUplinkForwardsHalfCloseAndClosesSend closes the local pipe end
// (the caller-side half-close), which must surface as io.EOF on remote.Read,
// forward a half-close item through the queue, and finally call closeSend
// exactly once -- after the half-close has drained, not before or instead.
func TestTunnelUplinkForwardsHalfCloseAndClosesSend(t *testing.T) {
	local, remote := net.Pipe()

	var mu sync.Mutex
	var events []string
	send := func(payload []byte, halfClose bool) error {
		mu.Lock()
		defer mu.Unlock()
		if halfClose {
			events = append(events, "halfClose")
			return nil
		}
		events = append(events, fmt.Sprintf("data:%s", payload))
		return nil
	}
	closeSend := func() error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "closeSend")
		return nil
	}

	done := make(chan struct{})
	go func() {
		runTunnelUplink(remote, send, closeSend)
		close(done)
	}()

	if _, err := local.Write([]byte("hello")); err != nil {
		t.Fatalf("local.Write: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatalf("local.Close: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTunnelUplink did not finish after local half-close")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"data:hello", "halfClose", "closeSend"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

// TestTunnelUplinkClosesPipeOnSendError verifies the sender is the sole
// toucher of send/closeSend, and that a send error closes remote so a caller
// blocked writing into the local pipe end unblocks with an error instead of
// hanging forever (the latent hang the old single-goroutine pump had: on a
// send failure it broke its loop without ever closing remote). It also
// checks for goroutine leaks via a done-signal: both the reader and sender
// must exit promptly once the pipe is torn down.
func TestTunnelUplinkClosesPipeOnSendError(t *testing.T) {
	local, remote := net.Pipe()

	sendErr := errors.New("broker send boom")
	var sendCalls atomic.Int32
	send := func(payload []byte, halfClose bool) error {
		sendCalls.Add(1)
		return sendErr
	}
	var closeSendCalls atomic.Int32
	closeSend := func() error {
		closeSendCalls.Add(1)
		return nil
	}

	done := make(chan struct{})
	go func() {
		runTunnelUplink(remote, send, closeSend)
		close(done)
	}()

	// First write rendezvous with the reader and triggers the failing send.
	if _, err := local.Write([]byte("x")); err != nil {
		t.Fatalf("first local.Write: %v", err)
	}

	// A subsequent write must fail once remote is closed behind the send
	// error. remote.Close() runs asynchronously right after the failing
	// send() returns, so a single write can legitimately win the race and
	// rendezvous with the reader's next Read just before the close lands;
	// retry until one fails (or the deadline expires) rather than asserting
	// on exactly one attempt.
	writeErr := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := local.Write([]byte("y")); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()

	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("local.Write did not fail within deadline after send error -- remote not closed? goroutine leak?")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write-retry loop did not report within deadline after send error")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTunnelUplink did not exit after send error -- goroutine leak")
	}

	if closeSendCalls.Load() != 1 {
		t.Fatalf("closeSend called %d times, want 1", closeSendCalls.Load())
	}
	if sendCalls.Load() != 1 {
		t.Fatalf("send called %d times, want exactly 1 (no retry, no send after error)", sendCalls.Load())
	}
}

// TestCloudKeepaliveProfiles is a guard-rail, not a behavior test: it pins the
// invariants the tunneled and broker keepalive profiles must satisfy so a
// future edit can't silently violate them. Real coverage of the tunneled
// dial's keepalive-under-flow-control behavior lives in the uplink-pump
// (A8) and end-to-end (A10) tests.
func TestCloudKeepaliveProfiles(t *testing.T) {
	// The agent enforces a minimum client ping interval via
	// grpc.KeepaliveEnforcementPolicy{MinTime: 10 * time.Second} in
	// internal/agent/mtls/server.go:108. Any client-side ping interval below
	// this floor gets the connection torn down with ENHANCE_YOUR_CALM.
	const agentMinTime = 10 * time.Second

	if tunneledKeepalivePing < agentMinTime {
		t.Errorf("tunneledKeepalivePing = %v, want >= agent MinTime %v (mtls/server.go:108)", tunneledKeepalivePing, agentMinTime)
	}
	if tunneledKeepaliveACKTimeout <= clouddefaults.KeepaliveACKTimeout {
		t.Errorf("tunneledKeepaliveACKTimeout = %v, want > clouddefaults.KeepaliveACKTimeout (%v) -- the tunneled profile is a slow end-to-end backstop, not the liveness probe", tunneledKeepaliveACKTimeout, clouddefaults.KeepaliveACKTimeout)
	}
	if tunneledKeepalivePing <= clouddefaults.KeepalivePing {
		t.Errorf("tunneledKeepalivePing = %v, want > clouddefaults.KeepalivePing (%v) -- the tunneled profile is a slow end-to-end backstop, not the liveness probe", tunneledKeepalivePing, clouddefaults.KeepalivePing)
	}
	if clouddefaults.KeepalivePing < agentMinTime {
		t.Errorf("clouddefaults.KeepalivePing = %v, want >= agent MinTime %v (mtls/server.go:108) -- this is the broker/cloud-API liveness ping", clouddefaults.KeepalivePing, agentMinTime)
	}
}
