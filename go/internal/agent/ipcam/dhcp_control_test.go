package ipcam

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func configureDHCPControl(t *testing.T, h *managerHarness, initial map[string]bool) {
	t.Helper()
	saved := maps.Clone(initial)
	h.mgr.readAllowlist = func(string) (map[string]bool, error) { return maps.Clone(saved), nil }
	h.mgr.writeAllowlist = func(_ string, allowed map[string]bool) error {
		saved = maps.Clone(allowed)
		return nil
	}
	if err := h.mgr.LoadDHCPAllowlist("camera-dhcp-interfaces"); err != nil {
		t.Fatal(err)
	}
}

func TestDHCPControlPersistsAndDisablesWithoutEnumeration(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	configureDHCPControl(t, h, map[string]bool{})
	result, err := h.mgr.SetDHCPEnabled("eth0", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Interfaces[0]; !got.Enabled || got.Serving || got.State != "waiting" {
		t.Fatalf("enabled port must wait for guard: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	serve := h.mgr.serve
	h.mgr.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, onLease func(net.HardwareAddr, net.IP, string)) error {
		defer close(stopped)
		return serve(ctx, link, seg, pool, onLease)
	}
	h.mgr.scanOnce(ctx)
	waitFor(t, "both ports to be watched", func() bool { return len(h.watchedLinks()) == 2 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to serve", func() bool { return len(h.servedLinks()) == 1 })
	if _, err := h.mgr.SetDHCPEnabled("eth1", true); err != nil {
		t.Fatal(err)
	}
	if got := h.mgr.DHCPStatus().Interfaces[0]; !got.Serving || got.State != "serving" {
		t.Fatalf("active port: %+v", got)
	}
	h.mgr.listInterfaces = func() ([]Interface, error) { return nil, errors.New("netlink unavailable") }
	result, err = h.mgr.SetDHCPEnabled("eth0", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.InterfaceError == "" || h.mgr.isClaimed("eth0") {
		t.Fatalf("disable result: %+v", result)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("server was not cancelled")
	}
	saved, err := h.mgr.readAllowlist("")
	if err != nil || saved["eth0"] || !saved["eth1"] {
		t.Fatalf("saved configuration: %v, %v", saved, err)
	}
	// Reloading retains the other port rather than replacing the whole list.
	if err := h.mgr.LoadDHCPAllowlist("camera-dhcp-interfaces"); err != nil {
		t.Fatal(err)
	}
	if !h.mgr.dhcpAllowed("eth1") || h.mgr.dhcpAllowed("eth0") {
		t.Fatal("saved settings did not survive reload")
	}
}

func TestDHCPControlRejectsUnsafeEnable(t *testing.T) {
	uplink := cabledEth("uplink0")
	uplink.HasIPv4, uplink.IPv4s = true, []net.IP{net.ParseIP("192.168.1.2")}
	wifi := cabledEth("wlan0")
	loopback := cabledEth("lo")
	loopback.Loopback = true
	h := newManagerHarness(t, []Interface{uplink, wifi, loopback})
	configureDHCPControl(t, h, map[string]bool{})
	for _, tc := range []struct {
		name string
		want error
	}{
		{"eth0\neth1", ErrInvalidDHCPInterface}, {"", ErrInvalidDHCPInterface}, {"..", ErrInvalidDHCPInterface},
		{"missing0", ErrDHCPInterfaceMissing}, {"uplink0", ErrDHCPInterfaceBlocked},
		{"wlan0", ErrDHCPInterfaceBlocked}, {"lo", ErrDHCPInterfaceBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.mgr.SetDHCPEnabled(tc.name, true); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	saved, _ := h.mgr.readAllowlist("")
	if len(saved) != 0 {
		t.Fatalf("invalid request changed permissions: %v", saved)
	}
}

func TestDHCPControlWriteFailurePreservesPreviousSettings(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	h.mgr.writeAllowlist = func(string, map[string]bool) error { return errors.New("disk full") }
	for _, tc := range []struct {
		name   string
		enable bool
	}{{"eth0", false}, {"eth1", true}} {
		if _, err := h.mgr.SetDHCPEnabled(tc.name, tc.enable); err == nil {
			t.Fatal("failed write reported success")
		}
	}
	if !h.mgr.dhcpAllowed("eth0") || h.mgr.dhcpAllowed("eth1") {
		t.Fatal("failed write changed permission")
	}
}

func TestDHCPControlConcurrentUpdatesPreserveBothPorts(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	configureDHCPControl(t, h, map[string]bool{})
	var wg sync.WaitGroup
	for _, name := range []string{"eth0", "eth1"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.mgr.SetDHCPEnabled(name, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	saved, _ := h.mgr.readAllowlist("")
	if !saved["eth0"] || !saved["eth1"] {
		t.Fatalf("lost concurrent update: %v", saved)
	}
}

func TestDHCPControlDisableCannotRaceAnInflightClaim(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "watcher", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	adding, proceed := make(chan struct{}), make(chan struct{})
	addAddress := h.mgr.addAddress
	h.mgr.addAddress = func(link, cidr string) error {
		close(adding)
		<-proceed
		return addAddress(link, cidr)
	}
	scanned := make(chan struct{})
	go func() { h.mgr.scanOnce(ctx); close(scanned) }()
	<-adding
	disabled := make(chan error, 1)
	go func() { _, err := h.mgr.SetDHCPEnabled("eth0", false); disabled <- err }()
	close(proceed)
	<-scanned
	if err := <-disabled; err != nil {
		t.Fatal(err)
	}
	if h.mgr.isClaimed("eth0") || h.mgr.dhcpAllowed("eth0") {
		t.Fatal("inflight claim survived completed disable")
	}
	h.mgr.scanOnce(ctx)
	if h.mgr.isClaimed("eth0") {
		t.Fatal("revoked claim was restored on the next scan")
	}
}

func TestDHCPStatusReportsBlockedMissingAndErrors(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true, "gone0": true})
	h.mgr.guard.Disqualify("eth0")
	got := h.mgr.DHCPStatus()
	if len(got.Interfaces) != 3 || got.Interfaces[0].State != "blocked" || got.Interfaces[1].State != "disabled" || got.Interfaces[2].State != "missing" {
		t.Fatalf("status: %+v", got)
	}
	h.mgr.readAllowlist = func(string) (map[string]bool, error) { return nil, errors.New("insecure configuration") }
	got = h.mgr.DHCPStatus()
	if got.ConfigurationError == "" || got.Interfaces[0].Enabled {
		t.Fatalf("invalid configuration: %+v", got)
	}
	h.mgr.listInterfaces = func() ([]Interface, error) { return nil, errors.New("interface inspection failed") }
	got = h.mgr.DHCPStatus()
	if got.InterfaceError == "" {
		t.Fatal("interface inspection failure was hidden")
	}
}

func TestWriteDHCPAllowlistPersistsSecurely(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	// The first CLI change also creates a missing agent configuration directory.
	path := filepath.Join(t.TempDir(), "config", "camera-dhcp-interfaces")
	if got, err := readDHCPAllowlist(path); err != nil || len(got) != 0 {
		t.Fatalf("new device configuration = %v, %v", got, err)
	}
	if err := writeDHCPAllowlist(path, map[string]bool{"eth1": true, "eth0": true}); err != nil {
		t.Fatal(err)
	}
	got, err := readDHCPAllowlist(path)
	if err != nil || len(got) != 2 || !got["eth0"] || !got["eth1"] {
		t.Fatalf("saved config = %v, %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("insecure file: %v, %v", info, err)
	}
	if err := writeDHCPAllowlist(path, map[string]bool{"eth1": true}); err != nil {
		t.Fatal(err)
	}
	got, err = readDHCPAllowlist(path)
	if err != nil || got["eth0"] || !got["eth1"] {
		t.Fatalf("updated config = %v, %v", got, err)
	}
	if err := writeDHCPAllowlist(path, map[string]bool{"eth0\neth1": true}); err == nil {
		t.Fatal("accepted injection")
	}
	got, err = readDHCPAllowlist(path)
	if err != nil || len(got) != 1 || !got["eth1"] {
		t.Fatalf("failed write damaged config: %v, %v", got, err)
	}
}
