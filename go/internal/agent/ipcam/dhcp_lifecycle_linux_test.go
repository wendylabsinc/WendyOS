//go:build linux

package ipcam

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Run only in an isolated Linux container with NET_ADMIN. This checks real
// DHCP replies after a CLI-equivalent disable of an interface's current name.
func TestDHCPLinuxRenamedPortStopsOfferingAfterDisable(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated test network namespace")
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	ip("link", "add", "reviewcam0", "type", "veth", "peer", "name", "reviewpeer0")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "reviewpeer0").Run() })
	ip("link", "set", "reviewcam0", "up")
	ip("link", "set", "reviewpeer0", "up")
	// The simulated peer is also visible to the manager in this namespace;
	// keep its fixture address outside the camera subnets being allocated.
	ip("addr", "add", "192.0.2.2/24", "dev", "reviewpeer0")
	m := NewLinkManager(nil, zap.NewNop())
	if err := m.LoadDHCPAllowlist(filepath.Join(t.TempDir(), "camera-dhcp-interfaces")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetDHCPEnabled("reviewcam0", true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimLinuxTestPort(t, m, ctx, "reviewcam0")

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "reviewpeer0"); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Port: 68}); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "review-client")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	udp := conn.(*net.UDPConn)
	offered := func(xid uint32) bool {
		t.Helper()
		for attempt := 0; attempt < 4; attempt++ {
			request := BuildDiscover(xid, net.HardwareAddr{2, 0, 0, 0, 0, 9}, "review-camera")
			if _, err := udp.WriteToUDP(request, &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
				t.Fatal(err)
			}
			udp.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			buf := make([]byte, 1500)
			n, _, err := udp.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			p, err := ParsePacket(buf[:n])
			if err == nil && p.Type == Offer && p.XID == xid {
				return true
			}
		}
		return false
	}
	if !offered(123) {
		t.Fatal("initial camera server did not answer; fixture is not working")
	}
	ip("link", "set", "reviewcam0", "down")
	ip("link", "set", "reviewcam0", "name", "reviewcam1")
	ip("link", "set", "reviewcam1", "up")
	result, err := m.SetDHCPEnabled("reviewcam1", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range result.Interfaces {
		if iface.Name == "reviewcam1" && (iface.Enabled || iface.Serving || len(iface.IPv4s) != 0) {
			t.Fatalf("renamed port still configured after disable: %+v", iface)
		}
	}
	if offered(456) {
		t.Fatal("real DHCP OFFER received after successful disable of the current interface name")
	}
}

func TestDHCPLinuxRestartResumesRetainedSegment(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated test network namespace")
	}
	for _, args := range [][]string{{"link", "add", "reviewcam0", "type", "dummy"}, {"link", "set", "reviewcam0", "up"}} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "reviewcam0").Run() })
	dir := t.TempDir()
	path := filepath.Join(dir, "camera-dhcp-interfaces")
	m := NewLinkManager(nil, zap.NewNop())
	if err := m.LoadDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetDHCPEnabled("reviewcam0", true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	serve := m.serve
	m.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
		defer close(stopped)
		return serve(ctx, link, seg, pool, lease)
	}
	claimLinuxTestPort(t, m, ctx, "reviewcam0")
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("old server failed to stop")
	}
	// No camera registry is needed: saved permission and the retained address
	// are sufficient to recover a fresh manager's segment.
	m = NewLinkManager(nil, zap.NewNop())
	if err := m.LoadDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	m.scanOnce(ctx)
	if m.isClaimed("reviewcam0") {
		t.Fatal("recovered without waiting for the competing-server guard")
	}
	m.mu.Lock()
	m.watchingSince["reviewcam0"] = time.Now().Add(-unansweredWindow)
	m.mu.Unlock()
	m.scanOnce(ctx)
	seg, claimed := m.claimedSegment("reviewcam0")
	if !claimed || seg.CIDR() != "10.98.0.1/24" {
		ifaces, _ := m.listInterfaces()
		t.Logf("interfaces: %+v; status: %+v", ifaces, m.DHCPStatus())
		t.Fatalf("retained Linux address was not recovered: %+v, %v", seg, claimed)
	}
	if _, err := m.SetDHCPEnabled("reviewcam0", false); err != nil {
		t.Fatal(err)
	}
	ifaces, err := m.listInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Name == "reviewcam0" && iface.HasIPv4 {
			t.Fatalf("disable left the recovered address configured: %+v", iface)
		}
	}
	if _, err := m.SetDHCPEnabled("reviewcam0", true); err != nil {
		t.Fatalf("could not re-enable after disabling recovered segment: %v", err)
	}
}

func claimLinuxTestPort(t *testing.T, m *LinkManager, ctx context.Context, name string) {
	t.Helper()
	ifaces, err := m.listInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Name == name {
			m.guard.RestoreClaim(name)
			m.claim(ctx, iface, nil, ifaces)
			return
		}
	}
	t.Fatalf("interface %s not found", name)
}

// A prefix change removes the camera route without changing the server IP.
// Exercise kernel address inspection and cleanup as well as server cancellation.
func TestDHCPLinuxPrefixChangeStopsServingAndPreservesForeignPrefix(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated test network namespace")
	}
	ip := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
		return string(out)
	}
	ip("link", "add", "prefixcam0", "type", "dummy")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "prefixcam0").Run() })
	ip("link", "set", "prefixcam0", "up")
	ip("addr", "add", "10.98.0.1/24", "dev", "prefixcam0")
	m := NewLinkManager(nil, zap.NewNop())
	if err := m.LoadDHCPAllowlist(filepath.Join(t.TempDir(), "camera-dhcp-interfaces")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetDHCPEnabled("prefixcam0", true); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	serve := m.serve
	m.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
		err := serve(ctx, link, seg, pool, lease)
		stopped <- err
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.scanOnce(ctx)
	m.mu.Lock()
	m.watchingSince["prefixcam0"] = time.Now().Add(-unansweredWindow)
	m.mu.Unlock()
	m.scanOnce(ctx)
	if !m.isClaimed("prefixcam0") {
		t.Fatal("fixture did not resume camera segment")
	}
	if route := ip("route", "get", "10.98.0.50"); !strings.Contains(route, "dev prefixcam0") {
		t.Fatal("fixture lacks camera route: " + route)
	}
	ip("addr", "del", "10.98.0.1/24", "dev", "prefixcam0")
	ip("addr", "add", "10.98.0.1/32", "dev", "prefixcam0")
	m.scanOnce(ctx)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prefix change did not stop DHCP")
	}
	if m.isClaimed("prefixcam0") {
		t.Fatal("prefix change retained claim")
	}
	found := false
	for _, s := range m.DHCPStatus().Interfaces {
		if s.Name == "prefixcam0" {
			found = true
			if s.State != "blocked" || s.Serving || !strings.Contains(s.Detail, "conflicting IPv4") {
				t.Fatalf("conflict not surfaced: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("missing interface status")
	}
	m.scanOnce(ctx)
	addresses := ip("-4", "-o", "addr", "show", "dev", "prefixcam0")
	if !strings.Contains(addresses, "10.98.0.1/32") || strings.Contains(addresses, "10.98.0.1/24") {
		t.Fatalf("changed prefix was removed or overwritten: %s", addresses)
	}
}
