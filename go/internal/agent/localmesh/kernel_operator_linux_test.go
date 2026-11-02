//go:build linux

package localmesh

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"golang.org/x/sys/unix"
)

func operatorPolicyTestRule(legacy bool) *netlink.Rule {
	r := netlink.NewRule()
	r.Family, r.Table, r.Priority = netlink.FAMILY_V6, operatorRouteTable, operatorRulePriority
	r.Protocol = routeProtocol
	if legacy {
		r.Table, r.Priority, r.Protocol = RouteTable, RouteTable, 0
	}
	r.Dst = ipNet(meshoperator.Prefix)
	return r
}

func TestOperatorPolicyCleanupMigration(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			r := operatorPolicyTestRule(legacy)
			ops := emptyKernelCleanupOps(t)
			ops.rules = func(int) ([]netlink.Rule, error) { return []netlink.Rule{*r}, nil }
			ops.routes = func(family int, filter *netlink.Route, _ uint64) ([]netlink.Route, error) {
				if family != netlink.FAMILY_V6 {
					return nil, nil
				}
				return []netlink.Route{{Table: filter.Table, Protocol: routeProtocol}}, nil
			}
			if err := validateStaleKernelOwnershipWith(ops.links, ops.rules, ops.routes); err != nil {
				t.Fatal(err)
			}
			var deletedRules, deletedRoutes int
			ops.deleteRule = func(actual *netlink.Rule) error {
				if actual.Table != r.Table || actual.Priority != r.Priority {
					t.Fatalf("wrong rule reclaimed: %v", actual)
				}
				deletedRules++
				return nil
			}
			ops.deleteRoute = func(*netlink.Route) error { deletedRoutes++; return nil }
			if err := reclaimReservedStateWith(ops, true); err != nil {
				t.Fatal(err)
			}
			if deletedRules != 1 || deletedRoutes != 2 {
				t.Fatalf("cleanup did not reclaim both tables: rules=%d routes=%d", deletedRules, deletedRoutes)
			}
			if baseRuleMatches(*r) {
				t.Fatal("operator rule satisfied the device base invariant")
			}
		})
	}
}

func TestOperatorPolicyCleanupPreservesForeignState(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, change := range []struct {
			name   string
			mutate func(*netlink.Rule)
		}{
			{"mark", func(r *netlink.Rule) { r.Mark = 1 }},
			{"source", func(r *netlink.Rule) { r.Src = ipNet(netip.MustParsePrefix("2001:db8::/64")) }},
			{"input", func(r *netlink.Rule) { r.IifName = "eth0" }},
			{"destination", func(r *netlink.Rule) { r.Dst = ipNet(netip.MustParsePrefix("fd45:b891:5680::/49")) }},
			{"priority", func(r *netlink.Rule) { r.Priority++ }},
			{"table", func(r *netlink.Rule) { r.Table++ }},
			{"family", func(r *netlink.Rule) { r.Family = netlink.FAMILY_V4 }},
		} {
			t.Run(fmt.Sprintf("legacy=%v/%s", legacy, change.name), func(t *testing.T) {
				r := operatorPolicyTestRule(legacy)
				change.mutate(r)
				ops := emptyKernelCleanupOps(t)
				ops.rules = func(int) ([]netlink.Rule, error) { return []netlink.Rule{*r}, nil }
				for _, err := range []error{
					validateStaleKernelOwnershipWith(ops.links, ops.rules, ops.routes),
					reclaimReservedStateWith(ops, true),
				} {
					if err == nil {
						t.Fatal("foreign operator rule accepted")
					}
					for _, detail := range []string{"priority=" + strconv.Itoa(r.Priority), "table=" + strconv.Itoa(r.Table), r.Dst.String()} {
						if !strings.Contains(err.Error(), detail) {
							t.Fatalf("conflict omitted %q: %v", detail, err)
						}
					}
				}
			})
		}
	}
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("protocol/legacy=%v", legacy), func(t *testing.T) {
			r := operatorPolicyTestRule(legacy)
			r.Protocol++
			if ownedKernelRule(*r) {
				t.Fatal("adopted an operator rule with foreign protocol")
			}
		})
	}
	t.Run("route-in-operator-table", func(t *testing.T) {
		ops := emptyKernelCleanupOps(t)
		ops.routes = func(family int, filter *netlink.Route, _ uint64) ([]netlink.Route, error) {
			if family == netlink.FAMILY_V6 && filter.Table == operatorRouteTable {
				return []netlink.Route{{Table: operatorRouteTable, Protocol: 99}}, nil
			}
			return nil, nil
		}
		if err := validateStaleKernelOwnershipWith(ops.links, ops.rules, ops.routes); err == nil {
			t.Fatal("reset accepted a foreign operator route")
		}
		if err := reclaimReservedStateWith(ops, false); err == nil {
			t.Fatal("startup accepted a foreign operator route")
		}
	})
}

