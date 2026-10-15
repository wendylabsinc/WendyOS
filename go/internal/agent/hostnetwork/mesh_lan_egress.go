package hostnetwork

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// LANServiceAccess grants one isolated app one discovered physical-LAN
// endpoint. The netns route is /32; forwarding is limited to its SRV port.
type LANServiceAccess struct {
	NetNS, AppIP, Gateway, Destination, Interface, Protocol, AppBridge string
	Port                                                               uint16
}

var lanInterfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

const lanRuleMarker = "wendy-lan-service-v1"
const LANChainName = "WENDY-LAN"
const lanReplyGuardMarker = "wendy-lan-reply-guard-v1"

var lanChainMu sync.Mutex
var lanGuardMu sync.Mutex

// InitLANServiceChain owns a separate scoped chain, never the broad CNI chain.
// Hosts with a localmesh forwarding fallback also place their own tracked jump
// immediately before that fallback; ordinary forwarding uses this stable hook.
func InitLANServiceChain() error {
	lanChainMu.Lock()
	defer lanChainMu.Unlock()
	if out, err := lanIPTables("-t", "filter", "-N", LANChainName); err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("create LAN chain: %w (%s)", err, out)
	}
	return lanEnsureRule("filter", "FORWARD", "-A", []string{"-m", "comment", "--comment", lanRuleMarker, "-j", LANChainName})
}

func (a LANServiceAccess) validate() error {
	for _, value := range []string{a.AppIP, a.Gateway, a.Destination} {
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() {
			return fmt.Errorf("invalid LAN access IPv4 address %q", value)
		}
	}
	destination := net.ParseIP(a.Destination).To4()
	if destination[0] == 10 && (destination[1] == 88 || destination[1] == 99) {
		return errors.New("LAN destination overlaps Wendy mesh range")
	}
	if a.NetNS == "" || !strings.HasPrefix(a.NetNS, "/") || !lanInterfaceName.MatchString(a.Interface) ||
		(a.Protocol != "tcp" && a.Protocol != "udp") || a.Port == 0 {
		return errors.New("invalid scoped LAN service access")
	}
	return nil
}

var lanIPTables = func(args ...string) ([]byte, error) {
	return exec.Command("iptables", append([]string{"-w", "5"}, args...)...).CombinedOutput()
}

var lanIPRoute = func(netns string, args ...string) ([]byte, error) {
	return exec.Command("nsenter", append([]string{"--net=" + netns, "--", "ip", "route"}, args...)...).CombinedOutput()
}

var lanHostRoute = func(destination string) ([]byte, error) {
	return exec.Command("ip", "-4", "route", "get", destination).CombinedOutput()
}

