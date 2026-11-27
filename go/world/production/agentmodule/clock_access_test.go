package agentmodule

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func moduleClockRequest() *appv1.ClockRequest {
	return &appv1.ClockRequest{Nonce: bytes.Repeat([]byte{19}, appv1.ClockNonceSize)}
}

func TestClockAccessReadOnlyScopePausedSourceAndRestart(t *testing.T) {
	f := setupModule(t, accessConfig)
	source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	client := moduleDial(t, accessMount(t, f, source).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := moduleClockRequest()
	first, err := client.Clock(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	w, err := appv1.DecodeClockReply(first)
	if err != nil || !bytes.Equal(first.Nonce, request.Nonce) || first.App != "com.example.fusion" || first.Service != "reader" || first.DeploymentGeneration != 7 || first.Device != f.config.Policy.Device || first.SampleClockDomain != "utc" || first.SampleUnixNs != f.deps.SampleNow().UnixNano() || !w.Earliest.Equal(moduleTestNow) {
		t.Fatal("clock response did not preserve commissioned scope and clock domains", err)
	}
	finalReadAdvance(f.clock, 100*time.Millisecond)
	second, err := client.Clock(ctx, request)
	if err != nil || second.SampleUnixNs != first.SampleUnixNs || second.Physical.EarliestUnixNs <= first.Physical.EarliestUnixNs || second.ServerBootId != first.ServerBootId {
		t.Fatal("physical clock failed to advance independently of paused source", err)
	}
	if out, err := client.Clock(ctx, &appv1.ClockRequest{Nonce: []byte("short")}); status.Code(err) != codes.InvalidArgument || out != nil {
		t.Fatal("malformed nonce accepted", err)
	}
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	f.config.CreateJournal = false
	f.manager, err = New(context.Background(), f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	restarted := moduleDial(t, accessMount(t, f, source).SocketPath)
	third, err := restarted.Clock(ctx, request)
	if err != nil || third.ServerBootId == first.ServerBootId || third.Physical.BootId != first.Physical.BootId {
		t.Fatal("module restart was not distinct from host boot", err)
	}
	f.identity.invalidate()
	if out, err := restarted.Clock(ctx, request); err == nil || out != nil {
		t.Fatal("revoked service retained clock authority")
	}
}

func TestClockResponseFinalizerPreservesBytesAndRefusesOldOrForeignSample(t *testing.T) {
	clock := moduleTestClock()
	policy := basePolicy()
	scope := policy.scope(policy.Grants[0], 1)
	w, _ := clock.Now()
	base := &appv1.ClockReply{Nonce: moduleClockRequest().Nonce, Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, SampleClockDomain: "utc", SampleUnixNs: moduleTestNow.UnixNano(), Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: 1, ServerBootId: strings.Repeat("a", 32)}
	m := finalReaderManager(clock, policy, scope, base.ServerBootId)
	want := proto.Clone(base)
	finalReadAdvance(clock, 500*time.Millisecond)
	if !m.clockResponseCurrent(scope, base) || !proto.Equal(base, want) {
		t.Fatal("finalizer rejected live clock or rewrote sampled bytes")
	}
	foreign := proto.Clone(base).(*appv1.ClockReply)
	foreign.ServerBootId = strings.Repeat("b", 32)
	if m.clockResponseCurrent(scope, foreign) {
		t.Fatal("foreign server incarnation accepted")
	}
	finalReadAdvance(clock, 501*time.Millisecond)
	if m.clockResponseCurrent(scope, base) {
		t.Fatal("sample over response age limit accepted")
	}
}

func TestClockAccessExpiresDuringFinalPeerCheck(t *testing.T) {
	f := setupModule(t, accessConfig)
	source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	client := moduleDial(t, accessMount(t, f, source).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Clock(ctx, moduleClockRequest()); err != nil {
		t.Fatal(err)
	}
	original := f.manager.deps.PeerCheck
	entered, resume := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	f.manager.deps.PeerCheck = func(ctx context.Context, peer appservice.Peer, scope serviceorigin.Scope) error {
		if err := original(ctx, peer, scope); err != nil {
			return err
		}
		if scope.Service == "reader" && calls.Add(1) == 4 {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	type result struct {
		out *appv1.ClockReply
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := client.Clock(ctx, moduleClockRequest()); done <- result{out, err} }()
	finalReadAwait(t, entered)
	finalReadAdvance(f.clock, appv1.MaxClockResponseAge+time.Nanosecond)
	unblock()
	select {
	case got := <-done:
		if got.err == nil || got.out != nil {
			t.Fatal("delayed original sample escaped as fresh clock authority")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delayed clock RPC did not finish")
	}
}
