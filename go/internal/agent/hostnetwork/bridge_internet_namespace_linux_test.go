//go:build linux

package hostnetwork

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBridgeInternetEndpoint(t *testing.T) {
	role := os.Getenv("WENDY_BRIDGE_ENDPOINT")
	if role == "" {
		t.Skip("namespace child")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/push" {
			for i := 0; i < 100; i++ {
				if _, err := fmt.Fprintln(w, role); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
			return
		}
		fmt.Fprint(w, role)
	})
	for _, port := range []string{"8080", "8081", "9000"} {
		go http.ListenAndServe(":"+port, handler)
	}
	time.Sleep(60 * time.Second)
}

func TestBridgeInternetNativeAndBorrowedPolicy(t *testing.T) {
	if os.Getenv("WENDY_BRIDGE_INTERNET_NAMESPACE_TEST") != "1" {
		t.Skip("disposable privileged network-none container only")
	}
	if order := os.Getenv("WENDY_BRIDGE_INTERNET_ORDER_CHILD"); order != "" {
		if order != "lan-first" && order != "internet-first" {
			t.Fatalf("invalid fixture order %q", order)
		}
		runBridgeInternetNativeAndBorrowedPolicy(t, order == "internet-first")
		return
	}
	for _, order := range []string{"lan-first", "internet-first"} {
		t.Run(order, func(t *testing.T) {
			// Keep rules, conntrack and sysctls independent between both
			// initialization orders in the disposable test container.
			cmd := exec.Command("unshare", "--net", os.Args[0], "-test.run=^TestBridgeInternetNativeAndBorrowedPolicy$", "-test.v")
			cmd.Env = append(os.Environ(), "WENDY_BRIDGE_INTERNET_ORDER_CHILD="+order)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", order, err, out)
			}
		})
	}
}

