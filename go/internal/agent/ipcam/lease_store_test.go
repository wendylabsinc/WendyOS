package ipcam

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func leaseTestPath(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("requires root-owned lease storage")
	}
	return filepath.Join(t.TempDir(), "camera-dhcp-leases-10.98.0.1.json")
}

func TestLeaseStorageRestartRenewalReleaseAndExpiry(t *testing.T) {
	path := leaseTestPath(t)
	seg := SegmentFor(0)
	open := func() *LeasePool {
		t.Helper()
		p, err := openLeasePool(path, seg, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := open()
	a, b, c := net.HardwareAddr{2, 0, 0, 0, 0, 1}, net.HardwareAddr{2, 0, 0, 0, 0, 2}, net.HardwareAddr{2, 0, 0, 0, 0, 3}
	for _, mac := range []net.HardwareAddr{a, b} {
		if _, err := p.Lease(mac, nil); err != nil {
			t.Fatal(err)
		}
	}
	expiry := p.expires[a.String()]
	p = open()
	if p.expires[a.String()] != expiry {
		t.Fatal("loading extended a lease")
	}
	if got, err := p.Lease(c, nil); err != nil || got.String() != "10.98.0.52" {
		t.Fatalf("new client took an existing lease: %s, %v", got, err)
	}
	// A renewal has ciaddr rather than option 50; the saved MAC binding is
	// sufficient to preserve the address even without a preference.
	p.now = func() (time.Duration, error) { return expiry - LeaseWindow + time.Hour, nil }
	if got, err := p.Lease(a, nil); err != nil || got.String() != "10.98.0.50" {
		t.Fatalf("renewal: %s, %v", got, err)
	}
	p = open()
	if p.expires[a.String()] != expiry+time.Hour {
		t.Fatal("renewal expiry was not persisted")
	}
	if err := p.Release(b); err != nil {
		t.Fatal(err)
	}
	p = open()
	d := net.HardwareAddr{2, 0, 0, 0, 0, 4}
	if got, err := p.Lease(d, nil); err != nil || got.String() != "10.98.0.51" {
		t.Fatalf("released address was not reusable: %s, %v", got, err)
	}
	p.now = func() (time.Duration, error) { return expiry + 2*time.Hour, nil }
	if got, err := p.Lease(b, nil); err != nil || got.String() != "10.98.0.50" {
		t.Fatalf("expired reservations were not reclaimed: %s, %v", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("lease storage permissions: %v, %v", info, err)
	}
}

func TestLeasePersistenceFailureDoesNotPromiseOrFreeAddresses(t *testing.T) {
	p := NewLeasePool(SegmentFor(0).PoolBase, 1)
	a, b := net.HardwareAddr{2, 0, 0, 0, 0, 1}, net.HardwareAddr{2, 0, 0, 0, 0, 2}
	diskFull := errors.New("disk full")
	p.save = func([]savedLease) error { return diskFull }
	if ip, err := p.Lease(a, nil); !errors.Is(err, diskFull) || ip != nil || len(p.Leases()) != 0 {
		t.Fatalf("failed save promised an address: %s, %v", ip, err)
	}
	p.save = nil
	if _, err := p.Lease(a, nil); err != nil {
		t.Fatal(err)
	}
	expires := p.expires[a.String()]
	p.save = func([]savedLease) error { return diskFull }
	if _, err := p.Lease(a, nil); err == nil || p.expires[a.String()] != expires {
		t.Fatal("failed renewal changed the reservation")
	}
	if err := p.Release(a); !errors.Is(err, diskFull) {
		t.Fatalf("failed release: %v", err)
	}
	if _, err := p.Lease(b, nil); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("failed release freed the address: %v", err)
	}
}

func TestLeaseAdvertisedLifetimeExcludesPersistenceDelay(t *testing.T) {
	p := NewLeasePool(SegmentFor(0).PoolBase, 1)
	now := time.Hour
	p.now = func() (time.Duration, error) { return now, nil }
	p.save = func([]savedLease) error { now += time.Minute; return nil }
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	if _, err := p.Lease(mac, nil); err != nil {
		t.Fatal(err)
	}
	if got := p.remaining(mac); got != LeaseWindow-time.Minute {
		t.Fatalf("advertised lifetime %s outlives the persisted reservation", got)
	}
}

func TestLeaseStorageMigratesRegistryOnlyOnce(t *testing.T) {
	path := leaseTestPath(t)
	legacy := []Camera{{MAC: "02:00:00:00:00:01", Address: "10.98.0.50"}}
	p, err := openLeasePool(path, SegmentFor(0), legacy)
	if err != nil {
		t.Fatal(err)
	}
	expires := p.expires[legacy[0].MAC]
	legacy = append(legacy, Camera{MAC: "02:00:00:00:00:02", Address: "10.98.0.51"})
	p, err = openLeasePool(path, SegmentFor(0), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Leases()) != 1 || p.expires[legacy[0].MAC] != expires {
		t.Fatal("restart reimported the registry or extended its migration reservation")
	}
	now, err := leaseBootTime()
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := leaseBootID()
	if err != nil {
		t.Fatal(err)
	}
	records := []savedLease{{MAC: legacy[0].MAC, Address: legacy[0].Address, ExpiresAfterBoot: now - time.Second}}
	if err := saveLeaseFile(path, bootID, records); err != nil {
		t.Fatal(err)
	}
	p, err = openLeasePool(path, SegmentFor(0), legacy)
	if err != nil || len(p.Leases()) != 0 {
		t.Fatalf("expired record resurrected from registry: %v, %v", p, err)
	}
}

func TestLeaseStorageRejectsInvalidRecords(t *testing.T) {
	valid := savedLease{MAC: "02:00:00:00:00:01", Address: "10.98.0.50", ExpiresAfterBoot: time.Hour}
	for _, mode := range []string{"truncated", "version", "missing-boot", "invalid-deadline", "owned-quarantine", "duplicate-address", "duplicate-mac", "outside-pool", "invalid-mac", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			path := leaseTestPath(t)
			file := leaseFile{Version: 2, BootID: "previous-boot", Leases: []savedLease{valid}}
			switch mode {
			case "version":
				file.Version = 3
			case "missing-boot":
				file.BootID = ""
			case "invalid-deadline":
				file.Leases[0].ExpiresAfterBoot = 0
			case "owned-quarantine":
				file.Leases[0].Quarantined = true
			case "duplicate-address":
				other := valid
				other.MAC = "02:00:00:00:00:02"
				file.Leases = append(file.Leases, other)
			case "duplicate-mac":
				other := valid
				other.Address = "10.98.0.51"
				file.Leases = append(file.Leases, other)
			case "outside-pool":
				file.Leases[0].Address = "192.168.1.2"
			case "invalid-mac":
				file.Leases[0].MAC = "invalid"
			}
			data, err := json.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "truncated" {
				data = data[:len(data)/2]
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "symlink" {
				if err := os.Symlink(path, path+".link"); err != nil {
					t.Fatal(err)
				}
				path += ".link"
			}
			if _, err := openLeasePool(path, SegmentFor(0), nil); err == nil {
				t.Fatal("invalid lease storage was accepted")
			}
		})
	}
}

func TestManagerRestoresLeaseReservationsAcrossRestart(t *testing.T) {
	path := leaseTestPath(t)
	config := filepath.Join(filepath.Dir(path), "camera-dhcp-interfaces")
	newManager := func() *managerHarness {
		h := newManagerHarness(t, []Interface{cabledEth("eth0")})
		configureDHCPControl(t, h, map[string]bool{"eth0": true})
		h.mgr.allowlistPath = config
		h.mgr.openPool = openLeasePool
		return h
	}
	h := newManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)
	h.mu.Lock()
	p := h.poolsSeen["eth0"]
	h.mu.Unlock()
	if _, err := p.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 1}, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	h = newManager()
	h.addForeignAddress("eth0", "10.98.0.1/24")
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "restart watcher", func() bool { return len(h.watchedLinks()) == 1 })
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "recovered server", func() bool { return len(h.servedLinks()) == 1 })
	h.mu.Lock()
	p = h.poolsSeen["eth0"]
	h.mu.Unlock()
	if got, err := p.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 2}, nil); err != nil || got.String() != "10.98.0.51" {
		t.Fatalf("new client took the old client's address after restart: %s, %v", got, err)
	}
}

