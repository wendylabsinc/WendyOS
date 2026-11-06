//go:build linux

package ipcam

import (
	"context"
	"errors"
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

// Uses the same isolated-network opt-in and sysctls as the lifecycle tests.
func TestDHCPLinuxLeasesSurviveRestartAndFailClosedOnWriteError(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated test network namespace")
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	ip("link", "add", "leasecam0", "type", "veth", "peer", "name", "leasepeer0")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "leasepeer0").Run() })
	ip("link", "set", "leasecam0", "up")
	ip("link", "set", "leasepeer0", "up")
	// Both veth ends live in this namespace. Give the simulated client an
	// unrelated address so its receive socket works without reserving a camera
	// subnet on what the manager sees as another local interface.
	ip("addr", "add", "192.0.2.2/24", "dev", "leasepeer0")
	path := filepath.Join(t.TempDir(), "camera-dhcp-interfaces")
	start := func(restart bool) (*LinkManager, func()) {
		t.Helper()
		m := NewLinkManager(nil, zap.NewNop())
		if err := m.LoadDHCPAllowlist(path); err != nil {
			t.Fatal(err)
		}
		if !restart {
			if _, err := m.SetDHCPEnabled("leasecam0", true); err != nil {
				t.Fatal(err)
			}
		}
		stopped := make(chan struct{})
		serve := m.serve
		m.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
			defer close(stopped)
			return serve(ctx, link, seg, pool, lease)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stop := func() {
			cancel()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Error("server did not stop")
			}
		}
		t.Cleanup(stop)
		if restart {
			m.scanOnce(ctx)
			m.mu.Lock()
			m.watchingSince["leasecam0"] = time.Now().Add(-unansweredWindow)
			m.mu.Unlock()
			m.scanOnce(ctx)
		} else {
			claimLinuxTestPort(t, m, ctx, "leasecam0")
		}
		return m, stop
	}
	_, stop := start(false)
	udp := leaseTestClient(t, "leasepeer0")
	var xid uint32
	exchange := func(mac byte, kind MessageType, current net.IP, expect bool) *Packet {
		t.Helper()
		xid++
		request := BuildDiscover(xid, net.HardwareAddr{2, 0, 0, 0, 0, mac}, "lease-test")
		request[dhcpMinLen+2] = byte(kind)
		copy(request[12:16], current.To4())
		for attempt := 0; attempt < 4; attempt++ {
			if _, err := udp.WriteToUDP(request, &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
				t.Fatal(err)
			}
			if err := udp.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 1500)
			n, _, err := udp.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			p, err := ParsePacket(buf[:n])
			if err == nil && p.XID == xid {
				if !expect {
					t.Fatalf("DHCP reply sent despite failed persistence: %+v", p)
				}
				return p
			}
		}
		if expect {
			t.Fatal("no DHCP reply")
		}
		return nil
	}
	for mac := byte(1); mac <= 2; mac++ {
		offer := exchange(mac, Discover, nil, true)
		ack := exchange(mac, Request, offer.YIAddr, true)
		if offer.Type != Offer || ack.Type != Ack || !offer.YIAddr.Equal(ack.YIAddr) {
			t.Fatalf("unexpected offer/ack: %+v, %+v", offer, ack)
		}
	}
	stop()
	m, _ := start(true)
	if p := exchange(2, Request, net.ParseIP("10.98.0.51"), true); p.Type != Ack || p.YIAddr.String() != "10.98.0.51" {
		t.Fatalf("renewal changed the existing lease after restart: %+v", p)
	}
	if p := exchange(3, Discover, nil, true); p.Type != Offer || p.YIAddr.String() != "10.98.0.52" {
		t.Fatalf("new camera reused an outstanding lease: %+v", p)
	}
	m.mu.Lock()
	pool := m.pools["leasecam0"]
	m.mu.Unlock()
	pool.mu.Lock()
	pool.save = func([]savedLease) error { return errors.New("disk full") }
	pool.mu.Unlock()
	exchange(2, Request, net.ParseIP("10.98.0.51"), false)
	waitFor(t, "lease write failure status", func() bool {
		for _, iface := range m.DHCPStatus().Interfaces {
			if iface.Name == "leasecam0" {
				return !iface.Serving && iface.State == "blocked" && strings.Contains(iface.Detail, "disk full")
			}
		}
		return false
	})
}

