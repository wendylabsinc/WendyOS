//go:build linux

package meshsharing

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestInternetTunnelProcess(t *testing.T) {
	principal := os.Getenv("WENDY_TUNNEL_SELF")
	if principal == "" {
		t.Skip("namespace subprocess")
	}
	self, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := meshid.DeviceFromPrincipal(os.Getenv("WENDY_TUNNEL_PEER"))
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := newInternetTunnel("wlmitest", self, peer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tunnel.close(); err != nil {
			t.Errorf("close Internet tunnel: %v", err)
		}
	})
	lease := tunnelLease{Slot: 17, IPv6: true}
	if os.Getenv("WENDY_TUNNEL_CLIENT") == "1" {
		for _, address := range []netip.Addr{lease.address4(), lease.address6()} {
			if err := tunnel.assign(address); err != nil {
				t.Fatal(err)
			}
			if err := tunnel.route(address, true); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		for _, address := range []netip.Addr{lease.address4(), lease.address6()} {
			if err := tunnel.route(address, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(os.Getenv("WENDY_TUNNEL_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// This is an IPv6-only, two-hop underlay at the mesh MTU. The inner
// IPv6 ping reaches 1280 bytes, leaving 40 bytes for the IPv6 outer header.
// No IPv4 routes or addresses are installed on either transit link.
func TestInternetTunnelDualStackAcrossIPv6Transit(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("requires disposable privileged Linux with ip6tnl, ip and ping")
	}
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			for _, suffix := range []string{"c", "t", "g"} {
				ns := fmt.Sprintf("wmi%d%s", os.Getpid(), suffix)
				for _, cmd := range [][]string{{"-n", ns, "-s", "link"}, {"-n", ns, "-6", "route"}, {"-n", ns, "-4", "route"}} {
					out, _ := exec.Command("ip", cmd...).CombinedOutput()
					t.Logf("%v: %s", cmd, out)
				}
			}
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	base := fmt.Sprintf("wmi%d", os.Getpid())
	client, transit, gateway := base+"c", base+"t", base+"g"
	for _, name := range []string{client, transit, gateway} {
		run("netns", "add", name)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", name).Run() })
		run("-n", name, "link", "set", "lo", "up")
	}
	for i, endpoint := range []string{client, gateway} {
		a, b := fmt.Sprintf("m%d", i), fmt.Sprintf("t%d", i)
		run("-n", endpoint, "link", "add", a, "type", "veth", "peer", "name", b, "netns", transit)
		for _, pair := range [][2]string{{endpoint, a}, {transit, b}} {
			run("-n", pair[0], "link", "set", pair[1], "mtu", "1320", "up")
		}
		run("-n", endpoint, "-6", "addr", "add", fmt.Sprintf("fd00:%d::1/64", i+1), "dev", a, "nodad")
		run("-n", transit, "-6", "addr", "add", fmt.Sprintf("fd00:%d::2/64", i+1), "dev", b, "nodad")
	}
	self, peer := policySelf(t), mustPolicyDevice(t, policyTenant, policyDonor)
	for _, endpoint := range []struct {
		ns, dev, via string
		device       meshid.Device
	}{
		{client, "m0", "fd00:1::2", self}, {gateway, "m1", "fd00:2::2", peer},
	} {
		run("-n", endpoint.ns, "-6", "addr", "add", endpoint.device.MeshAddress().String()+"/128", "dev", "lo", "nodad")
		run("-n", endpoint.ns, "-6", "route", "add", "fd45:b891:567f::/48", "via", endpoint.via, "dev", endpoint.dev)
	}
	run("-n", transit, "-6", "route", "add", self.MeshAddress().String()+"/128", "via", "fd00:1::1", "dev", "t0")
	run("-n", transit, "-6", "route", "add", peer.MeshAddress().String()+"/128", "via", "fd00:2::1", "dev", "t1")
	run("netns", "exec", transit, "sysctl", "-w", "net.ipv6.conf.all.forwarding=1")
	run("-n", gateway, "addr", "add", "198.51.100.7/32", "dev", "lo")
	run("-n", gateway, "-6", "addr", "add", "2001:db8::7/128", "dev", "lo", "nodad")
	time.Sleep(1500 * time.Millisecond) // let automatically assigned link-local addresses finish DAD
	run("netns", "exec", client, "ping", "-n", "-6", "-I", self.MeshAddress().String(), "-c", "1", "-W", "3", peer.MeshAddress().String())
	for i, endpoint := range []struct {
		ns         string
		self, peer meshid.Device
	}{{gateway, peer, self}, {client, self, peer}} {
		ready := filepath.Join(t.TempDir(), "ready")
		cmd := exec.Command("ip", "netns", "exec", endpoint.ns, os.Args[0], "-test.run=^TestInternetTunnelProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_TUNNEL_SELF="+endpoint.self.Principal(), "WENDY_TUNNEL_PEER="+endpoint.peer.Principal(), "WENDY_TUNNEL_READY="+ready, fmt.Sprintf("WENDY_TUNNEL_CLIENT=%d", i))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("tunnel subprocess: %v", err)
			}
		})
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("tunnel did not become ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	run("netns", "exec", client, "ping", "-n", "-4", "-c", "1", "-W", "2", "-s", "1200", "198.51.100.7")
	run("netns", "exec", client, "ping", "-n", "-6", "-c", "1", "-W", "2", "-s", "1232", "2001:db8::7")
}