// Exercise migration, both FIBs, default-route reconciliation, withdrawal and
// complete cleanup against the real kernel in a disposable namespace.
func TestKernelOperatorPolicyIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	legacy := operatorPolicyTestRule(true)
	if err := netlink.RuleAdd(legacy); err != nil {
		t.Fatal(err)
	}
	defer netlink.RuleDel(legacy)
	self, peer := testDevices(t)
	k, err := NewKernel(self)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if k != nil {
			_ = k.Close()
		}
	}()
	rules, err := netlink.RuleList(netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	operatorFound := false
	for _, r := range rules {
		if legacyOperatorRuleMatches(r) {
			t.Fatal("legacy operator rule survived migration")
		}
		if operatorRuleMatches(r) {
			operatorFound = true
		}
	}
	if !operatorFound {
		t.Fatal("dedicated operator policy absent")
	}
	// A surviving operator rule must not satisfy a missing device rule.
	if err := netlink.RuleDel(k.rule); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatal(err)
	}
	// Give the host a working ordinary IPv6 default, so a missing operator
	// guard would leak to it rather than merely failing for lack of an uplink.
	uplink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "wlmouplink"}}
	if err := netlink.LinkAdd(uplink); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(uplink)
	if err := netlink.AddrAdd(uplink, &netlink.Addr{IPNet: ipNet(netip.MustParsePrefix("2001:db8:ffff::1/128")), Flags: unix.IFA_F_NODAD}); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(uplink); err != nil {
		t.Fatal(err)
	}
	hostDefault := netlink.Route{Table: unix.RT_TABLE_MAIN, LinkIndex: uplink.Index, Dst: ipNet(netip.MustParsePrefix("::/0"))}
	if err := netlink.RouteAdd(&hostDefault); err != nil {
		t.Fatal(err)
	}
	if got, err := netlink.RouteGet(netip.MustParseAddr("2001:db8::1").AsSlice()); err != nil || len(got) != 1 || got[0].LinkIndex != uplink.Index {
		t.Fatalf("ordinary host fallback unavailable: %v, %v", got, err)
	}
	if _, err := k.AddPeer(1, peer); err != nil {
		t.Fatal(err)
	}
	_, nextHop, err := meshid.LinkLocalAddresses(self, peer)
	if err != nil {
		t.Fatal(err)
	}
	dst := netip.MustParsePrefix("fd45:b891:5680::1234/128")
	route := babel.Route{Prefix: dst, Link: 1, NextHop: nextHop, Metric: 256}
	checkTables := func(want bool) {
		t.Helper()
		for _, table := range []int{RouteTable, operatorRouteTable} {
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range routes {
				if r.Dst != nil && r.Dst.String() == dst.String() {
					found = true
				}
			}
			if found != want {
				t.Fatalf("table %d: route present=%v, want=%v: %v", table, found, want, routes)
			}
		}
	}
	if err := k.Apply([]babel.Route{route}); err != nil {
		t.Fatal(err)
	}
	checkTables(true)
	got, err := netlink.RouteGet(dst.Addr().AsSlice())
	if err != nil || len(got) != 1 || got[0].Table != operatorRouteTable || got[0].LinkIndex != k.peers[1].link.Index {
		t.Fatalf("host operator lookup did not use its own table: %v, %v", got, err)
	}
	// Both defaults must be recognized as learned state, not mistaken for
	// guards. Removing operator base state must restore only its exact rules.
	defaultRoute := route
	defaultRoute.Prefix = netip.MustParsePrefix("::/0")
	if err := k.Apply([]babel.Route{route, defaultRoute}); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteDel(&k.routes[1]); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RuleDel(k.operatorRule); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatal(err)
	}
	if err := k.Apply(nil); err != nil {
		t.Fatal(err)
	}
	checkTables(false)
	if _, err := netlink.RouteGet(dst.Addr().AsSlice()); err == nil {
		t.Fatal("operator withdrawal fell through to another table")
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	// Avoid closing twice: the explicit close above verifies its errors.
	k = nil
	if err := validateStaleKernelOwnership(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []int{RouteTable, operatorRouteTable} {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil || len(routes) != 0 {
			t.Fatalf("close retained routes in table %d: %v, %v", table, routes, err)
		}
	}
	rules, err = netlink.RuleList(netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if reservedKernelRule(r) {
			t.Fatalf("close retained rule: %v", r)
		}
	}
}

// The baseline is a test binary compiled from an unmodified pre-operator main
// checkout. Running its real NewKernel/CleanupStaleKernel code after SIGKILL
// catches table conflicts as well as priority conflicts without reimplementing
// that older agent's ownership rules here.
func TestKernelOperatorWarmRollbackIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	baseline := os.Getenv("WENDY_LOCALMESH_ROLLBACK_BINARY")
	if baseline == "" {
		t.Skip("set WENDY_LOCALMESH_ROLLBACK_BINARY to an unmodified pre-operator main test binary")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"recover", "reset"} {
		t.Run(mode, func(t *testing.T) {
			ns := fmt.Sprintf("wlmo%d%s", os.Getpid(), mode)
			if out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput(); err != nil {
				t.Fatalf("create namespace: %v: %s", err, out)
			}
			defer exec.Command("ip", "netns", "del", ns).Run()
			run := func(binary, mode string) (string, error) {
				cmd := exec.Command("ip", "netns", "exec", ns, binary, "-test.run", "^TestKernelProcessLifecycle$", "-test.v")
				cmd.Env = append(os.Environ(), "WENDY_LOCALMESH_KERNEL_TEST_MODE="+mode)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			if out, err := run(binary, "abandon"); err == nil || !strings.Contains(out, "ABANDONED") {
				t.Fatalf("WN5 did not leave crash state: %v: %s", err, out)
			}
			out, err := run(baseline, mode)
			marker := "RECLAIMED"
			if mode == "reset" {
				marker = "RESET CLEANED"
			}
			if err != nil || !strings.Contains(out, marker) {
				t.Fatalf("pre-operator main %s did not complete over WN5 crash state: %v: %s", mode, err, out)
			}
			t.Logf("pre-operator main %s: %s", mode, out)
			// The old agent ignores the dedicated operator table. Confirm its
			// fail-closed guard survives, then let WN5 reclaim the whole state.
			outBytes, err := exec.Command("ip", "-n", ns, "-6", "route", "show", "table", strconv.Itoa(operatorRouteTable)).CombinedOutput()
			if err != nil || !strings.Contains(string(outBytes), "unreachable default") {
				t.Fatalf("rollback lost operator guard: %v: %s", err, outBytes)
			}
			if out, err := run(binary, "reset"); err != nil {
				t.Fatalf("WN5 cleanup after main rollback: %v: %s", err, out)
			}
		})
	}
}
