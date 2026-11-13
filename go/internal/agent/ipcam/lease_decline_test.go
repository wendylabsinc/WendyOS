package ipcam

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestDeclineQuarantinesAddressAndRetriesFailedPersistence(t *testing.T) {
	p := NewLeasePool(net.IPv4(10, 98, 0, 50), 3)
	now := time.Hour
	p.now = func() (time.Duration, error) { return now, nil }
	mac := testMAC(t)
	addr, err := p.Lease(mac, nil)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("disk full")
	p.save = func([]savedLease) error { return failure }
	if err := p.decline(mac, addr); !errors.Is(err, failure) {
		t.Fatalf("decline error = %v, want persistence failure", err)
	}
	p.save = func(records []savedLease) error {
		for _, r := range records {
			if r.Address == addr.String() && r.Quarantined && r.MAC == "" {
				return nil
			}
		}
		t.Fatal("allocation did not retry persisting the address conflict")
		return nil
	}
	other := net.HardwareAddr{2, 0, 0, 0, 0, 2}
	next, err := p.Lease(other, addr)
	if err != nil || next.Equal(addr) {
		t.Fatalf("reused declined address: %s, %v", next, err)
	}
	p.save = nil
	now += LeaseWindow
	if again, err := p.Lease(mac, addr); err != nil || !again.Equal(addr) {
		t.Fatalf("expired conflict did not return to pool: %s, %v", again, err)
	}
}

func TestReleaseAndDeclineRequireServerAndExactBinding(t *testing.T) {
	for _, kind := range []MessageType{Release, Decline} {
		for _, wrong := range []string{"server", "address", "client", "missing server", "valid"} {
			t.Run(kind.String()+"/"+wrong, func(t *testing.T) {
				p := NewLeasePool(net.IPv4(10, 98, 0, 50), 2)
				mac := testMAC(t)
				addr, err := p.Lease(mac, nil)
				if err != nil {
					t.Fatal(err)
				}
				server := net.IPv4(10, 98, 0, 1)
				req := &Packet{Type: kind, CHAddr: mac, ServerID: server}
				if kind == Release {
					req.CIAddr = addr
				} else {
					req.RequestedIP = addr
				}
				switch wrong {
				case "server":
					req.ServerID = net.IPv4(10, 98, 0, 2)
				case "missing server":
					req.ServerID = nil
				case "client":
					req.CHAddr = net.HardwareAddr{2, 0, 0, 0, 0, 99}
				case "address":
					if kind == Release {
						req.CIAddr = net.IPv4(10, 98, 0, 51)
					} else {
						req.RequestedIP = net.IPv4(10, 98, 0, 51)
					}
				}
				if err := endClientLease(req, server, p); err != nil {
					t.Fatal(err)
				}
				_, held := p.Holder(addr)
				if held != (wrong != "valid") {
					t.Fatalf("binding held = %v after %s", held, wrong)
				}
			})
		}
	}
}
