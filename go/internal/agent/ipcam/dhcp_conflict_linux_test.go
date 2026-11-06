//go:build linux

package ipcam

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

type dhcpConflictLink struct {
	m      *LinkManager
	client *net.UDPConn
	seg    CameraSegment
	pool   *LeasePool
	xid    uint32
}

func TestDHCPLinuxIPv6ConfiguredPortCannotBeEnabled(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated test network namespace")
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	ip("link", "add", "ipv6cam0", "type", "veth", "peer", "name", "ipv6peer0")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "ipv6cam0").Run() })
	ip("link", "set", "ipv6cam0", "up")
	ip("link", "set", "ipv6peer0", "up")
	ip("-6", "addr", "add", "fdab:1234::1/64", "dev", "ipv6cam0")
	m := NewLinkManager(nil, zap.NewNop())
	if err := m.LoadDHCPAllowlist(filepath.Join(t.TempDir(), "camera-dhcp-interfaces")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetDHCPEnabled("ipv6cam0", true); !errors.Is(err, ErrDHCPInterfaceBlocked) {
		t.Fatalf("IPv6-configured interface accepted camera DHCP: %v", err)
	}
}

func newDHCPConflictLink(t *testing.T) *dhcpConflictLink {
	t.Helper()
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires isolated test network namespace")
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	ip("link", "add", "conflictcam0", "type", "veth", "peer", "name", "conflictpeer0")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "conflictcam0").Run() })
	ip("link", "set", "conflictcam0", "up")
	ip("link", "set", "conflictpeer0", "up")
	ip("addr", "add", "192.0.2.2/24", "dev", "conflictpeer0")
	m := NewLinkManager(nil, zap.NewNop())
	if err := m.LoadDHCPAllowlist(filepath.Join(t.TempDir(), "camera-dhcp-interfaces")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetDHCPEnabled("conflictcam0", true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		// Finish address cleanup before the next test reuses these link names.
		// Work already waiting for scanMu will see the canceled context.
		m.scanMu.Lock()
		m.release("conflictcam0")
		m.scanMu.Unlock()
	})
	m.scanOnce(ctx) // Start the real incoming-packet watcher too.
	claimLinuxTestPort(t, m, ctx, "conflictcam0")
	m.mu.Lock()
	seg, pool := m.claimed["conflictcam0"], m.pools["conflictcam0"]
	m.mu.Unlock()
	return &dhcpConflictLink{m: m, client: leaseTestClient(t, "conflictpeer0"), seg: seg, pool: pool}
}

func (f *dhcpConflictLink) offer(t *testing.T, mac byte) *Packet {
	t.Helper()
	f.xid++
	wire := BuildDiscover(f.xid, net.HardwareAddr{2, 0, 0, 0, 0, mac}, "camera")
	for range 8 {
		if _, err := f.client.WriteToUDP(wire, &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpServerPort}); err != nil {
			t.Fatal(err)
		}
		if err := f.client.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1500)
		n, _, err := f.client.ReadFromUDP(buf)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			t.Fatal(err)
		}
		p, err := ParsePacket(buf[:n])
		if err == nil && p.XID == f.xid && p.Type == Offer {
			return p
		}
	}
	t.Fatal("no DHCP offer")
	return nil
}

func TestDHCPLinuxCompetingServerWithSameAddressStopsServing(t *testing.T) {
	f := newDHCPConflictLink(t)
	f.offer(t, 1)
	f.offer(t, 1) // Outgoing replies must not disqualify our own server.
	if f.m.guard.State("conflictcam0") != LinkServing {
		t.Fatal("own outgoing replies disqualified the camera port")
	}
	foreign, err := bindUDPToLink("conflictpeer0")
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	wire := BuildReply(&Packet{XID: 999, CHAddr: net.HardwareAddr{2, 0, 0, 0, 0, 2}}, Offer,
		ReplyConfig{ServerIP: f.seg.ServerIP, ClientIP: f.seg.PoolBase, Mask: f.seg.Mask, Lease: LeaseWindow})
	waitFor(t, "competing server with our address", func() bool {
		if f.m.guard.State("conflictcam0") == LinkDisqualified {
			return true
		}
		// The watcher starts asynchronously. Retransmit until it is listening.
		if _, err := foreign.WriteToUDP(wire, &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpClientPort}); err != nil {
			t.Fatal(err)
		}
		return false
	})
	waitFor(t, "competing server stops camera DHCP", func() bool {
		_, claimed := f.m.claimedSegment("conflictcam0")
		return !claimed
	})
}

func TestDHCPLinuxDeclinedAddressIsNotOfferedAgain(t *testing.T) {
	f := newDHCPConflictLink(t)
	first := f.offer(t, 1)
	wire := BuildDiscover(999, first.CHAddr, "camera")
	wire[dhcpMinLen+2] = byte(Decline)
	wire = append(wire[:len(wire)-1], optRequestedIP, 4)
	wire = append(wire, first.YIAddr.To4()...)
	wire = append(wire, optServerID, 4)
	wire = append(wire, f.seg.ServerIP.To4()...)
	wire = append(wire, optEnd)
	if _, err := f.client.WriteToUDP(wire, &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpServerPort}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "decline releases the client binding", func() bool { _, held := f.pool.Holder(first.YIAddr); return !held })
	second := f.offer(t, 2)
	if second.YIAddr.Equal(first.YIAddr) {
		t.Fatalf("offered declined/conflicting address again: %s", second.YIAddr)
	}
	path := filepath.Join(filepath.Dir(f.m.allowlistPath), "camera-dhcp-leases-"+f.seg.ServerIP.String()+".json")
	reloaded, err := openLeasePool(path, f.seg, nil)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := reloaded.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 3}, first.YIAddr)
	if err != nil || afterRestart.Equal(first.YIAddr) {
		t.Fatalf("declined address not reserved across restart: %s, %v", afterRestart, err)
	}
}
