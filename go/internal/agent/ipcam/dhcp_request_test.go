package ipcam

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestDHCPRequestValidatesExactReservation(t *testing.T) {
	server := net.ParseIP("10.98.0.1")
	for _, tc := range []struct {
		name, server, requested, current string
		known, expired                   bool
		want                             MessageType
	}{
		{name: "select offered address", server: "10.98.0.1", requested: "10.98.0.50", known: true, want: Ack},
		{name: "select different address", server: "10.98.0.1", requested: "10.98.0.51", known: true, want: Nak},
		{name: "select expired offer", server: "10.98.0.1", requested: "10.98.0.50", known: true, expired: true, want: Nak},
		{name: "select unknown offer", server: "10.98.0.1", requested: "10.98.0.50", want: Nak},
		{name: "select other server", server: "10.98.0.2", requested: "10.98.0.50", known: true},
		{name: "select missing address", server: "10.98.0.1", known: true},
		{name: "select with ciaddr", server: "10.98.0.1", requested: "10.98.0.50", current: "10.98.0.50", known: true},
		{name: "reboot matching", requested: "10.98.0.50", known: true, want: Ack},
		{name: "reboot mismatch", requested: "10.98.0.51", known: true, want: Nak},
		{name: "reboot unknown", requested: "10.98.0.50"},
		{name: "reboot expired", requested: "10.98.0.50", known: true, expired: true},
		{name: "renew matching", current: "10.98.0.50", known: true, want: Ack},
		{name: "renew mismatch", current: "10.98.0.51", known: true, want: Nak},
		{name: "renew unknown", current: "10.98.0.50"},
		{name: "missing address", known: true},
		{name: "zero address", requested: "0.0.0.0", known: true},
		{name: "conflicting address fields", requested: "10.98.0.50", current: "10.98.0.50", known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := NewLeasePool(net.ParseIP("10.98.0.50"), 2)
			now := time.Hour
			pool.now = func() (time.Duration, error) { return now, nil }
			mac := testMAC(t)
			if tc.known {
				if _, err := pool.Lease(mac, nil); err != nil {
					t.Fatal(err)
				}
			}
			now += time.Hour
			if tc.expired {
				now += LeaseWindow
			}
			writes := 0
			pool.save = func([]savedLease) error { writes++; return nil }
			kind, addr, err := requestLease(&Packet{CHAddr: mac, ServerID: net.ParseIP(tc.server), RequestedIP: net.ParseIP(tc.requested), CIAddr: net.ParseIP(tc.current)}, server, pool)
			if err != nil || kind != tc.want {
				t.Fatalf("reply = %s, %s, %v; want %s", kind, addr, err, tc.want)
			}
			if tc.want == Ack {
				if addr.String() != "10.98.0.50" || writes != 1 || pool.remaining(mac) != LeaseWindow {
					t.Fatalf("reservation not renewed exactly: %s, writes=%d", addr, writes)
				}
			} else if addr != nil || writes != 0 {
				t.Fatalf("rejected request allocated or renewed an address: %s, writes=%d", addr, writes)
			}
		})
	}
}

func TestDHCPRequestPersistenceFailureDoesNotACKOrExtend(t *testing.T) {
	pool := NewLeasePool(net.ParseIP("10.98.0.50"), 1)
	now := time.Hour
	pool.now = func() (time.Duration, error) { return now, nil }
	mac := testMAC(t)
	addr, err := pool.Lease(mac, nil)
	if err != nil {
		t.Fatal(err)
	}
	now += time.Hour
	failure := errors.New("disk full")
	pool.save = func([]savedLease) error { return failure }
	kind, got, err := requestLease(&Packet{CHAddr: mac, CIAddr: addr}, net.ParseIP("10.98.0.1"), pool)
	if kind != 0 || got != nil || !errors.Is(err, failure) || pool.remaining(mac) != LeaseWindow-time.Hour {
		t.Fatalf("failed persistence acknowledged or extended lease: %s, %s, %v", kind, got, err)
	}
}

func TestDHCPNakDoesNotAdvertiseConfiguration(t *testing.T) {
	req := &Packet{XID: 123, CHAddr: testMAC(t), CIAddr: net.ParseIP("10.98.0.50")}
	wire := BuildReply(req, Nak, ReplyConfig{ServerIP: net.ParseIP("10.98.0.1"), ClientIP: req.CIAddr, Mask: net.CIDRMask(24, 32), Lease: LeaseWindow})
	got, err := ParsePacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != Nak || got.XID != req.XID || !got.YIAddr.IsUnspecified() || !got.CIAddr.IsUnspecified() || !got.SIAddr.IsUnspecified() || !got.ServerID.Equal(net.ParseIP("10.98.0.1")) {
		t.Fatalf("invalid NAK: %+v", got)
	}
	// Only message type and server identifier are allowed in this minimal NAK.
	for i := dhcpMinLen; wire[i] != optEnd; i += 2 + int(wire[i+1]) {
		if wire[i] != optMessageType && wire[i] != optServerID {
			t.Fatalf("NAK contains configuration option %d", wire[i])
		}
	}
}
