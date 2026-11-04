package mesh

import (
	"context"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"io"
	"net"
	"net/netip"
	"testing"
)

type legacyDirectory struct{ org int32 }

func (d *legacyDirectory) LegacyOrgID() int32 { return d.org }
func (d *legacyDirectory) ResolveLegacy(name, slug string) (int32, bool) {
	return 674, name == "pi" && slug == "lab"
}
func TestLegacyVIPAndDNSIsolation(t *testing.T) {
	for _, id := range []int32{1, 255, 256, 674, 65534} {
		vip, err := LegacyVIPForDevice(id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := LegacyDeviceForVIP(vip)
		if err != nil || got != id {
			t.Fatalf("roundtrip %d: %d %v", id, got, err)
		}
	}
	for _, id := range []int32{-1, 0, 65535, 2147483647} {
		if _, err := LegacyVIPForDevice(id); err == nil {
			t.Fatalf("accepted %d", id)
		}
	}
	for _, ip := range []string{"10.99.0.0", "10.99.255.255", "10.98.2.162", "::ffff:10.99.2.162"} {
		if _, err := LegacyDeviceForVIP(netip.MustParseAddr(ip)); err == nil {
			t.Fatalf("accepted %s", ip)
		}
	}
	server, addr, _ := startTestDNS(t, "")
	directory := &legacyDirectory{64}
	server.SetLegacyResolver(directory)
	for _, name := range []string{"device-674.mesh.wendy.internal.", "device-674.cloud.wendy.dev.", "pi.lab.mesh.wendy.internal."} {
		wantSingleA(t, query(t, addr, name, dns.TypeA), name, "10.99.2.162")
	}
	for _, name := range []string{"device-0.mesh.wendy.internal.", "device-65535.mesh.wendy.internal.", "device-2147483648.mesh.wendy.internal.", "pi.other.mesh.wendy.internal.", "device-" + deviceUUID + ".mesh.wendy.internal."} {
		if got := query(t, addr, name, dns.TypeA); got.Rcode != dns.RcodeNameError {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	server.SetLegacyResolver(nil)
	if got := query(t, addr, "device-674.mesh.wendy.internal.", dns.TypeA); got.Rcode != dns.RcodeNameError {
		t.Fatal("v2 answered numeric name")
	}
	singleA(t, query(t, addr, "device-"+deviceUUID+".mesh.wendy.internal.", dns.TypeA), "v2")
}

type legacyAppProbe struct{ called chan struct{} }

func (d legacyAppProbe) LegacyOrgID() int32 { return 64 }
func (d legacyAppProbe) DialLegacyApp(_ context.Context, org, asset int32, port uint16) (net.Conn, string, error) {
	if org != 64 || asset != 674 || port != 28091 {
		panic("wrong numeric target")
	}
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(b, payload); err == nil {
			b.Write(payload)
		}
		close(d.called)
	}()
	return a, "relay", nil
}
func TestLegacyProxyDoesNotEnterUUIDDialer(t *testing.T) {
	probe := legacyAppProbe{make(chan struct{})}
	proxy := NewProxy(zap.NewNop(), nil, nil, nil)
	proxy.SetLegacyDialer(probe)
	proxy.origDst = func(*net.TCPConn) (netip.AddrPort, error) { return netip.MustParseAddrPort("10.99.2.162:28091"), nil }
	if err := proxy.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	payload := make([]byte, 4)
	if _, err := io.ReadFull(conn, payload); err != nil || string(payload) != "ping" {
		t.Fatalf("proxy echo=%q %v", payload, err)
	}
	<-probe.called
}
