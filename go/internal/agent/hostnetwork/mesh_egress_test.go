package hostnetwork

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// meshRuleFixture pre-creates the WENDY-MESH chain (which in production the
// agent creates via InitMeshChain before any container starts) so
// AddMeshRule/RemoveMeshRule have somewhere to install rules. It registers
// cleanup so the chain is flushed and removed afterward.
func meshRuleFixture(t *testing.T) {
	t.Helper()
	requireIPTables(t)

	if err := InitMeshChain(); err != nil {
		t.Fatalf("InitMeshChain: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("iptables", "-t", "filter", "-F", MeshChainName).Run()
		exec.Command("iptables", "-t", "filter", "-D", "FORWARD", "-j", MeshChainName).Run()
		exec.Command("iptables", "-t", "filter", "-X", MeshChainName).Run()
	})
}

// meshRuleCount returns how many rules matching containerIP/serviceCIDR exist
// in the WENDY-MESH chain, by parsing `iptables -S WENDY-MESH` output.
func meshRuleCount(t *testing.T, containerIP, serviceCIDR string) int {
	t.Helper()
	out, err := exec.Command("iptables", "-t", "filter", "-S", MeshChainName).CombinedOutput()
	if err != nil {
		t.Fatalf("iptables -S %s failed: %v\n%s", MeshChainName, err, out)
	}
	want := fmt.Sprintf("-s %s/32 -d %s -j ACCEPT", containerIP, serviceCIDR)
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, want) {
			count++
		}
	}
	return count
}

func TestAddMeshRuleCreatesRule(t *testing.T) {
	meshRuleFixture(t)

	const containerIP = "10.88.0.7"
	const serviceCIDR = "10.99.0.0/16"

	if err := AddMeshRule(containerIP, serviceCIDR); err != nil {
		t.Fatalf("AddMeshRule: %v", err)
	}

	if got := meshRuleCount(t, containerIP, serviceCIDR); got != 1 {
		t.Fatalf("expected exactly 1 mesh rule after AddMeshRule, got %d", got)
	}
}

func TestAddMeshRuleIsIdempotent(t *testing.T) {
	meshRuleFixture(t)

	const containerIP = "10.88.0.8"
	const serviceCIDR = "10.99.0.0/16"

	if err := AddMeshRule(containerIP, serviceCIDR); err != nil {
		t.Fatalf("first AddMeshRule: %v", err)
	}
	if err := AddMeshRule(containerIP, serviceCIDR); err != nil {
		t.Fatalf("second AddMeshRule: %v", err)
	}

	if got := meshRuleCount(t, containerIP, serviceCIDR); got != 1 {
		t.Fatalf("expected exactly 1 mesh rule after two AddMeshRule calls, got %d", got)
	}
}

func TestRemoveMeshRuleDeletesRule(t *testing.T) {
	meshRuleFixture(t)

	const containerIP = "10.88.0.9"
	const serviceCIDR = "10.99.0.0/16"

	if err := AddMeshRule(containerIP, serviceCIDR); err != nil {
		t.Fatalf("AddMeshRule: %v", err)
	}
	if err := RemoveMeshRule(containerIP, serviceCIDR); err != nil {
		t.Fatalf("RemoveMeshRule: %v", err)
	}

	if got := meshRuleCount(t, containerIP, serviceCIDR); got != 0 {
		t.Fatalf("expected 0 mesh rules after RemoveMeshRule, got %d", got)
	}
}

func TestRemoveMeshRuleWhenAbsentIsNotError(t *testing.T) {
	meshRuleFixture(t)

	const containerIP = "10.88.0.10"
	const serviceCIDR = "10.99.0.0/16"

	// Never added; removing must still succeed.
	if err := RemoveMeshRule(containerIP, serviceCIDR); err != nil {
		t.Fatalf("first RemoveMeshRule (nothing installed) errored: %v", err)
	}
	if err := RemoveMeshRule(containerIP, serviceCIDR); err != nil {
		t.Fatalf("second RemoveMeshRule (still nothing installed) errored: %v", err)
	}
}