func runBridgeInternetNativeAndBorrowedPolicy(t *testing.T, internetFirst bool) {
	t.Helper()
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v %s", name, args, err, out)
		}
		return string(out)
	}
	// No host/production networking exists in this explicitly opted-in test.
	oldMarker := forwardingMarker
	forwardingMarker = filepath.Join(t.TempDir(), "forwarding-baseline")
	t.Cleanup(func() { forwardingMarker = oldMarker })
	for _, pair := range []struct{ iface, namespace, host, peer string }{
		{"biapph", "bi-app", "10.42.3.1/28", "10.42.3.2/28"},
		{"bimeshh", "bi-mesh", "10.42.4.1/28", "10.42.4.2/28"},
		{"biwanh", "bi-native", "192.168.60.1/24", "192.168.60.2/24"},
		{"wlmi-fixture", "bi-borrowed", "10.90.0.1/24", "10.90.0.2/24"},
		{"wlmp-fixture", "bi-peer", "10.91.0.1/24", "10.91.0.2/24"},
		{"wendy-sibling", "bi-sibling", "10.92.0.1/24", "10.92.0.2/24"},
	} {
		run("ip", "netns", "add", pair.namespace)
		t.Cleanup(func() { exec.Command("ip", "netns", "del", pair.namespace).Run() })
		run("ip", "link", "add", pair.iface, "type", "veth", "peer", "name", "bin")
		t.Cleanup(func() { exec.Command("ip", "link", "del", pair.iface).Run() })
		run("ip", "link", "set", "bin", "netns", pair.namespace)
		run("ip", "addr", "add", pair.host, "dev", pair.iface)
		run("ip", "link", "set", pair.iface, "up")
		run("ip", "netns", "exec", pair.namespace, "ip", "addr", "add", pair.peer, "dev", "bin")
		run("ip", "netns", "exec", pair.namespace, "ip", "link", "set", "bin", "up")
		run("ip", "netns", "exec", pair.namespace, "ip", "link", "set", "lo", "up")
		gateway := strings.Split(pair.host, "/")[0]
		run("ip", "netns", "exec", pair.namespace, "ip", "route", "add", "default", "via", gateway)
	}
	for _, endpoint := range []struct{ ns, ip, via string }{
		{"bi-native", "203.0.113.9", "192.168.60.2"},
		{"bi-borrowed", "198.51.100.9", "10.90.0.2"},
		{"bi-peer", "198.51.100.10", "10.91.0.2"},
		{"bi-sibling", "198.51.100.11", "10.92.0.2"},
	} {
		run("ip", "netns", "exec", endpoint.ns, "ip", "addr", "add", endpoint.ip+"/32", "dev", "lo")
		run("ip", "route", "add", endpoint.ip+"/32", "via", endpoint.via)
	}
	run("ip", "netns", "exec", "bi-borrowed", "ip", "addr", "add", "203.0.113.9/32", "dev", "lo")
	run("sysctl", "-w", "net.ipv4.ip_forward=0")
	run("iptables", "-P", "FORWARD", "ACCEPT")
	foreign := []string{"-s", "198.18.0.1/32", "-m", "comment", "--comment", "foreign-test-rule", "-j", "DROP"}
	run("iptables", append([]string{"-A", "FORWARD"}, foreign...)...)
	const owner = "bridge-internet-test/app/eth0"
	if internetFirst {
		// The CNI bridge gateway enables the switch before publishing its
		// Internet grant, even when no agent forwarding consumer exists yet.
		run("sysctl", "-w", "net.ipv4.ip_forward=1")
		if err := EnsureBridgeInternetAccess(owner, "biapph", []net.IP{net.ParseIP("10.42.3.2")}); err != nil {
			t.Fatal(err)
		}
	}
	// Upgrade the exact two-rule b810 guard in place. CNI can already have
	// enabled forwarding while the persisted owner still records baseline 0.
	if err := InitLANServiceChain(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forwardingMarker, []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("iptables", "-I", "FORWARD", "1", "-m", "comment", "--comment", forwardingComment, "-j", "DROP")
	run("iptables", "-I", "FORWARD", "1", "-m", "comment", "--comment", forwardingComment, "-j", LANChainName)
	run("sysctl", "-w", "net.ipv4.ip_forward=1")
	// Model the existing host-local boundary; Internet access must not add
	// any INPUT exception for gateway/host services.
	run("iptables", "-A", "INPUT", "-i", "biapph", "-p", "tcp", "--dport", "8080", "-j", "DROP")
	for _, iface := range []string{"biwanh", "wlmi-fixture"} {
		run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "10.42.3.2/32", "-o", iface, "-j", "MASQUERADE")
	}
	if err := AcquireForwarding("bridge-internet-test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ReleaseForwarding("bridge-internet-test") })
	var owned []string
	for _, line := range strings.Split(run("iptables", "-S", "FORWARD"), "\n") {
		if strings.Contains(line, forwardingComment) {
			owned = append(owned, strings.Fields(line)[len(strings.Fields(line))-1])
		}
	}
	if strings.Join(owned, ",") != BridgeInternetChain+","+LANChainName+",DROP" {
		t.Fatalf("upgrade changed Internet/LAN/fallback precedence: %v", owned)
	}
	if err := EnsureBridgeInternetAccess(owner, "biapph", []net.IP{net.ParseIP("10.42.3.2")}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RevokeBridgeInternetAccess(owner) })
	ips := []net.IP{net.ParseIP("10.42.3.2")}
	if err := CheckBridgeInternetAccess(owner, "biapph", ips); err != nil {
		t.Fatal(err)
	}
	// Live firewall/routing rules cannot compensate for missing or stale
	// ownership metadata: DEL needs that record to reclaim policy routing.
	if err := os.Remove(bridgeGrantPath(owner)); err != nil {
		t.Fatal(err)
	}
	if err := CheckBridgeInternetAccess(owner, "biapph", ips); err == nil {
		t.Fatal("CHECK accepted a grant whose reply-rule ownership was lost")
	}
	if err := rememberBridgeGrant(owner, "biapph", []net.IP{net.ParseIP("10.42.3.3")}); err != nil {
		t.Fatal(err)
	}
	if err := CheckBridgeInternetAccess(owner, "biapph", ips); err == nil {
		t.Fatal("CHECK accepted mismatched ownership addresses")
	}
	if err := rememberBridgeGrant(owner, "biapph", ips); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"app", "native", "borrowed", "peer", "sibling"} {
		cmd := exec.Command("ip", "netns", "exec", "bi-"+role, os.Args[0], "-test.run=^TestBridgeInternetEndpoint$")
		cmd.Env = append(os.Environ(), "WENDY_BRIDGE_ENDPOINT="+role)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	}
	// The host-local endpoint is deliberately listening, so the denial is
	// meaningful rather than a closed-port false positive.
	host := exec.Command(os.Args[0], "-test.run=^TestBridgeInternetEndpoint$")
	host.Env = append(os.Environ(), "WENDY_BRIDGE_ENDPOINT=host")
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Process.Kill(); host.Wait() })
	time.Sleep(300 * time.Millisecond)
	probe := func(ns, address string) (string, error) {
		returnBytes, err := exec.Command("ip", "netns", "exec", ns, "curl", "--noproxy", "*", "-fsS", "--connect-timeout", "0.4", "--max-time", "0.6", "http://"+address+"/").CombinedOutput()
		return string(returnBytes), err
	}
	allowed := func(ns, address, want string) {
		t.Helper()
		if out, err := probe(ns, address); err != nil || out != want {
			t.Fatalf("allowed %s -> %s: %v %s\n%s", ns, address, err, out, run("iptables-save", "-c"))
		}
	}
	denied := func(ns, address string) {
		t.Helper()
		if out, err := probe(ns, address); err == nil {
			t.Fatalf("unexpected access %s -> %s: %s", ns, address, out)
		}
	}
	allowed("bi-app", "203.0.113.9:8080", "native")
	allowed("bi-app", "198.51.100.9:8080", "borrowed")
	for _, address := range []string{"203.0.113.9:8080", "198.51.100.9:8080"} {
		denied("bi-mesh", address) // Even a manually added default cannot grant Internet.
	}
	denied("bi-app", "192.168.60.2:8080")  // Native gateway/LAN needs a service grant.
	denied("bi-app", "10.42.3.1:8080")     // Host-local listening service remains blocked.
	denied("bi-app", "198.51.100.10:8080") // Raw Babel peer is not an Internet tunnel.
	denied("bi-app", "198.51.100.11:8080") // Nor is another app's link.
	denied("bi-native", "10.42.3.2:9000")  // Unsolicited native and borrowed inbound denied.
	denied("bi-borrowed", "10.42.3.2:9000")
	// Exact physical-LAN projections still precede the fallback; the native
	// exclusions must return to those grants for their private destination.
	lan := LANServiceAccess{AppBridge: "biapph", NetNS: "/run/netns/bi-app", AppIP: "10.42.3.2", Gateway: "10.42.3.1", Destination: "192.168.60.2", Interface: "biwanh", Protocol: "tcp", Port: 8081}
	if err := EnsureLANServiceAccess(lan); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WithdrawLANServiceAccess(lan) })
	allowed("bi-app", "192.168.60.2:8081", "native")
	denied("bi-app", "192.168.60.2:8080")
	// Fail over an already granted destination without rerunning CNI ADD.
	run("ip", "route", "replace", "203.0.113.9/32", "via", "10.90.0.2")
	allowed("bi-app", "203.0.113.9:8080", "borrowed")
	run("ip", "route", "replace", "203.0.113.9/32", "via", "192.168.60.2")
	allowed("bi-app", "203.0.113.9:8080", "native")
	// Revoke while both paths have live NAT/conntrack entries. Established
	// replies must stop too; an ESTABLISHED shortcut would fail this check.
	var streams []*os.File
	for _, address := range []string{"203.0.113.9:8080", "198.51.100.9:8080"} {
		file, err := os.CreateTemp(t.TempDir(), "push")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		cmd := exec.Command("ip", "netns", "exec", "bi-app", "curl", "--noproxy", "*", "-fsSN", "--max-time", "6", "http://"+address+"/push")
		cmd.Stdout = file
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		streams = append(streams, file)
	}
	time.Sleep(300 * time.Millisecond)
	for _, file := range streams {
		if info, _ := file.Stat(); info.Size() < 10 {
			t.Fatal("push never became live")
		}
	}
	if err := RevokeBridgeInternetAccess(owner); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	var sizes []int64
	for _, file := range streams {
		info, _ := file.Stat()
		sizes = append(sizes, info.Size())
	}
	time.Sleep(300 * time.Millisecond)
	for i, file := range streams {
		if info, _ := file.Stat(); info.Size() != sizes[i] {
			t.Fatal("revoked Internet stream still delivers established replies")
		}
	}
	denied("bi-app", "203.0.113.9:8080")
	denied("bi-app", "198.51.100.9:8080")
	allowed("bi-app", "192.168.60.2:8081", "native") // Revocation preserves the independent LAN grant.
	if err := WithdrawLANServiceAccess(lan); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseForwarding("bridge-internet-test"); err != nil {
		t.Fatal(err)
	}
	dispatch := run("iptables", "-S", lanForwardChain)
	internetAt, lanAt := strings.Index(dispatch, "-j "+BridgeInternetChain), strings.Index(dispatch, "-j "+LANChainName)
	if internetAt < 0 || lanAt <= internetAt {
		t.Fatalf("dispatcher shadows explicit Internet grants: %s", dispatch)
	}
	if text := run("iptables", "-S", "FORWARD"); strings.Contains(text, forwardingComment) || !strings.Contains(text, "foreign-test-rule") || !strings.Contains(text, lanForwardMarker) {
		t.Fatalf("release failed to remove its fallback while preserving the LAN dispatcher and foreign rule: %s", text)
	}
}
