//go:build linux

package localmesh

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func emptyKernelCleanupOps(t *testing.T) kernelCleanupOps {
	t.Helper()
	return kernelCleanupOps{
		links:     func() ([]netlink.Link, error) { return nil, nil },
		addresses: func(netlink.Link) ([]loopbackAddress, error) { return nil, nil },
		rules:     func(int) ([]netlink.Rule, error) { return nil, nil },
		routes:    func(int, *netlink.Route, uint64) ([]netlink.Route, error) { return nil, nil },
		deleteLink: func(netlink.Link) error {
			t.Fatal("unexpected interface deletion")
			return nil
		},
		deleteAddress: func(netlink.Link, loopbackAddress) error {
			t.Fatal("unexpected address deletion")
			return nil
		},
		deleteRule: func(*netlink.Rule) error {
			t.Fatal("unexpected policy rule deletion")
			return nil
		},
		deleteRoute: func(*netlink.Route) error {
			t.Fatal("unexpected route deletion")
			return nil
		},
	}
}

func TestResetKernelCleanupDoesNotSuppressDeletionErrors(t *testing.T) {
	for _, failure := range []error{unix.EAFNOSUPPORT, unix.EOPNOTSUPP} {
		for _, operation := range []string{"link", "rule", "route"} {
			t.Run(fmt.Sprintf("%s/%s", operation, failure), func(t *testing.T) {
				ops := emptyKernelCleanupOps(t)
				deleted := false
				switch operation {
				case "link":
					ops.links = func() ([]netlink.Link, error) {
						return []netlink.Link{&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: legacyInterfaceName, Alias: interfaceOwner}}}, nil
					}
					ops.deleteLink = func(netlink.Link) error { deleted = true; return failure }
				case "rule":
					ops.rules = func(int) ([]netlink.Rule, error) {
						rule := netlink.NewRule()
						rule.Family, rule.Table, rule.Priority, rule.IifName = netlink.FAMILY_V6, RouteTable, RouteTable+1, "wlmp1"
						return []netlink.Rule{*rule}, nil
					}
					ops.deleteRule = func(*netlink.Rule) error { deleted = true; return failure }
				case "route":
					ops.routes = func(int, *netlink.Route, uint64) ([]netlink.Route, error) {
						return []netlink.Route{{Table: RouteTable, Protocol: routeProtocol}}, nil
					}
					ops.deleteRoute = func(*netlink.Route) error { deleted = true; return failure }
				}
				if err := reclaimReservedStateWith(ops, true); !deleted || !errors.Is(err, failure) {
					t.Fatalf("failed %s deletion was reported complete: attempted=%v, err=%v", operation, deleted, err)
				}
			})
		}
	}
}

func TestResetKernelCleanupToleratesOnlyUnavailableQueries(t *testing.T) {
	for _, failure := range []error{unix.EAFNOSUPPORT, unix.EOPNOTSUPP, unix.EPERM} {
		for _, operation := range []string{"rules", "routes"} {
			ops := emptyKernelCleanupOps(t)
			if operation == "rules" {
				ops.rules = func(int) ([]netlink.Rule, error) { return nil, failure }
			} else {
				ops.routes = func(int, *netlink.Route, uint64) ([]netlink.Route, error) { return nil, failure }
			}
			err := reclaimReservedStateWith(ops, true)
			if errors.Is(failure, unix.EPERM) {
				if !errors.Is(err, failure) {
					t.Fatalf("permission error was hidden for %s: %v", operation, err)
				}
			} else if err != nil {
				t.Fatalf("unavailable %s query blocked an empty host: %v", operation, err)
			}
			if err := reclaimReservedStateWith(ops, false); !errors.Is(err, failure) {
				t.Fatalf("normal kernel startup lost strict %s inspection: %v", operation, err)
			}
		}
	}
}

func TestResetKernelCleanupRechecksRuleOwnership(t *testing.T) {
	ops := emptyKernelCleanupOps(t)
	reads := 0
	ops.rules = func(int) ([]netlink.Rule, error) {
		reads++
		if reads == 1 {
			return nil, nil // Initial validation sees an empty table.
		}
		return []netlink.Rule{{Family: netlink.FAMILY_V6, Table: RouteTable, Priority: RouteTable + 9}}, nil
	}
	if err := validateStaleKernelOwnershipWith(ops.links, ops.rules, ops.routes); err != nil {
		t.Fatal(err)
	}
	if err := reclaimReservedStateWith(ops, true); err == nil {
		t.Fatal("cleanup accepted a foreign rule added after prevalidation")
	}
	// emptyKernelCleanupOps fails immediately if any deletion was attempted.
}

func TestResetKernelCleanupMissingPolicySupport(t *testing.T) {
	for _, unavailable := range []error{unix.EAFNOSUPPORT, unix.EOPNOTSUPP} {
		for _, owned := range []bool{false, true} {
			listLinks := func() ([]netlink.Link, error) {
				if owned {
					return []netlink.Link{&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: legacyInterfaceName, Alias: interfaceOwner}}}, nil
				}
				return nil, nil
			}
			for _, ruleFailure := range []bool{false, true} {
				err := validateStaleKernelOwnershipWith(listLinks, func(int) ([]netlink.Rule, error) {
					if ruleFailure {
						return nil, unavailable
					}
					return nil, nil
				}, func(int, *netlink.Route, uint64) ([]netlink.Route, error) { return nil, unavailable })
				if owned && !errors.Is(err, unavailable) {
					t.Fatalf("existing owned link with unsupported policy family did not fail closed: %v", err)
				}
				if !owned && err != nil {
					t.Fatalf("fresh host without IPv6 policy support was rejected: %v", err)
				}
			}
		}
	}
	err := validateStaleKernelOwnershipWith(func() ([]netlink.Link, error) { return nil, nil },
		func(int) ([]netlink.Rule, error) { return nil, unix.EPERM },
		func(int, *netlink.Route, uint64) ([]netlink.Route, error) { return nil, nil })
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("permission failure was treated as absent kernel support: %v", err)
	}
}

func TestResetKernelCleanupPreservesForeignRule(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable network namespace")
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: legacyInterfaceName, Alias: interfaceOwner}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(link)
	if err := netlink.LinkSetAlias(link, interfaceOwner); err != nil {
		t.Fatal(err)
	}
	foreign := netlink.NewRule()
	foreign.Family = netlink.FAMILY_V6
	foreign.Table = RouteTable
	foreign.Priority = RouteTable + 9
	if err := netlink.RuleAdd(foreign); err != nil {
		t.Fatal(err)
	}
	defer netlink.RuleDel(foreign)
	if err := CleanupStaleKernel(); err == nil {
		t.Fatal("reset cleanup accepted a foreign rule selecting the reserved table")
	}
	if _, err := netlink.LinkByName(legacyInterfaceName); err != nil {
		t.Fatalf("ownership validation mutated state before rejecting the conflict: %v", err)
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		if rule.Table == foreign.Table && rule.Priority == foreign.Priority {
			return
		}
	}
	t.Fatal("reset cleanup deleted the foreign rule")
}
