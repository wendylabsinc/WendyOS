//go:build linux

package meshingress

import (
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestAdmittedEndpointSurvivesDNATLoss(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("requires disposable privileged network namespace")
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestAdmittedEndpointProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "WENDY_INGRESS_ENDPOINT_PROCESS=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("endpoint namespace: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

func TestAdmittedEndpointProcess(t *testing.T) {
	if os.Getenv("WENDY_INGRESS_ENDPOINT_PROCESS") != "1" {
		t.Skip("namespace subprocess")
	}
	run := func(binary string, args ...string) {
		t.Helper()
		if out, err := exec.Command(binary, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v %s", binary, args, err, out)
		}
	}
	run("ip", "link", "set", "lo", "up")
	run("ip", "address", "add", "10.201.150.2/32", "dev", "lo")
	var hostDials atomic.Int32
	serve := func(address, reply string) net.Listener {
		t.Helper()
		l, err := net.Listen("tcp4", address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, e := l.Accept()
				if e != nil {
					return
				}
				if reply == "host" {
					hostDials.Add(1)
				}
				_, _ = io.WriteString(c, reply)
				c.Close()
			}
		}()
		return l
	}
	app := serve("10.201.150.2:0", "app")
	host := serve("127.0.0.1:0", "host")
	hostPort := uint16(host.Addr().(*net.TCPAddr).Port)
	appPort := uint16(app.Addr().(*net.TCPAddr).Port)
	rule := []string{"-d", "127.0.0.1/32", "-p", "tcp", "--dport", strconv.Itoa(int(hostPort)), "-j", "DNAT", "--to-destination", app.Addr().String()}
	run("iptables", append([]string{"-t", "nat", "-A", "OUTPUT"}, rule...)...)
	r := NewRegistry()
	if err := r.Claim("running-app", hostPort, "10.201.150.2", appPort); err != nil {
		t.Fatal(err)
	}
	dial := func() (net.Conn, error) {
		return r.DialAuthorized(hostPort, func(address string) (net.Conn, error) { return net.DialTimeout("tcp4", address, time.Second) })
	}
	for _, remove := range []bool{false, true} {
		if remove {
			run("iptables", append([]string{"-t", "nat", "-D", "OUTPUT"}, rule...)...)
		}
		c, err := dial()
		if err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(time.Second))
		body, err := io.ReadAll(c)
		c.Close()
		if err != nil || string(body) != "app" {
			t.Fatalf("removed=%v: got %q %v", remove, body, err)
		}
	}
	app.Close()
	if c, err := dial(); err == nil {
		c.Close()
		t.Fatal("missing container endpoint fell back to host listener")
	}
	if hostDials.Load() != 0 {
		t.Fatalf("unrelated host listener received %d dials", hostDials.Load())
	}
	t.Log("live app reached with and without DNAT; unavailable app failed closed while unrelated host listener stayed live")
	testUDPEndpointWithoutDNAT(t, run)
}

func testUDPEndpointWithoutDNAT(t *testing.T, run func(string, ...string)) {
	t.Helper()
	var hostDials atomic.Int32
	serve := func(ip, reply string) *net.UDPConn {
		l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			buf := make([]byte, 32)
			for {
				_, from, err := l.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if reply == "host" {
					hostDials.Add(1)
				}
				_, _ = l.WriteToUDP([]byte(reply), from)
			}
		}()
		return l
	}
	app, host := serve("10.201.150.2", "app"), serve("127.0.0.1", "host")
	appPort := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	hostPort := uint16(host.LocalAddr().(*net.UDPAddr).Port)
	rule := []string{"-d", "127.0.0.1/32", "-p", "udp", "--dport", strconv.Itoa(int(hostPort)), "-j", "DNAT", "--to-destination", app.LocalAddr().String()}
	run("iptables", append([]string{"-t", "nat", "-A", "OUTPUT"}, rule...)...)
	r := NewRegistry()
	if err := r.ClaimUDPForApp("udp-app", "udp-app", hostPort, "10.201.150.2", appPort); err != nil {
		t.Fatal(err)
	}
	token := r.UDPToken(hostPort)
	exchange := func() error {
		var conn *net.UDPConn
		if err := r.WithAuthorizedUDPToken(hostPort, token, func(address netip.AddrPort) error {
			var err error
			conn, err = net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(address))
			return err
		}); err != nil {
			return err
		}
		defer conn.Close()
		if err := r.WithAuthorizedUDPToken(hostPort, token, func(netip.AddrPort) error { _, err := conn.Write([]byte("probe")); return err }); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 16)
		n, err := conn.Read(buf)
		if err == nil && string(buf[:n]) != "app" {
			t.Fatalf("UDP reached %q", buf[:n])
		}
		return err
	}
	for _, remove := range []bool{false, true} {
		if remove {
			run("iptables", append([]string{"-t", "nat", "-D", "OUTPUT"}, rule...)...)
		}
		if err := exchange(); err != nil {
			t.Fatalf("UDP removed=%v: %v", remove, err)
		}
	}
	app.Close()
	if err := exchange(); err == nil {
		t.Fatal("missing UDP container fell back to host listener")
	}
	if hostDials.Load() != 0 {
		t.Fatal("UDP reached unrelated host listener")
	}
	t.Log("UDP uses admitted container endpoint with and without DNAT, and fails closed after app loss")
}