func TestManagerLeaseStorageFailureAppearsInStatus(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	h.addForeignAddress("eth0", "10.98.0.1/24")
	h.mgr.openPool = func(string, CameraSegment, []Camera) (*LeasePool, error) {
		return nil, errors.New("corrupt lease file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	got := h.mgr.DHCPStatus().Interfaces[0]
	if h.mgr.isClaimed("eth0") || got.State != "blocked" || !strings.Contains(got.Detail, "corrupt lease file") {
		t.Fatalf("lease storage failure was hidden: %+v", got)
	}
}

func TestManagerReusedLeasePoolSurvivesStaleServerFailure(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	contexts := make(chan context.Context, 2)
	serve := h.mgr.serve
	h.mgr.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
		contexts <- ctx
		return serve(ctx, link, seg, pool, lease)
	}
	claimEth0(t, h, ctx)
	oldContext := <-contexts
	h.mu.Lock()
	oldPool := h.poolsSeen["eth0"]
	h.mu.Unlock()
	if _, err := oldPool.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 1}, nil); err != nil {
		t.Fatal(err)
	}
	h.mgr.scanMu.Lock()
	h.mgr.release("eth0")
	h.mgr.scanMu.Unlock()
	h.feed("eth0", &Packet{Type: Discover})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	newContext := <-contexts
	waitFor(t, "replacement server", func() bool { return len(h.servedLinks()) == 2 })
	h.mu.Lock()
	newPool := h.poolsSeen["eth0"]
	h.mu.Unlock()
	if newPool != oldPool {
		t.Fatal("replacement lost outstanding reservations")
	}
	// An old server can fail before cancellation but wait behind a scan until
	// after its replacement starts. Pool identity alone no longer distinguishes it.
	h.mgr.recoverFailedServer(oldContext, "eth0", SegmentFor(0), oldPool, errors.New("old socket failure"))
	if !h.mgr.isClaimed("eth0") || newContext.Err() != nil {
		t.Fatal("stale failure tore down the replacement server")
	}
	if ip, err := newPool.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 2}, nil); err != nil || ip.String() != "10.98.0.51" {
		t.Fatalf("replacement reused an outstanding lease: %s, %v", ip, err)
	}
}
