//go:build linux

package meshsharing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestCleanupStaleCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CleanupStale(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup cleanup: %v", err)
	}
}

func TestCleanupStaleNamespace(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("disposable privileged Linux network namespace required")
	}
	ns := fmt.Sprintf("wmclean%d", os.Getpid())
	if out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput(); err != nil {
		t.Fatalf("create namespace: %v %s", err, out)
	}
	defer exec.Command("ip", "netns", "del", ns).Run()
	cmd := exec.Command("ip", "netns", "exec", ns, os.Args[0], "-test.run=^TestCleanupStaleNamespaceProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "WENDY_CLEANUP_PROCESS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("startup cleanup: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

func TestCleanupStaleNamespaceProcess(t *testing.T) {
	if os.Getenv("WENDY_CLEANUP_PROCESS") != "1" {
		t.Skip("namespace subprocess")
	}
	ctx := context.Background()
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v %s", name, args, err, out)
		}
		return string(out)
	}
	run("ip", "link", "set", "lo", "up")
	// An already-enabled host baseline is foreign state, not ours to restore.
	for _, key := range []string{"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding"} {
		if strings.TrimSpace(run("sysctl", "-n", key)) != "1" {
			run("sysctl", "-w", key+"=1")
		}
	}
	self, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := newInternetTunnel("wlmi1", self, peer)
	if err != nil {
		t.Fatal(err)
	}
	lease := tunnelLease{Slot: 1}
	if err := tunnel.assign(lease.address4()); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.route(lease.address4(), true); err != nil {
		t.Fatal(err)
	}
	var donorRules []netlink.Rule
	if err := addDonorLeaseRules(tunnel.link.Name, lease, &donorRules); err != nil {
		t.Fatal(err)
	}
	// Name/priority matches alone never establish ownership.
	run("ip", "link", "add", "wlmi-foreign", "type", "dummy")
	run("ip", "-4", "rule", "add", "priority", "18868", "table", "18870", "protocol", "99")
	run("ip", "-4", "route", "add", "unreachable", "default", "table", "18868", "metric", "32766", "protocol", "99")
	for _, tool := range []string{"iptables", "ip6tables"} {
		run(tool, "-A", "FORWARD", "-m", "comment", "--comment", internetMarker, "-j", "DROP")
		run(tool, "-A", "FORWARD", "-m", "comment", "--comment", "foreign-guard", "-j", "DROP")
	}
	// A conflicting owner alias must stop before deleting firewall guards.
	run("ip", "link", "add", "owner-conflict", "type", "dummy")
	run("ip", "link", "set", "owner-conflict", "alias", tunnelOwner)
	if err := CleanupStale(ctx); err == nil {
		t.Fatal("accepted ownership marker on the wrong link type")
	}
	for _, tool := range []string{"iptables", "ip6tables"} {
		if !strings.Contains(run(tool, "-S", "FORWARD"), internetMarker) {
			t.Fatal("removed firewall guard after tunnel ownership failure")
		}
	}
	run("ip", "link", "del", "owner-conflict")
	for range 2 {
		if err := CleanupStale(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := netlink.LinkByName("wlmi1"); err == nil {
		t.Fatal("owned Internet tunnel survived startup cleanup")
	}
	if _, err := netlink.LinkByName("wlmi-foreign"); err != nil {
		t.Fatal("foreign interface with mesh-like name was removed")
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	foreign := false
	for _, rule := range rules {
		if rule.Protocol == tunnelProtocol {
			t.Fatalf("owned donor rule survived: %+v", rule)
		}
		foreign = foreign || rule.Priority == donorRoutingPriority && rule.Protocol == 99
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: donorRoutingTable}, netlink.RT_FILTER_TABLE)
	if err != nil || !foreign || len(routes) != 1 || routes[0].Protocol != 99 {
		t.Fatalf("foreign donor-table rule or route changed: rule=%t routes=%#v error=%v", foreign, routes, err)
	}
	for _, tool := range []string{"iptables", "ip6tables"} {
		out := run(tool, "-S", "FORWARD")
		if strings.Contains(out, internetMarker) || !strings.Contains(out, "foreign-guard") {
			t.Fatalf("cleanup changed foreign rules or retained owned rules: %s", out)
		}
	}
	// Fail initialization after base-rule acquisition, then fail its immediate
	// cleanup too. Disabling participation must not lose the remaining owner.
	bin := t.TempDir()
	failure := bin + "/fail-delete"
	if err := os.WriteFile(failure, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"iptables", "ip6tables"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do\nif [ \"$arg\" = -D ] && [ -e %q ]; then exit 4; fi\ndone\nexec %q \"$@\"\n", failure, real)
		if err := os.WriteFile(bin+"/"+tool, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	credentials := controlTestCredentials(t)(controlTestTenant, controlTestClient)
	// Its ULA is deliberately absent, so lease-listener creation fails.
	controller, initErr := NewLinux(ctx, credentials, &fakeNode{})
	if initErr == nil || controller == nil {
		t.Fatalf("failed setup lost its cleanup owner: controller=%v error=%v", controller, initErr)
	}
	if _, err := controller.Step(ctx, Config{}); err == nil {
		t.Fatal("failed initialization accepted reconciliation")
	}
	if err := controller.Close(); err == nil {
		t.Fatal("partial setup cleanup incorrectly reported success")
	}
	if err := os.Remove(failure); err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("failed setup cleanup could not recover: %v", err)
	}
	for _, tool := range []string{"iptables", "ip6tables"} {
		if strings.Contains(run(tool, "-S", "FORWARD"), internetMarker) {
			t.Fatal("failed setup retained Internet forwarding rules after recovery")
		}
	}
	for _, key := range []string{"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding"} {
		if strings.TrimSpace(run("sysctl", "-n", key)) != "1" {
			t.Fatalf("changed foreign forwarding baseline %s", key)
		}
	}
}