func lanRouteUsesInterface(destination, iface string) (bool, error) {
	out, err := lanHostRoute(destination)
	if err != nil {
		return false, fmt.Errorf("inspect physical LAN route: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	for i := range fields {
		if fields[i] == "dev" && i+1 < len(fields) {
			return fields[i+1] == iface, nil
		}
	}
	return false, errors.New("physical LAN route has no interface")
}

func lanRunIPTables(args ...string) error {
	out, err := lanIPTables(args...)
	if err != nil {
		return fmt.Errorf("iptables %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func lanRuleExists(table, chain string, args []string) (bool, error) {
	out, err := lanIPTables(append([]string{"-t", table, "-C", chain}, args...)...)
	if err == nil {
		return true, nil
	}
	if code := exitCode(err); code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect LAN %s rule: %w (%s)", chain, err, strings.TrimSpace(string(out)))
}

func lanEnsureRule(table, chain, position string, args []string) error {
	exists, err := lanRuleExists(table, chain, args)
	if err != nil || exists {
		return err
	}
	cmd := []string{"-t", table, position, chain}
	if position == "-I" {
		cmd = append(cmd, "1")
	}
	return lanRunIPTables(append(cmd, args...)...)
}

func lanRemoveRule(table, chain string, args []string) error {
	exists, err := lanRuleExists(table, chain, args)
	if err != nil || !exists {
		return err
	}
	return lanRunIPTables(append([]string{"-t", table, "-D", chain}, args...)...)
}

func (a LANServiceAccess) base() []string {
	return []string{"-i", a.AppBridge, "-s", a.AppIP + "/32", "-d", a.Destination + "/32", "-o", a.Interface,
		"-m", "comment", "--comment", lanRuleMarker}
}

func (a LANServiceAccess) portRule(target string) []string {
	return append(append([]string(nil), a.base()...), "-p", a.Protocol, "--dport", fmt.Sprint(a.Port), "-j", target)
}

func (a LANServiceAccess) natRule() []string {
	// POSTROUTING cannot match an input interface. The filter grant establishes
	// app ownership before this exact tuple can reach the NAT hook.
	return []string{"-s", a.AppIP + "/32", "-d", a.Destination + "/32", "-o", a.Interface,
		"-m", "comment", "--comment", lanRuleMarker, "-p", a.Protocol, "--dport", fmt.Sprint(a.Port), "-j", "MASQUERADE"}
}

func (a LANServiceAccess) reverseRule(target string) []string {
	args := []string{"-s", a.Destination + "/32", "-d", a.AppIP + "/32", "-i", a.Interface, "-p", a.Protocol, "--sport", fmt.Sprint(a.Port), "-m", "comment", "--comment", lanRuleMarker}
	if target == "ACCEPT" {
		args = append(args, "-o", a.AppBridge, "-m", "conntrack", "--ctstate", "ESTABLISHED", "--ctdir", "REPLY")
	}
	return append(args, "-j", target)
}
func (a LANServiceAccess) replyGuard() []string {
	return []string{"-d", a.AppIP + "/32", "-i", a.Interface, "-m", "comment", "--comment", lanReplyGuardMarker, "-j", "DROP"}
}

// EnsureLANServiceAccess installs a destination DROP before the exact ACCEPT,
// NATs replies through the selected physical interface, then adds the /32
// route. A preexisting rule is never duplicated. A failed step leaves either
// no route or the destination DROP in force.
func EnsureLANServiceAccess(a LANServiceAccess) error {
	lanGuardMu.Lock()
	defer lanGuardMu.Unlock()
	if !lanInterfaceName.MatchString(a.AppBridge) || a.AppBridge == a.Interface {
		return errors.New("invalid LAN app ingress interface")
	}
	if err := a.validate(); err != nil {
		return err
	}
	uses, err := lanRouteUsesInterface(a.Destination, a.Interface)
	if err != nil {
		return err
	}
	if !uses {
		return errors.New("LAN service is not routed through its observed physical interface")
	}
	if err := AcquireForwarding(a.forwardingKey()); err != nil {
		return err
	}
	if err := InitLANServiceChain(); err != nil {
		return err
	}
	if err := lanEnsureRule("filter", LANChainName, "-I", a.replyGuard()); err != nil {
		return err
	}
	if err := lanEnsureRule("filter", LANChainName, "-I", append(a.base(), "-j", "DROP")); err != nil {
		return err
	}
	if err := lanEnsureRule("filter", LANChainName, "-I", a.portRule("ACCEPT")); err != nil {
		return err
	}
	if err := lanEnsureRule("nat", "POSTROUTING", "-A", a.natRule()); err != nil {
		return err
	}
	if out, err := lanIPRoute(a.NetNS, "replace", a.Destination+"/32", "via", a.Gateway); err != nil {
		return fmt.Errorf("install LAN /32 route: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if err := lanEnsureRule("filter", LANChainName, "-I", a.reverseRule("ACCEPT")); err != nil {
		return err
	}
	// A previous withdrawal may have left a fail-closed exact-port deny.
	if err := lanRemoveRule("filter", LANChainName, a.portRule("DROP")); err != nil {
		return err
	}
	return lanRemoveRule("filter", LANChainName, a.reverseRule("DROP"))
}

// WithdrawLANServiceAccess blocks the exact port before deleting its allow
// rule, so an xtables deletion failure cannot leave an expired service open.
// The caller retains the destination DROP and route while sibling services
// still use this address.
func WithdrawLANServiceAccess(a LANServiceAccess) error {
	lanGuardMu.Lock()
	defer lanGuardMu.Unlock()
	if !lanInterfaceName.MatchString(a.AppBridge) || a.AppBridge == a.Interface {
		return errors.New("invalid LAN app ingress interface")
	}
	if err := a.validate(); err != nil {
		return err
	}
	if err := lanEnsureRule("filter", LANChainName, "-I", a.portRule("DROP")); err != nil {
		return err
	}
	if err := lanEnsureRule("filter", LANChainName, "-I", a.reverseRule("DROP")); err != nil {
		return err
	}
	if err := lanRemoveRule("filter", LANChainName, a.reverseRule("ACCEPT")); err != nil {
		return err
	}
	if err := lanRemoveRule("filter", LANChainName, a.portRule("ACCEPT")); err != nil {
		return err
	}
	if err := lanRemoveRule("nat", "POSTROUTING", a.natRule()); err != nil {
		return err
	}
	if err := lanRemoveRule("filter", LANChainName, a.portRule("DROP")); err != nil {
		return err
	}
	if err := lanRemoveRule("filter", LANChainName, a.reverseRule("DROP")); err != nil {
		return err
	}
	return ReleaseForwarding(a.forwardingKey())
}

// WithdrawLANDestination removes the route first and then its default DROP.
// This is called only after the last service on this destination is gone.
func WithdrawLANDestination(a LANServiceAccess) error {
	if err := a.validate(); err != nil {
		return err
	}
	if err := WithdrawLANRoute(a); err != nil {
		return err
	}
	return WithdrawLANDrop(a)
}

func WithdrawLANRoute(a LANServiceAccess) error {
	if err := a.validate(); err != nil {
		return err
	}
	out, err := lanIPRoute(a.NetNS, "del", a.Destination+"/32", "via", a.Gateway)
	if err != nil && !strings.Contains(string(out), "No such process") {
		return fmt.Errorf("remove LAN /32 route: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func WithdrawLANDrop(a LANServiceAccess) error {
	if err := a.validate(); err != nil {
		return err
	}
	return lanRemoveRule("filter", LANChainName, append(a.base(), "-j", "DROP"))
}

// RevokeOrphanLANServiceAllows runs before inherited app tasks are resumed.
// App-netns /32 routes can survive an agent crash, so keep destination DROP
// guards, but remove all grants/NAT from the previous process. A new bridge
// reauthorizes only services still live in the physical-LAN cache.
func RevokeOrphanLANServiceAllows() error {
	lanGuardMu.Lock()
	defer lanGuardMu.Unlock()
	if err := InitLANServiceChain(); err != nil {
		return err
	}
	for _, item := range []struct{ table, chain, target string }{
		{"filter", MeshChainName, "ACCEPT"},
		{"filter", LANChainName, "ACCEPT"},
		{"nat", "POSTROUTING", "MASQUERADE"},
	} {
		out, err := lanIPTables("-t", item.table, "-S", item.chain)
		if err != nil {
			return fmt.Errorf("inspect owned LAN %s rules: %w (%s)", item.chain, err, strings.TrimSpace(string(out)))
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[0] != "-A" || fields[1] != item.chain ||
				!slices.Contains(fields, "--comment") || !slices.Contains(fields, item.target) {
				continue
			}
			marker := false
			for _, field := range fields {
				if strings.Trim(field, `"`) == lanRuleMarker {
					marker = true
				}
			}
			if !marker {
				continue
			}
			for i := range fields {
				fields[i] = strings.Trim(fields[i], `"`)
			}
			if err := lanRunIPTables(append([]string{"-t", item.table, "-D", item.chain}, fields[2:]...)...); err != nil {
				return err
			}
		}
	}
	return reconcileUnusedForwarding()
}

// RevokeOrphanLANAppRoutes removes retained /32 routes before removing their
// destination guards for a verified live app namespace. It handles both an
// inherited task after agent restart and a new task reusing the same CNI IP.
func RevokeOrphanLANAppRoutes(netns, appIP, gateway string) error {
	if netns == "" || net.ParseIP(appIP).To4() == nil || net.ParseIP(gateway).To4() == nil {
		return errors.New("invalid inherited LAN app network")
	}
	var out []byte
	for _, chain := range []string{MeshChainName, LANChainName} {
		part, err := lanIPTables("-t", "filter", "-S", chain)
		if err != nil {
			return fmt.Errorf("inspect inherited LAN guards: %w (%s)", err, part)
		}
		out = append(out, part...)
		out = append(out, '\n')
	}
	type ownedRule struct {
		args   []string
		chain  string
		access LANServiceAccess
	}
	var guards []ownedRule
	routes := make(map[string]LANServiceAccess)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "-A" || (fields[1] != MeshChainName && fields[1] != LANChainName) {
			continue
		}
		for i := range fields {
			fields[i] = strings.Trim(fields[i], `"`)
		}
		values := make(map[string]string)
		for i := 2; i+1 < len(fields); i++ {
			if strings.HasPrefix(fields[i], "-") {
				values[fields[i]] = fields[i+1]
			}
		}
		if values["--comment"] != lanRuleMarker || values["-s"] != appIP+"/32" || values["-j"] != "DROP" {
			continue
		}
		destination := strings.TrimSuffix(values["-d"], "/32")
		a := LANServiceAccess{NetNS: netns, AppIP: appIP, Gateway: gateway,
			Destination: destination, Interface: values["-o"], Protocol: "tcp", Port: 1}
		if err := a.validate(); err != nil {
			return err
		}
		guards = append(guards, ownedRule{args: append([]string(nil), fields[2:]...), chain: fields[1], access: a})
		routes[destination] = a
	}
	for _, a := range routes {
		if err := WithdrawLANRoute(a); err != nil {
			return err // retain guards until the route is confirmed gone
		}
	}
	for _, rule := range guards {
		if err := lanRunIPTables(append([]string{"-t", "filter", "-D", rule.chain}, rule.args...)...); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileLANReplyGuards removes historical app guards only after a complete
// current container inventory proves that no mesh container can own their IP.
// The callback runs under the guard lock: a concurrent newly created app cannot
// install a guard between this inventory and deletion. Taskless/restarting mesh
// containers remain owners; ordinary catalog/provider restarts never call this
// with an empty guessed inventory. Errors retain every guard.
func ReconcileLANReplyGuards(inventory func() ([]string, error)) error {
	lanGuardMu.Lock()
	defer lanGuardMu.Unlock()
	out, err := lanIPTables("-t", "filter", "-S", LANChainName)
	if err != nil {
		return fmt.Errorf("inspect LAN reply guards: %w (%s)", err, out)
	}
	type guard struct {
		args []string
		ip   net.IP
	}
	var guards []guard
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "-A" || fields[1] != LANChainName {
			continue
		}
		for i := range fields {
			fields[i] = strings.Trim(fields[i], `"`)
		}
		values := map[string]string{}
		for i := 2; i+1 < len(fields); i++ {
			if strings.HasPrefix(fields[i], "-") {
				values[fields[i]] = fields[i+1]
			}
		}
		if values["--comment"] != lanReplyGuardMarker || values["-j"] != "DROP" {
			continue
		}
		ip := net.ParseIP(strings.TrimSuffix(values["-d"], "/32"))
		if ip == nil || ip.To4() == nil || !strings.HasSuffix(values["-d"], "/32") {
			return errors.New("malformed owned LAN reply guard")
		}
		guards = append(guards, guard{fields[2:], ip})
	}
	if len(guards) == 0 {
		return nil
	}
	cidrs, err := inventory()
	if err != nil {
		return err
	}
	var networks []*net.IPNet
	for _, cidr := range cidrs {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil || ip.To4() == nil {
			return errors.New("invalid live mesh app subnet inventory")
		}
		networks = append(networks, network)
	}
	for _, g := range guards {
		live := false
		for _, network := range networks {
			if network.Contains(g.ip) {
				live = true
				break
			}
		}
		if live {
			continue
		}
		if err := lanRunIPTables(append([]string{"-t", "filter", "-D", LANChainName}, g.args...)...); err != nil {
			return err
		}
	}
	return nil
}
