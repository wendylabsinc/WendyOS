package meshsharing

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func flushDiagnostics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := asyncdiag.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

type blockedDiagnosticCore struct {
	zapcore.Core
	entered chan struct{}
	release chan struct{}
}

func (c *blockedDiagnosticCore) Enabled(zapcore.Level) bool { return true }
func (c *blockedDiagnosticCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(e, c)
}
func (c *blockedDiagnosticCore) Write(zapcore.Entry, []zapcore.Field) error {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	return nil
}

func TestBlockedDiagnosticSinkDoesNotHoldControllerOrShutdown(t *testing.T) {
	events := []string{}
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events}
	probe := &fakeProbe{}
	c, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	core := &blockedDiagnosticCore{Core: zap.NewNop().Core(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer func() { close(core.release); flushDiagnostics(t) }()
	c.SetLogger(zap.New(core))
	c.mu.Lock()
	if err := c.auditDNS(dnsState{iface: "test0", address: "1.1.1.1"}, nil); err != nil {
		t.Fatal(err)
	}
	c.mu.Unlock()
	select {
	case <-core.entered:
	case <-time.After(time.Second):
		t.Fatal("sink was not reached")
	}
	done := make(chan struct{})
	go func() {
		// Overflow deliberately loses observations rather than backpressuring.
		for range 1000 {
			c.mu.Lock()
			_ = c.auditDNS(dnsState{}, nil)
			c.mu.Unlock()
		}
		_ = c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("diagnostic sink blocked controller/Close")
	}
}

func TestBlockedDiagnosticSinkDoesNotDelaySignedWithdrawal(t *testing.T) {
	var events []string
	now := time.Now()
	donor := policyDonorDevice(t)
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	c, err := NewWithDeps(policySelf(t), node, &fakePolicy{events: &events}, &fakeProbe{gateway: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true}
	for range 2 {
		if _, err := c.Step(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	events = nil
	core := &blockedDiagnosticCore{Core: zap.NewNop().Core(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer func() { close(core.release); flushDiagnostics(t) }()
	c.SetLogger(zap.New(core))
	node.view.Devices[0].Expires = time.Now().Add(-time.Second).UnixMilli()
	done := make(chan struct{})
	go func() { _, _ = c.Step(context.Background(), cfg); _ = c.Close(); close(done) }()
	select {
	case <-core.entered:
	case <-time.After(time.Second):
		t.Fatal("sink not reached")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signed withdrawal/Close waits for diagnostic sink")
	}
	if !slices.Contains(events, "gateway:") {
		t.Fatalf("expired gateway not withdrawn: %v", events)
	}
}
