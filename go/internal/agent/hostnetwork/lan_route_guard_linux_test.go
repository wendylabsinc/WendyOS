//go:build linux

package hostnetwork

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLANGuardEndpoint(t *testing.T) {
	role := os.Getenv("WENDY_LAN_GUARD_ENDPOINT")
	if role == "" {
		t.Skip("namespace subprocess")
	}
	ports := []string{"8080", "8081", "8082"}
	if role == "app" {
		ports = []string{"9000"}
	}
	for _, port := range ports {
		go http.ListenAndServe(":"+port, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, role) }))
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// The observed link remains up throughout. Both foreign FORWARD accepts and
// live backends make route/ingress changes meaningful negative controls.
func TestLANGuardsSurviveHostRouteAndIngressChanges(t *testing.T) {
	if os.Getenv("WENDY_LAN_GUARD_TEST") != "1" {
		t.Skip("requires a disposable privileged network-none container")
	}
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("foreign-before=%t", before), func(t *testing.T) { testLANGuards(t, before) })
	}
}

func testLANGuards(t *testing.T, foreignFirst bool) {
	run := func(program string, args ...string) string {
		t.Helper()
		out, err := exec.Command(program, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v: %s", program, args, err, out)
		}
		return string(out)
	}
	for _, endpoint := range []struct{ name, host, subnet, address string }{
		{"guard-app", "guardah", "10.42.2.1/28", "10.42.2.2/28"},
		{"guard-lan", "guardlh", "192.168.41.1/24", "192.168.41.22/24"},
		{"guard-alt", "guardxh", "192.168.42.1/24", "192.168.42.22/24"},
	} {
		run("ip", "netns", "add", endpoint.name)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", endpoint.name).Run() })
		run("ip", "link", "add", endpoint.host, "type", "veth", "peer", "name", "eth0", "netns", endpoint.name)
		t.Cleanup(func() { _ = exec.Command("ip", "link", "del", endpoint.host).Run() })
		run("ip", "addr", "add", endpoint.subnet, "dev", endpoint.host)
		run("ip", "link", "set", endpoint.host, "up")
		run("ip", "-n", endpoint.name, "addr", "add", endpoint.address, "dev", "eth0")
		run("ip", "-n", endpoint.name, "link", "set", "eth0", "up")
		run("ip", "-n", endpoint.name, "link", "set", "lo", "up")
	}
	// Use a real CNI-style bridge with netfilter enabled: same-app peers
	// must remain reachable while physical unsolicited ingress is denied.
	run("ip", "link", "add", "guardbr", "type", "bridge")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "guardbr").Run() })
	run("ip", "addr", "del", "10.42.2.1/28", "dev", "guardah")
	run("ip", "addr", "add", "10.42.2.1/28", "dev", "guardbr")
	run("ip", "link", "set", "guardah", "master", "guardbr")
	run("ip", "link", "set", "guardbr", "up")
	run("ip", "netns", "add", "guard-sibling")
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", "guard-sibling").Run() })
	run("ip", "link", "add", "guardsibh", "type", "veth", "peer", "name", "eth0", "netns", "guard-sibling")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "guardsibh").Run() })
	run("ip", "link", "set", "guardsibh", "master", "guardbr")
	run("ip", "link", "set", "guardsibh", "up")
	run("ip", "-n", "guard-sibling", "addr", "add", "10.42.2.3/28", "dev", "eth0")
	run("ip", "-n", "guard-sibling", "link", "set", "eth0", "up")
	run("sysctl", "-w", "net.bridge.bridge-nf-call-iptables=1")
	run("ip", "-n", "guard-lan", "route", "add", "10.42.2.0/28", "via", "192.168.41.1")
	run("ip", "-n", "guard-lan", "route", "add", "192.168.42.0/24", "via", "192.168.41.1")
	run("ip", "-n", "guard-lan", "addr", "add", "192.168.41.23/32", "dev", "eth0")
	run("ip", "-n", "guard-alt", "route", "add", "10.42.2.0/28", "via", "192.168.42.1")
	run("ip", "-n", "guard-alt", "route", "add", "192.168.41.0/24", "via", "192.168.42.1")
	run("ip", "-n", "guard-alt", "addr", "add", "192.168.41.22/32", "dev", "lo")
	run("sysctl", "-w", "net.ipv4.ip_forward=1")
	run("iptables", "-P", "FORWARD", "ACCEPT")
	if err := InitMeshChain(); err != nil {
		t.Fatal(err)
	}
	for namespace, role := range map[string]string{"guard-app": "app", "guard-sibling": "app", "guard-lan": "observed", "guard-alt": "alternate"} {
		cmd := exec.Command("ip", "netns", "exec", namespace, os.Args[0], "-test.run=^TestLANGuardEndpoint$")
		cmd.Env = append(os.Environ(), "WENDY_LAN_GUARD_ENDPOINT="+role)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	}
	time.Sleep(300 * time.Millisecond)
	probe := func(namespace, address string, port uint16, source string) (string, error) {
		args := []string{"netns", "exec", namespace, "curl", "-fsS", "--noproxy", "*", "--max-time", "1", "--connect-timeout", "0.4"}
		if source != "" {
			args = append(args, "--interface", source)
		}
		out, err := exec.Command("ip", append(args, fmt.Sprintf("http://%s:%d/", address, port))...).CombinedOutput()
		return string(out), err
	}
	allowed := func(namespace, address string, port uint16, source, want string) {
		t.Helper()
		if body, err := probe(namespace, address, port, source); err != nil || body != want {
			t.Fatalf("positive %s -> %s:%d: %v %s", namespace, address, port, err, body)
		}
	}
	denied := func(namespace, address string, port uint16) {
		t.Helper()
		if body, err := probe(namespace, address, port, ""); err == nil {
			t.Fatalf("unauthorized %s -> %s:%d became reachable: %s\n%s", namespace, address, port, body, run("iptables-save", "-c"))
		}
	}
	a := LANServiceAccess{AppBridge: "guardbr", NetNS: "/run/netns/guard-app", AppIP: "10.42.2.2", Gateway: "10.42.2.1", Destination: "192.168.41.22", Interface: "guardlh", Protocol: "tcp", Port: 8080}
	foreign := [][]string{
		{"-i", a.AppBridge, "-s", a.AppIP + "/32", "-d", a.Destination + "/32", "-p", "tcp", "--dport", "8081", "-m", "comment", "--comment", "foreign-guard-egress", "-j", "ACCEPT"},
		{"-o", a.AppBridge, "-d", a.AppIP + "/32", "-p", "tcp", "--dport", "9000", "-m", "comment", "--comment", "foreign-guard-ingress", "-j", "ACCEPT"},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	}
	installForeign := func() {
		for _, args := range foreign {
			run("iptables", append([]string{"-A", "FORWARD"}, args...)...)
			t.Cleanup(func() { _ = exec.Command("iptables", append([]string{"-D", "FORWARD"}, args...)...).Run() })
		}
	}
	if foreignFirst {
		installForeign()
	}
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	if !foreignFirst {
		installForeign()
	}
	allowed("guard-app", "10.42.2.3", 9000, "", "app")
	allowed("guard-sibling", a.AppIP, 9000, "", "app")
	allowed("guard-app", a.Destination, 8080, "", "observed")
	denied("guard-app", a.Destination, 8081)
	denied("guard-lan", a.AppIP, 9000)
	denied("guard-alt", a.AppIP, 9000)
	// Host-local OUTPUT and unrelated physical forwarding retain their policy.
	if out, err := exec.Command("curl", "-fsS", "--noproxy", "*", "--max-time", "1", "http://10.42.2.2:9000/").CombinedOutput(); err != nil || string(out) != "app" {
		t.Fatalf("host-local app access changed: %v %s", err, out)
	}
	allowed("guard-lan", "192.168.42.22", 8081, "192.168.41.23", "alternate")
	sibling := a
	sibling.Port = 8082
	if err := EnsureLANServiceAccess(sibling); err != nil {
		t.Fatal(err)
	}
	if err := WithdrawLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	if err := WithdrawLANDrop(a); err != nil {
		t.Fatal(err)
	}
	allowed("guard-app", a.Destination, 8082, "", "observed")
	denied("guard-app", a.Destination, 8080)
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	if err := WithdrawLANServiceAccess(sibling); err != nil {
		t.Fatal(err)
	}
	if err := WithdrawLANDrop(sibling); err != nil {
		t.Fatal(err)
	}
	allowed("guard-app", a.Destination, 8080, "", "observed")
	run("ip", "route", "add", a.Destination+"/32", "via", "192.168.42.22", "dev", "guardxh")
	if !strings.Contains(run("ip", "link", "show", "guardlh"), "UP") {
		t.Fatal("observed link disappeared")
	}
	// An independent host query proves the alternate backend/route is live.
	if out, err := exec.Command("curl", "-fsS", "--noproxy", "*", "--max-time", "1", "http://192.168.41.22:8081/").CombinedOutput(); err != nil || string(out) != "alternate" {
		t.Fatalf("alternate route positive control failed: %v %s", err, out)
	}
	denied("guard-app", a.Destination, 8080)
	denied("guard-app", a.Destination, 8081)
	other := a
	other.Destination, other.Interface = "192.168.42.22", "guardxh"
	if err := EnsureLANServiceAccess(other); err != nil {
		t.Fatal(err)
	}
	allowed("guard-app", other.Destination, 8080, "", "alternate")
	if err := WithdrawLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	if err := WithdrawLANDestination(a); err != nil {
		t.Fatal(err)
	}
	allowed("guard-app", other.Destination, 8080, "", "alternate")
	run("ip", "route", "del", a.Destination+"/32", "via", "192.168.42.22", "dev", "guardxh")
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	allowed("guard-app", a.Destination, 8080, "", "observed")
	for _, access := range []LANServiceAccess{other, a} {
		if err := WithdrawLANServiceAccess(access); err != nil {
			t.Fatal(err)
		}
		if err := WithdrawLANDestination(access); err != nil {
			t.Fatal(err)
		}
	}
	// Model an earlier process's narrower retained guards and live app route.
	// Startup must upgrade them before deleting grants, without app suspension.
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	run("iptables", append([]string{"-D", LANChainName}, a.destinationGuard()...)...)
	run("iptables", append([]string{"-D", LANChainName}, a.replyGuard()...)...)
	run("iptables", append([]string{"-I", LANChainName, "1"}, append(a.base(), "-j", "DROP")...)...)
	run("iptables", "-I", LANChainName, "1", "-d", a.AppIP+"/32", "-i", a.Interface, "-m", "comment", "--comment", lanReplyGuardMarker, "-j", "DROP")
	run("ip", "route", "add", a.Destination+"/32", "via", "192.168.42.22", "dev", "guardxh")
	if err := RevokeOrphanLANServiceAllows(); err != nil {
		t.Fatal(err)
	}
	run("ip", "netns", "exec", "guard-app", "ip", "route", "get", a.Destination)
	denied("guard-app", a.Destination, 8081)
	denied("guard-alt", a.AppIP, 9000)
	if err := RevokeOrphanLANAppRoutes(a.NetNS, a.AppIP, a.Gateway); err != nil {
		t.Fatal(err)
	}
	run("ip", "route", "del", a.Destination+"/32", "via", "192.168.42.22", "dev", "guardxh")
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	allowed("guard-app", a.Destination, 8080, "", "observed")
	if err := WithdrawLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	if err := WithdrawLANDestination(a); err != nil {
		t.Fatal(err)
	}
	denied("guard-alt", a.AppIP, 9000)
	for _, line := range strings.Split(run("iptables-save", "-c"), "\n") {
		if strings.Contains(line, "foreign-guard-egress") && !strings.HasPrefix(line, "[0:") {
			t.Fatalf("denied traffic consumed a foreign accept: %s", line)
		}
	}
	if n := strings.Count(run("iptables", "-S", LANChainName), lanReplyGuardMarker); n != 1 {
		t.Fatalf("shared app guards accumulated: %d", n)
	}
	if err := ReconcileLANReplyGuards(func() ([]string, error) { return []string{"10.42.2.0/28"}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileLANReplyGuards(func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(run("iptables", "-S", LANChainName), lanReplyGuardMarker) {
		t.Fatal("deleted app guard survived reconciliation")
	}
	t.Log("exact port/interface positives, original/changed-route sibling denies, alternate-ingress deny, host/peer controls, sibling ownership, withdrawal/recreate and cleanup passed")
}
