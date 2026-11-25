package meshsharing

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestGatewayHeartbeatWaitsForDelayedAcknowledgement(t *testing.T) {
	client, gateway := net.Pipe()
	gateway.SetDeadline(time.Now().Add(2 * time.Second))
	stop := make(chan struct{})
	done := make(chan error, 1)
	const interval = 30 * time.Millisecond
	const minInterval = 10 * time.Millisecond
	go func() { done <- exchangeLeaseHeartbeats(client, stop, interval, minInterval, 2*time.Second) }()
	t.Cleanup(func() {
		close(stop)
		client.Close()
		gateway.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("heartbeat exchange did not stop")
		}
	})
	var request [1]byte
	if _, err := io.ReadFull(gateway, request[:]); err != nil || request[0] != leaseHeartbeat {
		t.Fatalf("initial heartbeat: %v %v", request, err)
	}
	// Hold the ACK longer than several periodic writer ticks. A healthy but
	// slow radio must not make the client pipeline another request.
	gateway.SetReadDeadline(time.Now().Add(3 * interval))
	if _, err := io.ReadFull(gateway, request[:]); err == nil {
		t.Fatal("another heartbeat was sent before the first was acknowledged")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("waiting for the ACK caused an unexpected error: %v", err)
	}
	gateway.SetReadDeadline(time.Time{})
	if _, err := gateway.Write([]byte{leaseHeartbeat}); err != nil {
		t.Fatal(err)
	}
	// A queued ticker must not send immediately once the delayed ACK arrives:
	// the gateway's rate limit is based on arrival time, not the client's timer.
	gateway.SetReadDeadline(time.Now().Add(minInterval / 2))
	if _, err := io.ReadFull(gateway, request[:]); err == nil {
		t.Fatal("heartbeat interval was not measured after the acknowledgement")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("post-ACK wait caused an unexpected error: %v", err)
	}
	gateway.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(gateway, request[:]); err != nil || request[0] != leaseHeartbeat {
		t.Fatalf("next heartbeat: %v %v", request, err)
	}
}

func TestGatewayHeartbeatRejectsInvalidAcknowledgement(t *testing.T) {
	client, gateway := net.Pipe()
	gateway.SetDeadline(time.Now().Add(time.Second))
	defer client.Close()
	defer gateway.Close()
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- exchangeLeaseHeartbeats(client, stop, time.Millisecond, time.Millisecond, time.Second) }()
	var b [1]byte
	if _, err := io.ReadFull(gateway, b[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Write([]byte{leaseHeartbeat + 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invalid acknowledgement accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("invalid acknowledgement did not end the exchange")
	}
}
