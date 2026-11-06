package netmesh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// A normal unanswered TCP connection may overlap a broker disconnect. Close
// must cancel it and join stack users rather than wait for the caller's budget
// or let an endpoint created during stack destruction outlive the network.
func TestNetworkCloseJoinsPendingDialAndRejectsNewWork(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	self, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	peer, _ := meshid.ParseDevice(tenant, "33333333-3333-4333-8333-333333333333")
	n, err := NewNetwork(self)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	p, err := n.AddPeer(1, peer)
	if err != nil {
		t.Fatal(err)
	}
	if err = n.Apply([]babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), Link: 1}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dialDone := make(chan error, 1)
	go func() {
		conn, err := n.DialContext(ctx, netip.AddrPortFrom(peer.MeshAddress(), 9000).String())
		if conn != nil {
			_ = conn.Close()
		}
		dialDone <- err
	}()
	firstPacket := make(chan error, 1)
	go func() { buf := make([]byte, 1280); _, err := p.Read(buf); firstPacket <- err }()
	select {
	case err := <-firstPacket:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending dial did not emit its TCP SYN")
	}
	closed := make(chan struct{})
	go func() { _ = n.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("network shutdown waited for the caller's dial timeout")
	}
	select {
	case err := <-dialDone:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("pending dial after network shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dial survived network shutdown")
	}
	if _, err = p.Read(make([]byte, 1280)); !errors.Is(err, io.EOF) {
		t.Fatal("closed port read", err)
	}
	if _, err = p.Write(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed port write", err)
	}
	if _, err = n.AddPeer(2, peer); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed network accepted a carrier", err)
	}
	if err = n.Apply(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed network accepted new routes", err)
	}
	if err = n.ReconcileBase(); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed backend appears ready", err)
	}
	if conn, err := n.DialContext(ctx, netip.AddrPortFrom(peer.MeshAddress(), 9000).String()); !errors.Is(err, net.ErrClosed) || conn != nil {
		t.Fatal("closed network accepted another dial", err)
	}
	// Multiple ordinary owners can finish concurrently; all observe completed
	// shutdown and none repeats stack destruction.
	var owners sync.WaitGroup
	for range 8 {
		owners.Add(1)
		go func() { defer owners.Done(); _ = n.Close() }()
	}
	owners.Wait()
}

// gonet returns a typed *TCPConn. On a normal failed dial, converting that nil
// pointer straight to net.Conn produces a non-nil interface that panics on Close.
func TestNetworkCanceledDialReturnsNilConnection(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	self, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	peer, _ := meshid.ParseDevice(tenant, "33333333-3333-4333-8333-333333333333")
	n, err := NewNetwork(self)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	p, err := n.AddPeer(1, peer)
	if err != nil {
		t.Fatal(err)
	}
	if err = n.Apply([]babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), Link: 1}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := n.DialContext(ctx, netip.AddrPortFrom(peer.MeshAddress(), 9000).String())
		done <- result{conn, err}
	}()
	emitted := make(chan struct{})
	go func() { _, _ = p.Read(make([]byte, 1280)); close(emitted) }()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("dial did not emit SYN")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.conn != nil {
			t.Fatalf("canceled dial returned connection=%#v error=%v", got.conn, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not stop dial")
	}
}
