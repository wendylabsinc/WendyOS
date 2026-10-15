//go:build linux

package localmesh

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/babel"
)

func TestKernelIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	k, err := NewKernel(64, 445)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k.Close(); err != nil {
			t.Error(err)
		}
	})
	tun, err := k.AddPeer(1, 460)
	if err != nil {
		t.Fatal(err)
	}
	_, peer, _ := LinkAddresses(445, 460)
	dst := netip.MustParsePrefix("10.88.1.204/32")
	if err = k.Apply([]babel.Route{{Prefix: dst, RouterID: babel.RouterID(64<<32 | 460), Link: 1, NextHop: peer, Metric: 256}}); err != nil {
		t.Fatal(err)
	}
	got, err := netlink.RouteGet(net.ParseIP("10.88.1.204"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LinkIndex != k.peers[1].link.Index {
		t.Fatalf("wrong FIB: %v", got)
	}
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("10.88.1.204"), Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, TunnelMTU)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				done <- nil
				return
			}
			// Bringing up IPv6 link-local may emit a kernel router solicitation
			// before the test's IPv4 packet; it is not routed application data.
			if n > 0 && buf[0]>>4 == 4 {
				done <- buf[:n]
				return
			}
		}
	}()
	if _, err = conn.Write([]byte("local-mesh-kernel-test")); err != nil {
		t.Fatal(err)
	}
	select {
	case packet := <-done:
		if !validIP(packet) || packet[0]>>4 != 4 {
			t.Fatalf("invalid TUN output: %x", packet)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no IP packet delivered through TUN")
	}
	if err = k.Apply([]babel.Route{{Prefix: dst, Unreachable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err = netlink.RouteGet(net.ParseIP("10.88.1.204")); err == nil {
		t.Fatal("unreachable FIB fell through")
	}
	if err = k.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = netlink.RouteGet(net.ParseIP("10.88.1.204")); err == nil {
		t.Fatal("empty table fell through to default")
	}
}
