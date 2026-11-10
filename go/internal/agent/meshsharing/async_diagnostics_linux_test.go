//go:build linux

package meshsharing

import (
	"context"
	"go.uber.org/zap"
	"io"
	"net"
	"testing"
	"time"
)

func TestBlockedDiagnosticSinkDoesNotHoldBorrowerWorker(t *testing.T) {
	client, gateway := net.Pipe()
	defer client.Close()
	defer gateway.Close()
	b := &borrowedTunnel{control: client, done: make(chan struct{}), stop: make(chan struct{})}
	// No installed lease: no netlink, conntrack or firewall calls are possible.
	p := &linuxTunnelPolicy{}
	core := &blockedDiagnosticCore{Core: zap.NewNop().Core(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer func() { close(core.release); flushDiagnostics(t) }()
	p.SetLogger(zap.New(core))
	p.borrowers.Add(1)
	go p.watchBorrowed(b)
	_ = gateway.SetDeadline(time.Now().Add(time.Second))
	var heartbeat [1]byte
	if _, err := io.ReadFull(gateway, heartbeat[:]); err != nil {
		t.Fatal(err)
	}
	_ = gateway.Close()
	select {
	case <-core.entered:
	case <-time.After(time.Second):
		t.Fatal("sink not reached")
	}
	done := make(chan struct{})
	go func() { p.borrowers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("borrower shutdown waits for diagnostic sink")
	}
	select {
	case <-b.done:
	default:
		t.Fatal("borrower done not closed")
	}
}
func TestBlockedDiagnosticSinkDoesNotDelaySharingDisable(t *testing.T) {
	p := &linuxTunnelPolicy{sharing: "offline-fixture"}
	core := &blockedDiagnosticCore{Core: zap.NewNop().Core(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer func() { close(core.release); flushDiagnostics(t) }()
	p.SetLogger(zap.New(core))
	done := make(chan error, 1)
	go func() { done <- p.SetSharing(context.Background(), "", "") }()
	select {
	case <-core.entered:
	case <-time.After(time.Second):
		t.Fatal("sink not reached")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sharing disable waits for diagnostic sink")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sharing != "" {
		t.Fatal("sharing remains enabled")
	}
}
