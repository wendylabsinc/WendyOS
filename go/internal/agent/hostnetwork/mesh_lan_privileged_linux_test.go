//go:build linux

package hostnetwork

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run only inside a disposable privileged Docker container. It proves that
// the app's exact /32 route and NAT make an allowed LAN endpoint reachable,
// while the same destination's other port is denied by the owned DROP.
func TestLANServiceAccessPrivilegedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LAN_PRIVILEGED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux network namespace")
	}
	name := fmt.Sprint(os.Getpid() % 100000)
	app, peer := "ma"+name, "ml"+name
	appHost, appNet := "ah"+name, "an"+name
	lanHost, lanNet := "lh"+name, "ln"+name
	run := func(program string, args ...string) {
		t.Helper()
		if out, err := exec.Command(program, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v: %s", program, args, err, out)
		}
	}
	defer func() {
		_ = exec.Command("ip", "netns", "del", app).Run()
		_ = exec.Command("ip", "netns", "del", peer).Run()
		_ = exec.Command("ip", "link", "del", appHost).Run()
		_ = exec.Command("ip", "link", "del", lanHost).Run()
	}()
	run("ip", "netns", "add", app)
	run("ip", "netns", "add", peer)
	run("ip", "link", "add", appHost, "type", "veth", "peer", "name", appNet)
	run("ip", "link", "set", appNet, "netns", app)
	run("ip", "addr", "add", "10.42.2.1/28", "dev", appHost)
	run("ip", "link", "set", appHost, "up")
	run("ip", "netns", "exec", app, "ip", "addr", "add", "10.42.2.2/28", "dev", appNet)
	run("ip", "netns", "exec", app, "ip", "link", "set", appNet, "up")
	run("ip", "link", "add", lanHost, "type", "veth", "peer", "name", lanNet)
	run("ip", "link", "set", lanNet, "netns", peer)
	run("ip", "addr", "add", "192.168.41.1/24", "dev", lanHost)
	run("ip", "link", "set", lanHost, "up")
	run("ip", "netns", "exec", peer, "ip", "addr", "add", "192.168.41.22/24", "dev", lanNet)
	run("ip", "netns", "exec", peer, "ip", "link", "set", lanNet, "up")
	run("sysctl", "-w", "net.ipv4.ip_forward=1")
	if err := InitMeshChain(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = exec.Command("iptables", "-w", "5", "-D", "FORWARD", "-j", MeshChainName).Run()
		_ = exec.Command("iptables", "-w", "5", "-F", MeshChainName).Run()
		_ = exec.Command("iptables", "-w", "5", "-X", MeshChainName).Run()
	}()
	for _, port := range []string{"8080", "8081"} {
		cmd := exec.Command("ip", "netns", "exec", peer, "python3", "-m", "http.server", port, "--bind", "192.168.41.22")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		client := &http.Client{Timeout: 300 * time.Millisecond}
		if response, err := client.Get("http://192.168.41.22:8080"); err == nil {
			response.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	a := LANServiceAccess{AppBridge: appHost, NetNS: "/run/netns/" + app, AppIP: "10.42.2.2", Gateway: "10.42.2.1",
		Destination: "192.168.41.22", Interface: lanHost, Protocol: "tcp", Port: 8080}
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = WithdrawLANServiceAccess(a); _ = WithdrawLANDestination(a) }()
	probe := func(port string) (string, error) {
		out, err := exec.Command("ip", "netns", "exec", app, "curl", "--max-time", "2", "-fsS",
			"http://192.168.41.22:"+port).CombinedOutput()
		return string(out), err
	}
	if body, err := probe("8080"); err != nil || !strings.Contains(body, "Directory listing") {
		t.Fatalf("allowed LAN TCP failed: %v %s", err, body)
	}
	if body, err := probe("8081"); err == nil {
		t.Fatalf("undeclared LAN port remained reachable: %s", body)
	}
	if err := RevokeOrphanLANServiceAllows(); err != nil {
		t.Fatal(err)
	}
	if body, err := probe("8080"); err == nil {
		t.Fatalf("restart cleanup retained stale grant: %s", body)
	}
	if err := RevokeOrphanLANAppRoutes(a.NetNS, a.AppIP, a.Gateway); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ip", "netns", "exec", app, "ip", "route", "get", a.Destination).CombinedOutput(); err == nil {
		t.Fatalf("inherited app retained stale LAN route: %s", out)
	}
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	if _, err := probe("8080"); err != nil {
		t.Fatalf("recovered LAN access failed: %v", err)
	}
}
