package ipcam

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestLeaseBootDeadlinesSurviveRestartsAndReboots(t *testing.T) {
	path := leaseTestPath(t)
	seg := SegmentFor(0)
	boot, uptime := "boot-one", 4*time.Hour
	open := func() *LeasePool {
		t.Helper()
		p, err := openLeasePoolWithClock(path, seg, nil, boot, func() (time.Duration, error) { return uptime, nil })
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := net.HardwareAddr{2, 0, 0, 0, 0, 1}, net.HardwareAddr{2, 0, 0, 0, 0, 2}
	p := open()
	if _, err := p.Lease(a, nil); err != nil {
		t.Fatal(err)
	}
	// The persisted format has no dependency on the device's wall clock.
	saved, _, err := readLeaseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 || saved.BootID != boot || saved.Leases[0].LegacyExpiresAt != nil || saved.Leases[0].ExpiresAfterBoot != 16*time.Hour {
		t.Fatalf("unexpected boot-relative reservation: %+v", saved)
	}
	uptime += time.Hour
	p = open()
	if got := p.remaining(a); got != 11*time.Hour {
		t.Fatalf("restart changed deadline: %s", got)
	}
	if addr, err := p.Lease(b, nil); err != nil || addr.String() != "10.98.0.51" {
		t.Fatalf("restart reused an outstanding address: %s, %v", addr, err)
	}
	// On reboot neither wall-clock differences nor previous uptime prove how
	// long the device was off. Keep old addresses until boot age twelve hours.
	boot, uptime = "boot-two", time.Hour
	p = open()
	if got := p.remaining(a); got != 11*time.Hour {
		t.Fatalf("reboot grace: %s", got)
	}
	uptime += time.Hour
	p = open()
	if got := p.remaining(a); got != 10*time.Hour {
		t.Fatalf("restart extended reboot grace: %s", got)
	}
	uptime = LeaseWindow
	p = open()
	if len(p.Leases()) != 0 {
		t.Fatal("reboot reservations never expired")
	}
	if addr, err := p.Lease(b, nil); err != nil || addr.String() != "10.98.0.50" {
		t.Fatalf("expired address not reusable: %s, %v", addr, err)
	}
}

func TestLeaseClockCorrectionDoesNotExpireLegacyReservations(t *testing.T) {
	path := leaseTestPath(t)
	// This is exactly the old file after leasing with a clock two days behind,
	// then correcting it. The camera's lease has only just started.
	wrongExpiry := time.Now().Add(-48*time.Hour + LeaseWindow)
	file := leaseFile{Version: 1, Leases: []savedLease{{MAC: "02:00:00:00:00:01", Address: "10.98.0.50", LegacyExpiresAt: &wrongExpiry}}}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	uptime := 20 * time.Hour
	open := func() *LeasePool {
		t.Helper()
		p, err := openLeasePoolWithClock(path, SegmentFor(0), nil, "this-boot", func() (time.Duration, error) { return uptime, nil })
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := open()
	if addr, err := p.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 2}, nil); err != nil || addr.String() != "10.98.0.51" {
		t.Fatalf("clock correction lost the client's reservation: %s, %v", addr, err)
	}
	uptime += time.Hour
	p = open()
	if got := p.remaining(net.HardwareAddr{2, 0, 0, 0, 0, 1}); got != 11*time.Hour {
		t.Fatalf("legacy migration repeated on restart: %s", got)
	}
}

func TestLeaseMigrationQuarantinesHistoricalReuseAndThenExpires(t *testing.T) {
	path := leaseTestPath(t)
	legacy := []Camera{
		{MAC: "02:00:00:00:00:01", Address: "10.98.0.50"},
		{MAC: "02:00:00:00:00:02", Address: "10.98.0.50"},
		{MAC: "02:00:00:00:00:03", Address: "10.98.0.55"},
	}
	uptime := time.Hour
	open := func() *LeasePool {
		t.Helper()
		p, err := openLeasePoolWithClock(path, SegmentFor(0), legacy, "this-boot", func() (time.Duration, error) { return uptime, nil })
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := open()
	if len(p.quarantined) != 1 || p.quarantined["10.98.0.50"] != 13*time.Hour {
		t.Fatalf("ambiguous address not quarantined: %v", p.quarantined)
	}
	if got := p.Leases()[legacy[2].MAC]; got.String() != "10.98.0.55" {
		t.Fatalf("unique binding lost: %s", got)
	}
	// Neither historical MAC owns the ambiguous address or can release it.
	for _, camera := range legacy[:2] {
		mac, _ := net.ParseMAC(camera.MAC)
		if err := p.Release(mac); err != nil {
			t.Fatal(err)
		}
		if addr, err := p.Lease(mac, net.ParseIP("10.98.0.50")); err != nil || addr.String() == "10.98.0.50" {
			t.Fatalf("ambiguous historical owner received quarantined IP: %s, %v", addr, err)
		}
	}
	uptime += time.Hour
	p = open()
	if p.quarantined["10.98.0.50"] != 13*time.Hour {
		t.Fatal("restart extended quarantine")
	}
	uptime = 13 * time.Hour
	p = open()
	if addr, err := p.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 4}, nil); err != nil || addr.String() != "10.98.0.50" {
		t.Fatalf("expired quarantine was not reclaimed: %s, %v", addr, err)
	}
	p = open()
	if len(p.quarantined) != 0 {
		t.Fatal("registry history was reimported")
	}
}

func TestLeaseClockFailureCannotAllocateOrRelease(t *testing.T) {
	p := NewLeasePool(SegmentFor(0).PoolBase, 1)
	a := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	if _, err := p.Lease(a, nil); err != nil {
		t.Fatal(err)
	}
	p.now = func() (time.Duration, error) { return 0, errors.New("boot clock unavailable") }
	if _, err := p.Lease(a, nil); err == nil {
		t.Fatal("allocated without a clock")
	}
	if err := p.Release(a); err == nil {
		t.Fatal("released without a clock")
	}
	if p.remaining(a) != 0 || len(p.Leases()) != 1 {
		t.Fatal("clock failure promised or forgot a reservation")
	}
}