// TestAddMeshRuleSurfacesRealIPTablesError covers C3a-review Minor #2: a
// genuinely broken iptables invocation (here, a serviceCIDR that iptables
// itself rejects as an unparsable host/network specification, exit code 2)
// must surface as an error from AddMeshRule, not be silently treated as "rule
// absent" (exit code 1, the only code meshRuleExists/AddMeshRule should ever
// swallow).
func TestAddMeshRuleSurfacesRealIPTablesError(t *testing.T) {
	meshRuleFixture(t)

	const containerIP = "10.88.0.11"
	const malformedCIDR = "not-a-cidr"

	err := AddMeshRule(containerIP, malformedCIDR)
	if err == nil {
		t.Fatal("AddMeshRule with a malformed serviceCIDR: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "iptables") {
		t.Fatalf("AddMeshRule error does not look like a surfaced iptables failure: %v", err)
	}
}

// TestRemoveMeshRuleSurfacesRealIPTablesError mirrors the Add case for
// RemoveMeshRule (C3a-review Minor #2).
func TestRemoveMeshRuleSurfacesRealIPTablesError(t *testing.T) {
	meshRuleFixture(t)

	const containerIP = "10.88.0.12"
	const malformedCIDR = "not-a-cidr"

	err := RemoveMeshRule(containerIP, malformedCIDR)
	if err == nil {
		t.Fatal("RemoveMeshRule with a malformed serviceCIDR: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "iptables") {
		t.Fatalf("RemoveMeshRule error does not look like a surfaced iptables failure: %v", err)
	}
}

// requireNetnsTools skips unless running as root with ip/nsenter available,
// since SetMeshRoute manipulates a real network namespace.
func requireNetnsTools(t *testing.T) {
	t.Helper()
	requireIPTables(t)
	for _, bin := range []string{"ip", "nsenter"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s binary not found in PATH", bin)
		}
	}
}

// testNetns creates a throwaway network namespace with a dummy0 link holding
// an address in the gateway's subnet, mirroring the plugin test fixture's
// dummy-link setup so a `via <gateway>` route can resolve. It returns the
// path nsenter/SetMeshRoute expect (/var/run/netns/<name>) and registers
// cleanup to delete the namespace.
func testNetns(t *testing.T, name, addrCIDR string) string {
	t.Helper()
	requireNetnsTools(t)

	if out, err := exec.Command("ip", "netns", "add", name).CombinedOutput(); err != nil {
		t.Fatalf("ip netns add %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		exec.Command("ip", "netns", "del", name).Run()
	})

	steps := [][]string{
		{"ip", "netns", "exec", name, "ip", "link", "set", "lo", "up"},
		{"ip", "link", "add", "dummy0", "netns", name, "type", "dummy"},
		{"ip", "netns", "exec", name, "ip", "addr", "add", addrCIDR, "dev", "dummy0"},
		{"ip", "netns", "exec", name, "ip", "link", "set", "dummy0", "up"},
	}
	for _, s := range steps {
		if out, err := exec.Command(s[0], s[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", s, err, out)
		}
	}

	return "/var/run/netns/" + name
}

// netnsRoutes returns the `ip route` output for the given netns path via
// nsenter, so tests can assert on installed routes without a netlink dep.
func netnsRoutes(t *testing.T, netnsPath string) string {
	t.Helper()
	out, err := exec.Command("nsenter", "--net="+netnsPath, "--", "ip", "route").CombinedOutput()
	if err != nil {
		t.Fatalf("nsenter ip route: %v\n%s", err, out)
	}
	return string(out)
}

func TestSetMeshRouteAddsRouteInNetns(t *testing.T) {
	netnsPath := testNetns(t, "wendytest1", "10.88.0.7/24")

	const serviceCIDR = "10.99.0.0/16"
	const gateway = "10.88.0.1"

	if err := SetMeshRoute(netnsPath, serviceCIDR, gateway); err != nil {
		t.Fatalf("SetMeshRoute: %v", err)
	}

	routes := netnsRoutes(t, netnsPath)
	if !strings.Contains(routes, "10.99.0.0/16") || !strings.Contains(routes, "10.88.0.1") {
		t.Fatalf("expected service CIDR route via gateway in netns routes, got:\n%s", routes)
	}
}

func TestSetMeshRouteIsIdempotent(t *testing.T) {
	netnsPath := testNetns(t, "wendytest2", "10.88.0.7/24")

	const serviceCIDR = "10.99.0.0/16"
	const gateway = "10.88.0.1"

	if err := SetMeshRoute(netnsPath, serviceCIDR, gateway); err != nil {
		t.Fatalf("first SetMeshRoute: %v", err)
	}
	if err := SetMeshRoute(netnsPath, serviceCIDR, gateway); err != nil {
		t.Fatalf("second SetMeshRoute (retry) should be idempotent, got error: %v", err)
	}
}

func TestSetMeshMDNSRouteIsScopedAndIdempotent(t *testing.T) {
	netnsPath := testNetns(t, "wendymdns1", "10.88.0.7/24")
	for i := 0; i < 2; i++ {
		if err := SetMeshMDNSRoute(netnsPath, "dummy0"); err != nil {
			t.Fatalf("SetMeshMDNSRoute attempt %d: %v", i+1, err)
		}
	}
	routes := netnsRoutes(t, netnsPath)
	if !strings.Contains(routes, "224.0.0.251 dev dummy0 scope link") ||
		strings.Contains(routes, "224.0.0.0/4") || strings.Contains(routes, "default") {
		t.Fatalf("mDNS route escaped app link scope: %s", routes)
	}
}

func TestSetMeshRouteRejectsInvalidCIDR(t *testing.T) {
	if err := SetMeshRoute("/var/run/netns/does-not-matter", "not-a-cidr", "10.88.0.1"); err == nil {
		t.Fatal("expected error for invalid serviceCIDR, got nil")
	}
}

func TestSetMeshRouteRejectsInvalidGateway(t *testing.T) {
	if err := SetMeshRoute("/var/run/netns/does-not-matter", "10.99.0.0/16", "not-an-ip"); err == nil {
		t.Fatal("expected error for invalid gateway, got nil")
	}
}