func leaseTestClient(t *testing.T, link string) *net.UDPConn {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "lease-test-client")
	defer f.Close()
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, link); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Port: 68}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.FileConn(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*net.UDPConn)
}

// Exercise the real UDP handler: an ambiguous old address must not be ACKed
// with a different address, and the client must obtain that address via OFFER.
func TestDHCPLinuxQuarantinedRequestRequiresNewOffer(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated test network namespace")
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	ip("link", "add", "reqcam0", "type", "veth", "peer", "name", "reqpeer0")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "reqcam0").Run() })
	ip("link", "set", "reqcam0", "up")
	ip("link", "set", "reqpeer0", "up")
	ip("addr", "add", "10.98.0.1/24", "dev", "reqcam0")
	ip("addr", "add", "10.98.0.2/24", "dev", "reqpeer0")
	legacy := []Camera{{MAC: "02:00:00:00:00:01", Address: "10.98.0.50"}, {MAC: "02:00:00:00:00:02", Address: "10.98.0.50"}}
	pool, err := openLeasePool(leaseTestPath(t), SegmentFor(0), legacy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	acks := make(chan net.IP, 10)
	go func() {
		stopped <- serveDHCP(ctx, "reqcam0", SegmentFor(0), pool, func(_ net.HardwareAddr, addr net.IP, _ string) { acks <- addr })
	}()
	defer func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("DHCP server did not stop")
		}
	}()
	client := leaseTestClient(t, "reqpeer0")
	var xid uint32
	exchange := func(mac byte, kind MessageType, requested byte, selecting bool, want MessageType) *Packet {
		t.Helper()
		xid++
		wire := BuildDiscover(xid, net.HardwareAddr{2, 0, 0, 0, 0, mac}, "camera")
		wire[dhcpMinLen+2] = byte(kind)
		wire = append(wire[:len(wire)-1], optRequestedIP, 4, 10, 98, 0, requested)
		if selecting {
			wire = append(wire, optServerID, 4, 10, 98, 0, 1)
		}
		wire = append(wire, optEnd)
		for range 4 {
			if _, err := client.WriteToUDP(wire, &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpServerPort}); err != nil {
				t.Fatal(err)
			}
			if err := client.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 1500)
			n, _, err := client.ReadFromUDP(buf)
			if err != nil {
				if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
					t.Fatal(err)
				}
				continue
			}
			p, err := ParsePacket(buf[:n])
			if err != nil {
				t.Fatal(err)
			}
			if p.XID != xid {
				continue
			}
			if want == 0 || p.Type != want {
				t.Fatalf("unexpected reply to %s for .%d: %+v", kind, requested, p)
			}
			return p
		}
		if want != 0 {
			t.Fatalf("no %s reply", want)
		}
		return nil
	}
	// Prove the server is receiving packets before testing a silent response.
	exchange(3, Discover, 55, false, Offer)
	exchange(2, Request, 50, false, 0)
	if addr := pool.Leases()["02:00:00:00:00:02"]; addr != nil {
		t.Fatalf("cached-address request allocated %s", addr)
	}
	offer := exchange(2, Discover, 50, false, Offer)
	if offer.YIAddr.String() != "10.98.0.51" {
		t.Fatalf("offer reused quarantined address: %s", offer.YIAddr)
	}
	ack := exchange(2, Request, 51, true, Ack)
	if !ack.YIAddr.Equal(offer.YIAddr) {
		t.Fatalf("ACK %s differs from OFFER %s", ack.YIAddr, offer.YIAddr)
	}
	select {
	case addr := <-acks:
		if !addr.Equal(offer.YIAddr) {
			t.Fatalf("recorded wrong address: %s", addr)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK did not record lease")
	}
	nak := exchange(2, Request, 50, false, Nak)
	if !nak.YIAddr.IsUnspecified() {
		t.Fatalf("NAK advertised address: %s", nak.YIAddr)
	}
	select {
	case addr := <-acks:
		t.Fatalf("rejected request recorded lease: %s", addr)
	default:
	}
}
