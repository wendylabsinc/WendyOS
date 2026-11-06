//go:build linux

package meshsession

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

// A destination app replies to the proxy's bridge address, not the VIP route.
// Exercise deliberate fragmentation in both directions at two real local MTUs.
func TestLocalUDPDestinationContainerFragmentation(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires disposable privileged network namespace")
	}
	for _, mtu := range []int{1500, 576} {
		t.Run(strconv.Itoa(mtu), func(t *testing.T) {
			ns := fmt.Sprintf("wendy-dst-%d", os.Getpid())
			run := func(args ...string) {
				t.Helper()
				out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %v: %s", args, err, out)
				}
			}
			run("ip", "netns", "add", ns)
			t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
			run("ip", "link", "add", "wudpd-h", "type", "veth", "peer", "name", "wudpd-a")
			t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "wudpd-h").Run() })
			run("ip", "link", "set", "wudpd-a", "netns", ns)
			run("ip", "addr", "add", "10.79.43.1/24", "dev", "wudpd-h")
			run("ip", "link", "set", "wudpd-h", "mtu", strconv.Itoa(mtu), "up")
			run("ip", "netns", "exec", ns, "ip", "addr", "add", "10.79.43.2/24", "dev", "wudpd-a")
			run("ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up")
			run("ip", "netns", "exec", ns, "ip", "link", "set", "wudpd-a", "mtu", strconv.Itoa(mtu), "up")
			const script = `import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
s.setsockopt(socket.IPPROTO_IP,10,0) # fixture app IP_MTU_DISCOVER=DONT
s.bind(('10.79.43.2',47778))
print('READY',flush=True)
while True:
    data,addr=s.recvfrom(65535)
    assert addr[0]=='10.79.43.1',addr
    s.sendto(data,addr)
`
			cmd := exec.Command("ip", "netns", "exec", ns, "python3", "-u", "-c", script)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			scan := bufio.NewScanner(out)
			ready := make(chan bool, 1)
			go func() { ready <- scan.Scan() && scan.Text() == "READY" }()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("app not ready")
				}
			case <-time.After(time.Second):
				t.Fatal("app readiness timeout")
			}
			r := meshingress.NewRegistry()
			if err := r.ClaimUDPForApp("destination", "app", 7777, "10.79.43.2", 47778); err != nil {
				t.Fatal(err)
			}
			flow, err := DialLocalUDP(context.Background(), r, 7777)
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a, b := fixtureCredentials(t)
			address := lifecycleServer(t, b, r, true)
			remote, err := DialUDP(ctx, a, b.Device, address, 7777)
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			type messageFlow interface {
				Send([]byte) error
				Receive(context.Context) (*udpmessage.Message, error)
			}
			for _, path := range []struct {
				name string
				flow messageFlow
			}{{"self", flow}, {"QUIC", remote}} {
				for _, size := range []int{0, 1000, 1001, 1472, 8192, MaxUDPMessageSize} {
					p := make([]byte, size)
					for i := range p {
						p[i] = byte(i % 251)
					}
					if err := path.flow.Send(p); err != nil {
						t.Fatal(err)
					}
					got, err := readUDPMessage(path.flow, ctx)
					if err != nil || !bytes.Equal(got, p) {
						t.Fatalf("%s MTU%d size%d got%d: %v", path.name, mtu, size, len(got), err)
					}
				}
			}

			if got := flow.conn.RemoteAddr().String(); got != netip.MustParseAddrPort("10.79.43.2:47778").String() {
				t.Fatal(got)
			}
		})
	}
}
