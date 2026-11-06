//go:build linux

package mesh

import (
	"context"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type echoUDPFlow struct {
	replies chan []byte
	t       *testing.T
	onSend  func([]byte)
}

func (f *echoUDPFlow) Send(data []byte) error {
	if f.t != nil {
		f.t.Logf("QUIC message send bytes=%d", len(data))
	}
	if f.onSend != nil {
		f.onSend(data)
	}
	f.replies <- append([]byte(nil), data...)
	return nil
}
func (f *echoUDPFlow) Receive(ctx context.Context) (*udpmessage.Message, error) {
	select {
	case data := <-f.replies:
		if f.t != nil {
			f.t.Logf("QUIC message reply bytes=%d", len(data))
		}
		return &udpmessage.Message{Payload: data}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *echoUDPFlow) Close() error { return nil }

type namespaceSource struct {
	ip, dst netip.Addr
	ifindex int
	t       *testing.T
	token   *atomic.Uint64
}

func (s namespaceSource) SourceToken(ip, dst netip.Addr, ifindex int) uint64 {
	if s.t != nil {
		s.t.Logf("redirected UDP source=%s destination=%s ifindex=%d expected=%d", ip, dst, ifindex, s.ifindex)
	}
	if ip == s.ip && dst == s.dst && ifindex == s.ifindex {
		if s.token != nil {
			return s.token.Load()
		}
		return 1
	}
	return 0
}

func (s namespaceSource) SourceDone(netip.Addr, netip.Addr, int, uint64) <-chan struct{} { return nil }

func (s namespaceSource) WithSourceToken(ip, dst netip.Addr, ifindex int, token uint64, send func() error) error {
	if token == 0 || s.SourceToken(ip, dst, ifindex) != token {
		return fmt.Errorf("source denied")
	}
	return send()
}

// This privileged test exercises a real bridged app netns, TPROXY, original
// destination, ingress ifindex, and transparent VIP reply path. Run in a
// disposable Docker network namespace with WENDY_TEST_NETNS=1.
func TestUDPProxyRedirectNamespace(t *testing.T) {
	for _, port := range []uint16{7777, UDPProxyPort, 47777} {
		t.Run(strconv.Itoa(int(port)), func(t *testing.T) { testUDPProxyRedirectNamespace(t, port) })
	}
}

func TestUDPProxyFullCeilingMTU576Namespace(t *testing.T) {
	testUDPProxyRedirectNamespace(t, 7777, 576)
}

func testUDPProxyRedirectNamespace(t *testing.T, targetPort uint16, localMTU ...int) {
	mtu := 1500
	if len(localMTU) > 0 {
		mtu = localMTU[0]
	}
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires privileged disposable network namespace")
	}
	for _, tool := range []string{"ip", "iptables", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing %s: %v", tool, err)
		}
	}
	ns := fmt.Sprintf("wendy-udp-%d", os.Getpid())
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("ip", "netns", "add", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
	run("ip", "link", "add", "wudp-br", "type", "bridge")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "wudp-br").Run() })
	run("ip", "link", "add", "wudp-h", "type", "veth", "peer", "name", "wudp-a")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "wudp-h").Run() })
	run("ip", "link", "set", "wudp-a", "netns", ns)
	run("ip", "link", "set", "wudp-h", "master", "wudp-br")
	run("ip", "addr", "add", "10.79.42.1/24", "dev", "wudp-br")
	run("ip", "link", "set", "wudp-br", "mtu", strconv.Itoa(mtu), "up")
	run("ip", "link", "set", "wudp-h", "mtu", strconv.Itoa(mtu), "up")
	run("ip", "netns", "exec", ns, "ip", "addr", "add", "10.79.42.2/24", "dev", "wudp-a")
	run("ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up")
	run("ip", "netns", "exec", ns, "ip", "link", "set", "wudp-a", "mtu", strconv.Itoa(mtu), "up")
	run("ip", "netns", "exec", ns, "ip", "route", "add", "10.99.0.0/16", "via", "10.79.42.1")
	run("ip", "netns", "exec", ns, "ping", "-c", "1", "-W", "1", "10.79.42.1")
	iface, err := net.InterfaceByName("wudp-br")
	if err != nil {
		t.Fatal(err)
	}
	vips, err := OpenVIPTable(filepath.Join(t.TempDir(), "vips.json"))
	if err != nil {
		t.Fatal(err)
	}
	device, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/00000000-0000-4000-8000-000000000534")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := vips.Allocate(device)
	if err != nil {
		t.Fatal(err)
	}
	if dst.String() != "10.99.0.1" {
		t.Fatalf("unexpected fixture VIP: %s", dst)
	}
	var sourceToken atomic.Uint64
	sourceToken.Store(1)
	p, err := NewUDPProxy(UDPDialFunc(func(_ context.Context, request UDPDialRequest) (UDPFlow, error) {
		peer, port := request.Peer, request.Port
		t.Logf("UDP dial peer=%s port=%d", peer, port)
		if peer != device || port != targetPort {
			return nil, fmt.Errorf("denied port")
		}
		flow := &echoUDPFlow{replies: make(chan []byte, 32), t: t, onSend: func(data []byte) {
			if string(data) == "revoke-source" {
				sourceToken.Store(0)
			}
		}}
		if err := request.Endpoint.SetTransport(1000, true, flow.Send); err != nil {
			return nil, err
		}
		return flow, nil
	}), namespaceSource{netip.MustParseAddr("10.79.42.2"), dst, iface.Index, t, &sourceToken}, vips)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// This host socket exclusively owns the same numbered port as a remote
	// service. The VIP flow must never bind it or deliver app requests to it.
	var hostListener *net.UDPConn
	if targetPort == 47777 {
		hostListener, err = net.ListenUDP("udp4", &net.UDPAddr{Port: int(targetPort)})
		if err != nil {
			t.Fatal(err)
		}
		defer hostListener.Close()
	}
	if err := p.Start("0.0.0.0:" + strconv.Itoa(UDPProxyPort)); err != nil {
		t.Fatal(err)
	}
	proxyPort := p.Addr().(*net.UDPAddr).Port
	if err := hostnetwork.InitMeshUDPTProxy(proxyPort); err != nil {
		t.Fatal(err)
	}
	if err := hostnetwork.InitMeshUDPTProxy(proxyPort); err != nil {
		t.Fatalf("repeated TPROXY init: %v", err)
	}
	t.Cleanup(func() { _ = hostnetwork.CloseMeshUDPTProxy() })
	if err := hostnetwork.AddMeshUDPIntercept("10.79.42.2", "10.99.0.0/16", "wudp-h", proxyPort); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hostnetwork.RemoveMeshUDPIntercept("10.79.42.2", "10.99.0.0/16", "wudp-h", proxyPort) })
	// A redeploy can allocate a new CNI IP without an orderly old-task
	// teardown. Only the new source may retain a TPROXY rule for this bridge.
	if err := hostnetwork.AddMeshUDPIntercept("10.79.42.3", "10.99.0.0/16", "wudp-h", proxyPort); err != nil {
		t.Fatal(err)
	}
	if err := hostnetwork.AddMeshUDPIntercept("10.79.42.2", "10.99.0.0/16", "wudp-h", proxyPort); err != nil {
		t.Fatal(err)
	}
	if err := hostnetwork.RemoveMeshUDPIntercept("10.79.42.3", "10.99.0.0/16", "wudp-h", proxyPort); err != nil {
		t.Fatal(err)
	}
	stale := exec.Command("iptables", "-t", "mangle", "-C", hostnetwork.MeshUDPChainName,
		"-s", "10.79.42.3/32", "-d", "10.99.0.0/16", "-p", "udp", "-j", "TPROXY",
		"--on-port", strconv.Itoa(proxyPort), "--tproxy-mark", "0x40000000/0x40000000")
	if err := stale.Run(); err == nil {
		t.Fatal("redeploy retained stale app UDP interception")
	}
	// Carrier and network reconfiguration can remove the marked policy rule
	// after interception is already installed. Repair must preserve that app
	// rule and restore the marked route without restarting its proxy.
	run("ip", "-4", "rule", "del", "pref", "18864", "fwmark", "0x40000000/0x40000000", "lookup", "18867")
	if err := hostnetwork.EnsureMeshUDPTProxyPolicy(); err != nil {
		t.Fatalf("repair UDP policy after rule loss: %v", err)
	}
	const script = `import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
s.settimeout(3)
s.sendto(b'echo',('10.99.0.1',7777))
data,addr=s.recvfrom(1024)
assert data==b'echo' and addr==('10.99.0.1',7777),(data,addr)
print('VIP_REPLY_OK',flush=True)
for index in range(1, 4):
    payload=('repeat-%d'%index).encode()
    s.sendto(payload,('10.99.0.1',7777))
    data,addr=s.recvfrom(1024)
    assert data==payload and addr==('10.99.0.1',7777),(index,data,addr)
print('VIP_REPEAT_REPLY_OK',flush=True)
# Both flows receive replies from the same VIP:port. Keep two different app
# source tuples active and verify that responses cannot cross between them.
other=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
other.bind(('0.0.0.0',50059))
other.settimeout(3)
for index in range(3):
    s.sendto(('first-%d'%index).encode(),('10.99.0.1',7777))
    other.sendto(('other-%d'%index).encode(),('10.99.0.1',7777))
    a,aa=s.recvfrom(1024)
    b,ba=other.recvfrom(1024)
    assert a==('first-%d'%index).encode() and aa==('10.99.0.1',7777),(a,aa)
    assert b==('other-%d'%index).encode() and ba==('10.99.0.1',7777),(b,ba)
print('VIP_TUPLE_ISOLATION_OK',flush=True)
# Verify both send and reply paths preserve every byte at the payload bound,
# including odd lengths. Packet vectors cover checksum-zero encoding.
for client in (s,other):
    # The reduced-MTU fixture deliberately permits application fragmentation.
    # The ordinary MTU1500 test still sends1472 with default socket policy.
    if LOCAL_MTU < 1500: client.setsockopt(socket.IPPROTO_IP,10,0)
    for payload in (b'', b'x', b'hello', b'\x87'*1000, b'\x87'*1001, b'\x87'*1472):
        client.sendto(payload,('10.99.0.1',7777))
        data,addr=client.recvfrom(65535)
        assert data==payload and addr==('10.99.0.1',7777),(len(payload),len(data),addr)
# Full-ceiling application messages require the fixture application's own
# deliberate fragmentation policy. The preceding ordinary 1472 test leaves
# default socket policy unchanged; no interface MTU is lowered.
    client.setsockopt(socket.IPPROTO_IP,10,0) # IP_MTU_DISCOVER=DONT
    for size in (8192,65507):
        payload=bytes((i%251 for i in range(size)))
        client.sendto(payload,('10.99.0.1',7777))
        data,addr=client.recvfrom(65535)
        assert data==payload and addr==('10.99.0.1',7777),(size,len(data),addr)
print('VIP_MAX_PAYLOAD_BOTH_DIRECTIONS_OK',flush=True)
# For the 7777 case this tuple and payload calculate to UDP checksum zero.
# The checked 0xffff wire representation must still reach a real UDP socket.
zero=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
zero.bind(('0.0.0.0',41000))
zero.settimeout(3)
zero.sendto(b'\x02\x9c',('10.99.0.1',7777))
data,addr=zero.recvfrom(1024)
assert data==b'\x02\x9c' and addr==('10.99.0.1',7777),(data,addr)
zero.close()
print('VIP_ZERO_CHECKSUM_REPLY_OK',flush=True)
s.settimeout(0.4)
s.sendto(b'denied',('10.99.0.1',7778))
try:
    data,addr=s.recvfrom(1024)
    raise AssertionError((data,addr))
except socket.timeout: print('DENIED_PORT_OK',flush=True)
# Revoke the admitted source while its first flow is still active.
# Its pending reply, later packets, and a new tuple must all stop.
s.sendto(b'revoke-source',('10.99.0.1',7777))
try:
    raise AssertionError(s.recvfrom(1024))
except socket.timeout: pass
for client in (s,other,socket.socket(socket.AF_INET,socket.SOCK_DGRAM)):
    client.settimeout(0.4)
    client.sendto(b'after-revoke',('10.99.0.1',7777))
    try:
        raise AssertionError(client.recvfrom(1024))
    except socket.timeout: pass
print('SOURCE_REVOCATION_OK',flush=True)
`
	cmd := exec.Command("ip", "netns", "exec", ns, "python3", "-c", strings.ReplaceAll(strings.ReplaceAll(script, "7777", strconv.Itoa(int(targetPort))), "LOCAL_MTU", strconv.Itoa(mtu)))
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "VIP_REPLY_OK") || !strings.Contains(string(out), "VIP_REPEAT_REPLY_OK") || !strings.Contains(string(out), "VIP_TUPLE_ISOLATION_OK") || !strings.Contains(string(out), "VIP_MAX_PAYLOAD_BOTH_DIRECTIONS_OK") || !strings.Contains(string(out), "VIP_ZERO_CHECKSUM_REPLY_OK") || !strings.Contains(string(out), "DENIED_PORT_OK") || !strings.Contains(string(out), "SOURCE_REVOCATION_OK") {
		if rules, ruleErr := exec.Command("ip", "-4", "rule", "show").CombinedOutput(); ruleErr == nil {
			t.Logf("policy rules:\n%s", rules)
		}
		if route, routeErr := exec.Command("ip", "-4", "route", "get", "10.99.0.1", "from", "10.79.42.2", "iif", "wudp-br", "mark", "0x40000000").CombinedOutput(); routeErr == nil {
			t.Logf("marked route:\n%s", route)
		} else {
			t.Logf("marked route error: %s", route)
		}
		if counters, counterErr := exec.Command("iptables", "-t", "mangle", "-nvL", "PREROUTING").CombinedOutput(); counterErr == nil {
			t.Logf("PREROUTING counters:\n%s", counters)
		}
		if counters, counterErr := exec.Command("iptables", "-t", "mangle", "-nvL", hostnetwork.MeshUDPChainName).CombinedOutput(); counterErr == nil {
			t.Logf("mangle counters:\n%s", counters)
		}
		t.Fatalf("namespace UDP echo/denial: %v: %s", err, out)
	}
	if hostListener != nil {
		_ = hostListener.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, _, err := hostListener.ReadFromUDP(make([]byte, 1024)); err == nil {
			t.Fatal("remote VIP traffic reached the host's exclusive UDP listener")
		} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
			t.Fatalf("host listener was damaged: %v", err)
		}
	}

	// The raw UDP sender must not retain copies of unrelated local UDP.
	unrelated, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := net.DialUDP("udp4", nil, unrelated.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	_, err = writer.Write([]byte("unrelated raw receive copy"))
	if err != nil {
		t.Fatal(err)
	}
	_ = unrelated.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = unrelated.ReadFromUDP(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	unrelated.Close()
	var receiveErr error
	if err := p.replies.raw.Control(func(fd uintptr) { _, _, receiveErr = unix.Recvfrom(int(fd), make([]byte, 65535), unix.MSG_DONTWAIT) }); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(receiveErr, unix.EAGAIN) {
		t.Fatalf("send-only raw socket received unrelated UDP: %v", receiveErr)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.replies.Send([]byte("closed"), netip.AddrPortFrom(dst, targetPort), netip.MustParseAddrPort("10.79.42.2:50059"), iface.Index); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("proxy close did not retire its raw reply socket: %v", err)
	}

}
