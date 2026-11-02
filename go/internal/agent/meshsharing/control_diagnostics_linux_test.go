//go:build linux

package meshsharing

import (
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Use the actual borrower worker without installing a lease in the policy, so
// cleanup cannot call netlink, firewall, resolver or kernel configuration APIs.
func TestBorrowerControlEndDiagnostic(t *testing.T) {
	client, gateway := net.Pipe()
	defer client.Close()
	defer gateway.Close()
	core, logs := observer.New(zap.InfoLevel)
	policy := &linuxTunnelPolicy{}
	controller := &Controller{policy: policy}
	controller.SetLogger(zap.New(core))
	borrowed := &borrowedTunnel{grantedTunnel: grantedTunnel{peer: policyDonorDevice(t), lease: tunnelLease{Slot: 5}}, control: client, done: make(chan struct{}), stop: make(chan struct{})}
	policy.borrowers.Add(1)
	go policy.watchBorrowed(borrowed)
	gateway.SetDeadline(time.Now().Add(time.Second))
	var heartbeat [1]byte
	if _, err := io.ReadFull(gateway, heartbeat[:]); err != nil || heartbeat[0] != leaseHeartbeat {
		t.Fatalf("heartbeat: %v %v", heartbeat, err)
	}
	gateway.Close()
	select {
	case <-borrowed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("borrower worker did not stop")
	}
	entries := logs.FilterMessage("Wendy Net gateway control ended").All()
	if len(entries) != 1 {
		t.Fatalf("control events=%d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["role"] != "borrower" || fields["phase"] != "heartbeat-ack-read" || fields["reason"] != "peer-closed" || fields["peer_principal"] != borrowed.peer.Principal() || fields["slot"] != uint16(5) || fields["error"] == nil {
		t.Fatalf("control termination: %+v", fields)
	}
	if _, misleading := fields["heartbeats"]; misleading {
		t.Fatal("borrower reported an unmeasured heartbeat count")
	}
}
