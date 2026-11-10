//go:build linux

package meshsharing

import (
	"io"
	"net"
	"testing"
	"time"
)

// Exercise the actual borrower loop, without a kernel tunnel or any policy
// mutations. No lease is installed in p, so shutdown never calls netlink.
func TestBorrowedLeaseDoesNotPipelineHeartbeats(t *testing.T) {
	client, gateway := net.Pipe()
	defer gateway.Close()
	b := &borrowedTunnel{control: client, done: make(chan struct{}), stop: make(chan struct{})}
	p := &linuxTunnelPolicy{}
	p.borrowers.Add(1)
	go p.watchBorrowed(b)
	t.Cleanup(func() {
		b.stopOnce.Do(func() { close(b.stop) })
		client.Close()
		select {
		case <-b.done:
		case <-time.After(2 * time.Second):
			t.Error("borrowed heartbeat worker did not stop")
		}
	})
	gateway.SetDeadline(time.Now().Add(leaseHeartbeatInterval + 2*time.Second))
	var request [1]byte
	if _, err := io.ReadFull(gateway, request[:]); err != nil || request[0] != leaseHeartbeat {
		t.Fatalf("initial heartbeat: %v %v", request, err)
	}
	gateway.SetReadDeadline(time.Now().Add(leaseHeartbeatInterval + 2*time.Second))
	if _, err := io.ReadFull(gateway, request[:]); err == nil {
		t.Fatal("borrower pipelined another heartbeat while its ACK was delayed")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("unexpected borrower failure while the ACK was delayed: %v", err)
	}
}
