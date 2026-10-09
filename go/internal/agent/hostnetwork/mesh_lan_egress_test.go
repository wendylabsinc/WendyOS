package hostnetwork

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLANServiceRulesRestrictPortAndRestartRevokesAllows(t *testing.T) {
	oldSysctl, oldMarker := forwardingSysctl, forwardingMarker
	forwardingSysctl = filepath.Join(t.TempDir(), "forwarding")
	forwardingMarker = filepath.Join(t.TempDir(), "baseline")
	os.WriteFile(forwardingSysctl, []byte("1\n"), 0600)
	t.Cleanup(func() {
		forwardingSysctl, forwardingMarker = oldSysctl, oldMarker
		forwarding.users = make(map[string]bool)
	})

	oldTables, oldRoute, oldHostRoute := lanIPTables, lanIPRoute, lanHostRoute
	t.Cleanup(func() { lanIPTables, lanIPRoute, lanHostRoute = oldTables, oldRoute, oldHostRoute })
	lanHostRoute = func(string) ([]byte, error) { return []byte("192.168.41.22 dev eth7 src 192.168.41.1\n"), nil }
	rules := make(map[string][]string)
	var routeCommands []string
	lanIPRoute = func(_ string, args ...string) ([]byte, error) {
		routeCommands = append(routeCommands, strings.Join(args, " "))
		return nil, nil
	}
	absent := exec.Command("sh", "-c", "exit 1").Run()
	lanIPTables = func(args ...string) ([]byte, error) {
		if len(args) < 4 || args[0] != "-t" {
			t.Fatalf("unexpected iptables arguments %v", args)
		}
		table, verb, chain := args[1], args[2], args[3]
		chainKey := table + "/" + chain
		if verb == "-S" {
			var out []string
			for _, rule := range rules[chainKey] {
				out = append(out, "-A "+chain+" "+rule)
			}
			return []byte(strings.Join(out, "\n")), nil
		}
		parts := args[4:]
		if verb == "-I" {
			parts = parts[1:]
		}
		rule := strings.Join(parts, " ")
		idx := slices.Index(rules[chainKey], rule)
		switch verb {
		case "-N":
			return nil, nil
		case "-C":
			if idx < 0 {
				return nil, absent
			}
			return nil, nil
		case "-I":
			rules[chainKey] = append([]string{rule}, rules[chainKey]...)
		case "-A":
			rules[chainKey] = append(rules[chainKey], rule)
		case "-D":
			if idx < 0 {
				t.Fatalf("delete missing rule %q", rule)
			}
			rules[chainKey] = append(rules[chainKey][:idx], rules[chainKey][idx+1:]...)
		default:
			t.Fatalf("unexpected iptables verb %q", verb)
		}
		return nil, nil
	}
	a := LANServiceAccess{AppBridge: "wendy0", NetNS: "/proc/self/ns/net", AppIP: "10.42.2.2", Gateway: "10.42.2.1",
		Destination: "192.168.41.22", Interface: "eth7", Protocol: "tcp", Port: 8080}
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	filter := rules["filter/"+LANChainName]
	if len(filter) != 4 || !strings.Contains(strings.Join(filter, "\n"), "--ctstate ESTABLISHED --ctdir REPLY -j ACCEPT") {
		t.Fatalf("unexpected app port guard ordering: %v", filter)
	}
	if len(routeCommands) != 1 || routeCommands[0] != "replace 192.168.41.22/32 via 10.42.2.1" {
		t.Fatalf("route is not exact /32: %v", routeCommands)
	}
	if err := RevokeOrphanLANServiceAllows(); err != nil {
		t.Fatal(err)
	}
	if got := rules["filter/"+LANChainName]; len(got) != 2 || !strings.HasSuffix(got[0], "-j DROP") {
		t.Fatalf("restart retained allow or removed fail-closed guard: %v", got)
	}
	if got := rules["nat/POSTROUTING"]; len(got) != 0 {
		t.Fatalf("restart retained NAT grants: %v", got)
	}
	if err := RevokeOrphanLANAppRoutes(a.NetNS, a.AppIP, a.Gateway); err != nil {
		t.Fatal(err)
	}
	if len(rules["filter/"+LANChainName]) != 1 || !slices.Contains(routeCommands, "del 192.168.41.22/32 via 10.42.2.1") {
		t.Fatalf("inherited app route/guard not revoked: rules=%v routes=%v", rules["filter/"+LANChainName], routeCommands)
	}
	if err := EnsureLANServiceAccess(a); err != nil {
		t.Fatalf("recovery failed to reauthorize service: %v", err)
	}
	if err := WithdrawLANServiceAccess(a); err != nil {
		t.Fatal(err)
	}
	if err := WithdrawLANDestination(a); err != nil {
		t.Fatal(err)
	}
	if len(rules["filter/"+LANChainName]) != 1 || len(rules["nat/POSTROUTING"]) != 0 {
		t.Fatalf("teardown left owned rules: %v %v", rules["filter/"+LANChainName], rules["nat/POSTROUTING"])
	}
	if err := ReconcileLANReplyGuards(func() ([]string, error) { return nil, fmt.Errorf("inventory unavailable") }); err == nil {
		t.Fatal("failed inventory accepted")
	}
	if len(rules["filter/"+LANChainName]) != 1 {
		t.Fatal("inventory failure removed guard")
	}
	if err := ReconcileLANReplyGuards(func() ([]string, error) { return []string{"10.42.2.0/28"}, nil }); err != nil {
		t.Fatal(err)
	}
	if len(rules["filter/"+LANChainName]) != 1 {
		t.Fatal("live/taskless mesh app guard removed")
	}
	if err := ReconcileLANReplyGuards(func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if len(rules["filter/"+LANChainName]) != 0 {
		t.Fatal("deleted app guard retained")
	}
}

func TestLANAccessAllowsOrdinaryTenNetAndRejectsMeshPrefixes(t *testing.T) {
	a := LANServiceAccess{AppBridge: "wendy0", NetNS: "/proc/self/ns/net", AppIP: "10.42.2.2", Gateway: "10.42.2.1",
		Destination: "10.44.1.7", Interface: "eth7", Protocol: "tcp", Port: 8080}
	if err := a.validate(); err != nil {
		t.Fatalf("ordinary 10.x LAN denied: %v", err)
	}
	for _, ip := range []string{"10.88.1.7", "10.99.1.7", "127.0.0.1", "224.0.0.251"} {
		a.Destination = ip
		if err := a.validate(); err == nil {
			t.Errorf("mesh or non-unicast destination %s admitted", ip)
		}
	}
}
