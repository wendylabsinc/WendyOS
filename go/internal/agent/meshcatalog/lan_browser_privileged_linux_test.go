//go:build linux

package meshcatalog

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"golang.org/x/net/ipv4"
)

func TestLANBrowserCoexistsWithAvahiAndCarrierMDNS(t *testing.T) {
	if os.Getenv("WENDY_LAN_PRIVILEGED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux network namespace")
	}
	suffix := fmt.Sprint(os.Getpid() % 100000)
	peer, hostIF, peerIF := "mb"+suffix, "bh"+suffix, "bn"+suffix
	run := func(program string, args ...string) {
		t.Helper()
		if out, err := exec.Command(program, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v: %s", program, args, err, out)
		}
	}
	defer func() {
		_ = exec.Command("ip", "netns", "del", peer).Run()
		_ = exec.Command("ip", "link", "del", hostIF).Run()
	}()
	run("ip", "netns", "add", peer)
	run("ip", "link", "add", hostIF, "type", "veth", "peer", "name", peerIF)
	run("ip", "link", "set", peerIF, "netns", peer)
	run("ip", "addr", "add", "192.168.41.1/24", "dev", hostIF)
	run("ip", "link", "set", hostIF, "up")
	run("ip", "netns", "exec", peer, "ip", "addr", "add", "192.168.41.22/24", "dev", peerIF)
	run("ip", "netns", "exec", peer, "ip", "link", "set", peerIF, "up")
	run("mkdir", "-p", "/run/dbus")
	run("dbus-daemon", "--system", "--fork")
	run("avahi-daemon", "--no-chroot", "--daemonize")
	defer func() { _ = exec.Command("avahi-daemon", "--kill").Run() }()
	iface, err := net.InterfaceByName(hostIF)
	if err != nil {
		t.Fatal(err)
	}
	_, subnet, _ := net.ParseCIDR("192.168.41.0/24")
	selected := localmesh.PhysicalLANInterface{Name: hostIF, Index: iface.Index, IP: net.ParseIP("192.168.41.1"), Net: subnet}
	// The carrier responder also binds 5353 with REUSEADDR/REUSEPORT and
	// SO_BINDTODEVICE. Hold that same socket alongside Avahi and the browser.
	carrier, err := (&net.ListenConfig{Control: lanSocketControl(hostIF)}).ListenPacket(context.Background(), "udp4", "0.0.0.0:5353")
	if err != nil {
		t.Fatal(err)
	}
	defer carrier.Close()
	carrierMulticast := ipv4.NewPacketConn(carrier)
	if err := carrierMulticast.JoinGroup(iface, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)}); err != nil {
		t.Fatal(err)
	}
	defer carrierMulticast.LeaveGroup(iface, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)})
	full := announcement("Camera", 60, 8080, "192.168.41.22")
	full.Id = 0
	serviceType := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: []dns.RR{
		&dns.PTR{Hdr: dns.RR_Header{Name: "_services._dns-sd._udp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60}, Ptr: "_http._tcp.local."},
	}}
	fullWire, _ := full.Pack()
	typeWire, _ := serviceType.Pack()
	responder := `import base64,socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM,socket.IPPROTO_UDP)
s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEPORT,1)
s.bind(('0.0.0.0',5353))
s.setsockopt(socket.IPPROTO_IP,socket.IP_ADD_MEMBERSHIP,socket.inet_aton('224.0.0.251')+socket.inet_aton('192.168.41.22'))
packets=[base64.b64decode(x) for x in sys.argv[1:]]
while True:
  data,peer=s.recvfrom(9000)
  for packet in packets: s.sendto(packet,peer)
`
	child := exec.Command("ip", "netns", "exec", peer, "python3", "-u", "-c", responder,
		base64.StdEncoding.EncodeToString(typeWire), base64.StdEncoding.EncodeToString(fullWire))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	browser := NewLANBrowser(true, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- browser.runInterface(ctx, selected) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(12 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if got := browser.cache.Snapshot([]localmesh.PhysicalLANInterface{selected}, time.Now()); len(got) == 1 && got[0].Port == 8080 {
			found = true // service existed before browser startup
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !found {
		t.Fatal("browser did not discover a service already present before startup")
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	goodbye := full.Copy()
	for _, rr := range append(goodbye.Answer, goodbye.Extra...) {
		rr.Header().Ttl = 0
	}
	goodbyeWire, _ := goodbye.Pack()
	sender := `import base64,socket,sys,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM,socket.IPPROTO_UDP)
s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(('192.168.41.22',5353))
s.setsockopt(socket.IPPROTO_IP,socket.IP_MULTICAST_IF,socket.inet_aton('192.168.41.22'))
packet=base64.b64decode(sys.argv[1])
for _ in range(3):
  s.sendto(packet,('224.0.0.251',5353)); time.sleep(0.1)
`
	run("ip", "netns", "exec", peer, "python3", "-c", sender, base64.StdEncoding.EncodeToString(goodbyeWire))
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := browser.cache.Snapshot([]localmesh.PhysicalLANInterface{selected}, time.Now()); len(got) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("physical LAN goodbye did not withdraw browser cache")
}
