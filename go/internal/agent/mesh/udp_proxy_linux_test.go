//go:build linux

package mesh

import (
	"context"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

type echoUDPFlow struct {
	replies chan []byte
	t       *testing.T
}

func (f *echoUDPFlow) Send(data []byte) error {
	if f.t != nil {
		f.t.Logf("QUIC datagram send %q", data)
	}
	f.replies <- append([]byte(nil), data...)
	return nil
}
func (f *echoUDPFlow) Receive(ctx context.Context) ([]byte, error) {
	select {
	case data := <-f.replies:
		if f.t != nil {
			f.t.Logf("QUIC datagram reply %q", data)
		}
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *echoUDPFlow) Close() error { return nil }

type namespaceSource struct {
	ip, dst netip.Addr
	ifindex int
	t       *testing.T
}

func (s namespaceSource) SourceToken(ip, dst netip.Addr, ifindex int) uint64 {
	if s.t != nil {
		s.t.Logf("redirected UDP source=%s destination=%s ifindex=%d expected=%d", ip, dst, ifindex, s.ifindex)
	}
	if ip == s.ip && dst == s.dst && ifindex == s.ifindex {
		return 1
	}
	return 0
}

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
	run("ip", "link", "set", "wudp-br", "up")
	run("ip", "link", "set", "wudp-h", "up")
	run("ip", "netns", "exec", ns, "ip", "addr", "add", "10.79.42.2/24", "dev", "wudp-a")
	run("ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up")
	run("ip", "netns", "exec", ns, "ip", "link", "set", "wudp-a", "up")
	run("ip", "netns", "exec", ns, "ip", "route", "add", "10.99.0.0/16", "via", "10.79.42.1")
	run("ip", "netns", "exec", ns, "ping", "-c", "1", "-W", "1", "10.79.42.1")
	iface, err := net.InterfaceByName("wudp-br")
	if err != nil {
		t.Fatal(err)
	}
	dst := netip.MustParseAddr("10.99.2.22")
	p, err := NewUDPProxy(UDPDialFunc(func(_ context.Context, peer int32, port uint16) (UDPFlow, error) {
		t.Logf("UDP dial peer=%d port=%d", peer, port)
		if peer != 534 || port != 7777 {
			return nil, fmt.Errorf("denied port")
		}
		return &echoUDPFlow{replies: make(chan []byte, 32), t: t}, nil
	}), namespaceSource{netip.MustParseAddr("10.79.42.2"), dst, iface.Index, t})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	baseReplyDial := p.replyDial
	p.replyDial = func(src, dst netip.AddrPort) (*net.UDPConn, error) {
		conn, err := baseReplyDial(src, dst)
		t.Logf("reply socket %s -> %s: %v", src, dst, err)
		return conn, err
	}
	if err := p.Start("0.0.0.0:0"); err != nil {
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
s.sendto(b'echo',('10.99.2.22',7777))
data,addr=s.recvfrom(1024)
assert data==b'echo' and addr==('10.99.2.22',7777),(data,addr)
print('VIP_REPLY_OK',flush=True)
s.settimeout(0.4)
s.sendto(b'denied',('10.99.2.22',7778))
try:
    data,addr=s.recvfrom(1024)
    raise AssertionError((data,addr))
except socket.timeout: print('DENIED_PORT_OK',flush=True)
`
	cmd := exec.Command("ip", "netns", "exec", ns, "python3", "-c", script)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "VIP_REPLY_OK") || !strings.Contains(string(out), "DENIED_PORT_OK") {
		if rules, ruleErr := exec.Command("ip", "-4", "rule", "show").CombinedOutput(); ruleErr == nil {
			t.Logf("policy rules:\n%s", rules)
		}
		if route, routeErr := exec.Command("ip", "-4", "route", "get", "10.99.2.22", "from", "10.79.42.2", "iif", "wudp-br", "mark", "0x40000000").CombinedOutput(); routeErr == nil {
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
}
