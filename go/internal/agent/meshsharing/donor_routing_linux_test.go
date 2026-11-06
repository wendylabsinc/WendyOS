//go:build linux

package meshsharing

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"golang.org/x/sys/unix"
)

func TestDonorRoutingNamespace(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("disposable privileged Linux network namespace required")
	}
	ns := fmt.Sprintf("wmdr%d", os.Getpid())
	if out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput(); err != nil {
		t.Fatalf("create namespace: %v %s", err, out)
	}
	defer exec.Command("ip", "netns", "del", ns).Run()
	cmd := exec.Command("ip", "netns", "exec", ns, os.Args[0], "-test.run=^TestDonorRoutingNamespaceProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "WENDY_DONOR_ROUTING_PROCESS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("donor routing: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

func TestDonorRoutingNamespaceProcess(t *testing.T) {
	if os.Getenv("WENDY_DONOR_ROUTING_PROCESS") != "1" {
		t.Skip("namespace subprocess")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
		return string(out)
	}
	run("link", "set", "lo", "up")
	for _, iface := range []string{"uplink0", "uplink1", "wlmi1"} {
		run("link", "add", iface, "type", "dummy")
		run("link", "set", iface, "up")
	}
	for _, item := range []struct{ family, addr, iface string }{{"-4", "192.0.2.2/24", "uplink0"}, {"-4", "198.18.1.2/24", "uplink1"}, {"-6", "2001:db8:bad::2/64", "uplink0"}, {"-6", "2001:db8:1::2/64", "uplink1"}} {
		args := []string{item.family, "addr", "add", item.addr, "dev", item.iface}
		if item.family == "-6" {
			args = append(args, "nodad")
		}
		run(args...)
	}
	for _, item := range []struct{ family, gateway, metric string }{{"-4", "192.0.2.1", "5"}, {"-4", "198.18.1.1", "100"}, {"-6", "2001:db8:bad::1", "5"}, {"-6", "2001:db8:1::1", "100"}} {
		run(item.family, "route", "add", "default", "via", item.gateway, "metric", item.metric)
	}
	for _, path := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv6/conf/all/forwarding"} {
		if err := os.WriteFile(path, []byte("1\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lease := tunnelLease{Slot: 1, IPv6: true}
	routes, err := selectedDonorRoutes("uplink1", true)
	if err != nil {
		t.Fatal(err)
	}
	policy := donorRouting{}
	if err := policy.install(routes); err != nil {
		t.Fatal(err)
	}
	var rules []netlink.Rule
	if err := addDonorLeaseRules("wlmi1", lease, &rules); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeDonorLeaseRules(&rules); _ = policy.close() })
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		live, err := netlink.RuleList(family)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, r := range live {
			if r.Protocol == tunnelProtocol {
				count++
				if !ownedDonorRule(r) {
					t.Fatalf("cannot reclaim live rule: %#v", r)
				}
			}
		}
		if count != 2 {
			t.Fatalf("got %d lease rules for family %d", count, family)
		}
	}
	destinations := []struct{ family, target, source string }{{"-4", "203.0.113.7", lease.address4().String()}, {"-6", "2001:db8:feed::7", lease.address6().String()}}
	for _, dst := range destinations {
		if out := run(dst.family, "route", "get", dst.target, "from", dst.source, "iif", "wlmi1"); !strings.Contains(out, "dev uplink1") {
			t.Fatalf("lease chose wrong uplink: %s", out)
		}
		if out := run(dst.family, "route", "get", dst.target); !strings.Contains(out, "dev uplink0") {
			t.Fatalf("host route changed: %s", out)
		}
	}
	// A different incoming link or non-admitted source must not use this policy.
	if out := run("-4", "route", "get", "203.0.113.7", "from", lease.address4().String(), "iif", "uplink0"); !strings.Contains(out, "dev uplink0") {
		t.Fatalf("wrong iif matched: %s", out)
	}
	if out := run("-4", "route", "get", "203.0.113.7", "from", "10.88.0.2", "iif", "wlmi1"); !strings.Contains(out, "dev uplink0") {
		t.Fatalf("wrong source matched: %s", out)
	}
	// Refresh detects a new gateway even though interface and DNS stay unchanged.
	run("-4", "route", "replace", "default", "via", "198.18.1.3", "dev", "uplink1", "metric", "100")
	changed, err := selectedDonorRoutes("uplink1", true)
	if err != nil {
		t.Fatal(err)
	}
	if sameDonorRoutes(routes, changed) {
		t.Fatal("gateway change was ignored")
	}
	if err := removeDonorLeaseRules(&rules); err != nil {
		t.Fatal(err)
	}
	if err := policy.close(); err != nil {
		t.Fatal(err)
	}
	if err := policy.install(changed); err != nil {
		t.Fatal(err)
	}
	if err := addDonorLeaseRules("wlmi1", lease, &rules); err != nil {
		t.Fatal(err)
	}
	if out := run("-4", "route", "get", "203.0.113.7", "from", lease.address4().String(), "iif", "wlmi1"); !strings.Contains(out, "via 198.18.1.3") {
		t.Fatalf("stale selected gateway: %s", out)
	}
	// Reclaim a complete live table as after a crash, not just terminal routes.
	if err := reclaimDonorRouting(); err != nil {
		t.Fatal(err)
	}
	rules, policy.routes = nil, nil
	if err := checkDonorRoutingAvailable(); err != nil {
		t.Fatal(err)
	}
	if err := policy.install(changed); err != nil {
		t.Fatal(err)
	}
	if err := addDonorLeaseRules("wlmi1", lease, &rules); err != nil {
		t.Fatal(err)
	}
	// Removing the selected physical interface leaves the no-interface terminal
	// default. Even deleting that whole table must not fall back to uplink0.
	run("link", "del", "uplink1")
	for _, stage := range []string{"interface removed", "table emptied"} {
		if stage == "table emptied" {
			run("-4", "route", "flush", "table", "18868")
			run("-6", "route", "flush", "table", "18868")
		}
		for _, dst := range destinations {
			out, err := exec.Command("ip", dst.family, "route", "get", dst.target, "from", dst.source, "iif", "wlmi1").CombinedOutput()
			if err == nil || (!strings.Contains(string(out), "Network is unreachable") && !strings.Contains(string(out), "No route to host")) {
				t.Fatalf("%s failed open: %v %s", stage, err, out)
			}
		}
	}
	run("link", "del", "wlmi1")
	// Simulate crash reclamation, including a foreign rule using our reserved
	// priority. It must survive and prevent reuse rather than be deleted.
	foreign := netlink.NewRule()
	foreign.Family = netlink.FAMILY_V4
	foreign.Priority = donorRoutingPriority
	foreign.Table = 18870
	foreign.Protocol = 99
	if err := netlink.RuleAdd(foreign); err != nil {
		t.Fatal(err)
	}
	foreignRoute := netlink.Route{Table: donorRoutingTable, Family: netlink.FAMILY_V4, Type: unix.RTN_UNREACHABLE, Protocol: 99, Priority: 32766, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}
	if err := netlink.RouteAdd(&foreignRoute); err != nil {
		t.Fatal(err)
	}
	for _, route := range changed {
		if route.Type == unix.RTN_UNREACHABLE {
			if err := netlink.RouteAdd(&route); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := reclaimDonorRouting(); err != nil {
		t.Fatal(err)
	}
	rules = nil
	policy.routes = nil // reclaimed by simulated restarted process.
	live, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range live {
		if r.Protocol == 99 && r.Priority == donorRoutingPriority {
			found = true
		}
		if r.Protocol == tunnelProtocol {
			t.Fatalf("orphan owned rule: %#v", r)
		}
	}
	if !found {
		t.Fatal("foreign rule deleted")
	}
	existing, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: donorRoutingTable}, netlink.RT_FILTER_TABLE)
	if err != nil || len(existing) != 1 || existing[0].Protocol != 99 {
		t.Fatalf("foreign route changed: %v %v", existing, err)
	}
	if err := checkDonorRoutingAvailable(); err == nil {
		t.Fatal("foreign routing state accepted")
	}
	if err := netlink.RuleDel(foreign); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteDel(&foreignRoute); err != nil {
		t.Fatal(err)
	}
	if err := checkDonorRoutingAvailable(); err != nil {
		t.Fatal(err)
	}
	// Guard shape recognition independently of kernel representation.
	fake := *netlink.NewRule()
	fake.Protocol = tunnelProtocol
	fake.Priority = donorRoutingPriority
	fake.Table = donorRoutingTable
	fake.Src = &net.IPNet{IP: net.ParseIP("10.88.0.1"), Mask: net.CIDRMask(16, 32)}
	fake.IifName = "wlmi1"
	if ownedDonorRule(fake) {
		t.Fatal("broad foreign source rule mistaken for owned lease")
	}
	t.Log("dual-stack exact lease routing, host isolation, gateway refresh, fail-closed deletion and ownership cleanup passed")
	// A failed final forwarding release can outlive all routes and rules.
	// Retrying an empty sharing intent must still release that retained owner.
	if err := hostnetwork.AcquireIPv6Forwarding("mesh-internet"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hostnetwork.ReleaseIPv6Forwarding("mesh-internet") })
	cleanupPolicy := &linuxTunnelPolicy{forwarding6: true}
	if err := cleanupPolicy.SetSharing(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if cleanupPolicy.forwarding6 {
		t.Fatal("empty sharing intent skipped retained IPv6 forwarding cleanup")
	}
}
