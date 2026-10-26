//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Subprocess output is inspected while peers are still running. Keep the
// buffer private so io.Copy cannot bypass Write through an embedded ReadFrom.
type lifecycleProcessOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lifecycleProcessOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lifecycleProcessOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lifecycleTestConfig configures one re-executed node process for the route
// lifecycle suite. StopFile is the parent's lever for an orderly shutdown: the
// process watches for it instead of being killed.
type lifecycleTestConfig struct {
	Device, Peer      string
	Cert, Chain, Key  string
	Dir               string
	Underlay, Address string
	StopFile          string
}

// TestMeshLifecyclePeerProcess runs a real node until the parent asks it to
// stop. It keeps re-establishing the link, so a restarted peer is recovered by
// the same code an agent uses.
func TestMeshLifecyclePeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_LOCALMESH_LIFECYCLE_CONFIG")
	if path == "" {
		t.Skip("isolated test subprocess")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config lifecycleTestConfig
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(config.Cert, config.Chain, config.Key)
	if err != nil {
		t.Fatal(err)
	}
	self, peer := credentials.Device, testDevice(t, config.Peer)
	if self.Principal() == peer.Principal() {
		t.Fatal("test config names itself as the peer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	node, err := NewNode(ctx, config.Dir, credentials, "test", 50052)
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- node.Run() }()
	tlsConfig, err := credentials.PeerTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	if meshid.Compare(self, peer) > 0 {
		listener, err := quic.ListenAddr(config.Underlay, tlsConfig, QUICConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept(ctx)
				if err != nil {
					return
				}
				go node.Attach(ctx, peer, conn)
			}
		}()
	} else {
		go func() {
			for ctx.Err() == nil {
				attempt, cancelAttempt := context.WithTimeout(ctx, 2*time.Second)
				conn, err := quic.DialAddr(attempt, config.Address, tlsConfig, QUICConfig())
				cancelAttempt()
				if err != nil {
					continue
				}
				if err = node.Attach(ctx, peer, conn); err != nil {
					return
				}
			}
		}()
	}
	checked := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !checked && node.Snapshot().Peers > 0 {
			assertDerivedLinkLocals(t, self, peer)
			checked = true
		}
		if _, err = os.Stat(config.StopFile); err == nil {
			cancel()
			<-runDone
			t.Log("stopped cleanly")
			return
		}
		if ctx.Err() != nil {
			t.Fatal("lifecycle window expired")
		}
		<-ticker.C
	}
}

// TestMeshRouteLifecycleIsolatedNamespaces drives the whole IPv6 route lifecycle
// in two namespaces on a veth underlay: install, verify the mesh table answers
// for pool addresses instead of the decoy uplink default, withdraw when the
// peer stops, and recover when it comes back.
func TestMeshRouteLifecycleIsolatedNamespaces(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	if os.Getenv("WENDY_LOCALMESH_LIFECYCLE_CONFIG") != "" {
		t.Skip("parent test only")
	}
	names := []string{fmt.Sprintf("wlma%d", os.Getpid()), fmt.Sprintf("wlmb%d", os.Getpid())}
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
	}
	for _, name := range names {
		run("netns", "add", name)
		defer exec.Command("ip", "netns", "del", name).Run()
	}
	run("link", "add", "wlmtesta", "type", "veth", "peer", "name", "wlmtestb")
	run("link", "set", "wlmtesta", "netns", names[0])
	run("link", "set", "wlmtestb", "netns", names[1])
	underlay := []string{"wlmtesta", "wlmtestb"}
	for i, name := range names {
		run("-n", name, "addr", "add", fmt.Sprintf("10.200.0.%d/30", i+1), "dev", underlay[i])
		run("-n", name, "link", "set", underlay[i], "up")
		run("-n", name, "link", "set", "lo", "up")
		// A decoy uplink in every namespace. A pool address that the mesh table
		// stops answering must fail closed, never drift onto this route.
		run("-n", name, "-6", "route", "add", "default", "dev", underlay[i])
	}
	dir := t.TempDir()
	lower := testPrincipal(testTenantUUID, testDeviceAID)
	higher := testPrincipal(testTenantUUID, testDeviceBID)
	configs, stops := map[string]string{}, map[string]string{}
	testCredentialsWithPEM(t, func(device meshid.Device, cert, chain, key string) {
		peer, underlayAddr, address := higher, "10.200.0.2:18500", "10.200.0.2:18500"
		if device.Principal() == higher {
			peer, underlayAddr, address = lower, "10.200.0.2:18500", "10.200.0.1:18500"
		}
		cfg := lifecycleTestConfig{Device: device.Principal(), Peer: peer, Cert: cert, Chain: chain, Key: key,
			Dir: filepath.Join(dir, device.UUID().String()), Underlay: underlayAddr, Address: address,
			StopFile: filepath.Join(dir, device.UUID().String()+".stop")}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, device.UUID().String()+".json")
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		configs[device.Principal()] = path
		stops[device.Principal()] = cfg.StopFile
	})
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var procs []*exec.Cmd
	outputs := map[string]*lifecycleProcessOutput{}
	start := func(principal, namespace string) {
		t.Helper()
		buf := &lifecycleProcessOutput{}
		outputs[principal] = buf
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", namespace, binary, "-test.run", "^TestMeshLifecyclePeerProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_LOCALMESH_LIFECYCLE_CONFIG="+configs[principal])
		cmd.Stdout, cmd.Stderr = buf, buf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, cmd)
	}
	stop := func(principal string) {
		t.Helper()
		if err := os.WriteFile(stops[principal], nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, path := range stops {
			_ = os.WriteFile(path, nil, 0o600)
		}
		for _, p := range procs {
			_ = p.Wait()
		}
	}()
	dump := func(what string) {
		t.Helper()
		for principal, buf := range outputs {
			t.Logf("%s [%s]:\n%s", what, principal, buf.String())
		}
	}
	meshLower := netip.PrefixFrom(testDevice(t, lower).MeshAddress(), 128).Addr()
	meshHigher := netip.PrefixFrom(testDevice(t, higher).MeshAddress(), 128).Addr()
	routeGet := func(namespace string, addr netip.Addr) (string, error) {
		out, err := exec.Command("ip", "-n", namespace, "-6", "route", "get", addr.String()).CombinedOutput()
		return string(out), err
	}
	waitForMeshRoute := func(namespace string, addr netip.Addr) string {
		t.Helper()
		deadline := time.Now().Add(40 * time.Second)
		var last string
		var lastErr error
		for time.Now().Before(deadline) {
			last, lastErr = routeGet(namespace, addr)
			if lastErr == nil && strings.Contains(last, "dev wlmp") && !strings.Contains(last, "dev wlmtest") {
				return last
			}
			time.Sleep(250 * time.Millisecond)
		}
		dump("waiting for a wendy net route")
		t.Fatalf("%s never became a mesh-table route in %s (err=%v): %s", addr, namespace, lastErr, last)
		return ""
	}
	waitForUnreachable := func(namespace string, addr netip.Addr) string {
		t.Helper()
		deadline := time.Now().Add(40 * time.Second)
		var last string
		var lastErr error
		for time.Now().Before(deadline) {
			last, lastErr = routeGet(namespace, addr)
			// An RTN_UNREACHABLE route answers the lookup with EHOSTUNREACH, which
			// iproute2 prints as "No route to host" instead of a route line.
			if lastErr != nil && (strings.Contains(last, "unreachable") || strings.Contains(last, "No route to host")) {
				return last
			}
			time.Sleep(250 * time.Millisecond)
		}
		dump("waiting for an unreachable pool address")
		t.Fatalf("%s is still routable in %s (err=%v): %s", addr, namespace, lastErr, last)
		return ""
	}
	showTable := func(namespace string) string {
		t.Helper()
		out, err := exec.Command("ip", "-n", namespace, "-6", "route", "show", "table", strconv.Itoa(RouteTable)).CombinedOutput()
		if err != nil {
			t.Fatalf("ip route show table %d in %s: %v %s", RouteTable, namespace, err, out)
		}
		return string(out)
	}

	start(higher, names[1])
	start(lower, names[0])

	// Install: both directions resolve through the mesh table with a link-local
	// next hop on a TUN, never through the decoy uplink.
	viaA := waitForMeshRoute(names[0], meshHigher)
	viaB := waitForMeshRoute(names[1], meshLower)
	t.Logf("route to %s: %s", meshHigher, strings.TrimSpace(viaA))
	t.Logf("route to %s: %s", meshLower, strings.TrimSpace(viaB))

	// The pool prefix itself is never installed as reachability, and the
	// unreachable fallback is present in every mesh table.
	for _, namespace := range names {
		table := showTable(namespace)
		if strings.Contains(table, "/48") {
			t.Fatalf("%s installs the wendy net pool as a route:\n%s", namespace, table)
		}
		if !strings.Contains(table, "unreachable") {
			t.Fatalf("%s has no unreachable fallback in table %d:\n%s", namespace, RouteTable, table)
		}
	}
	// Each namespace carries its own ULA on the host link and the peer's ULA as
	// a mesh-table route. The instance's own /128 is an address plus a local
	// route, never something this package installs into the mesh table.
	for namespace, own := range map[string]netip.Addr{names[0]: meshLower, names[1]: meshHigher} {
		out, err := exec.Command("ip", "-n", namespace, "-6", "addr", "show", "dev", InterfaceName).CombinedOutput()
		if err != nil {
			t.Fatalf("ip -6 addr show dev %s in %s: %v %s", InterfaceName, namespace, err, out)
		}
		if !strings.Contains(string(out), own.String()) {
			t.Fatalf("%s does not carry its own wendy net address %s:\n%s", namespace, own, out)
		}
	}
	for namespace, peer := range map[string]netip.Addr{names[0]: meshHigher, names[1]: meshLower} {
		table := showTable(namespace)
		if !strings.Contains(table, peer.String()) {
			t.Fatalf("%s is missing the learnt /128 route for %s:\n%s", namespace, peer, table)
		}
	}

	// Withdraw: the peer stops cleanly, so its routes are withdrawn and the pool
	// address becomes unreachable rather than falling onto the decoy uplink.
	stop(lower)
	if err := procs[len(procs)-1].Wait(); err != nil {
		dump("client shutdown")
		t.Fatalf("client did not stop cleanly: %v", err)
	}
	t.Logf("withdrawal: %s", strings.TrimSpace(waitForUnreachable(names[1], meshLower)))

	// Cleanup: an orderly exit leaves no rule, no table entry and no interface.
	if table := showTable(names[0]); strings.TrimSpace(table) != "" {
		t.Fatalf("the stopped agent left routes behind in table %d:\n%s", RouteTable, table)
	}
	rules, err := exec.Command("ip", "-n", names[0], "-6", "rule", "show").CombinedOutput()
	if err != nil {
		t.Fatalf("ip rule show: %v %s", err, rules)
	}
	if strings.Contains(string(rules), strconv.Itoa(RouteTable)) {
		t.Fatalf("the stopped agent left a wendy net rule behind:\n%s", rules)
	}

	// Recovery: the peer comes back in the same namespace and the route returns.
	if err = os.Remove(stops[lower]); err != nil {
		t.Fatal(err)
	}
	start(lower, names[0])
	t.Logf("recovered: %s", strings.TrimSpace(waitForMeshRoute(names[1], meshLower)))
	waitForMeshRoute(names[0], meshHigher)
	stop(higher)
	stop(lower)
	dump("final")
}

// TestKernelProcessLifecycle builds mesh kernel state and then either dies
// without cleanup (an agent crash), restarts over that state, or meets a
// foreign route in the reserved table.
func TestKernelProcessLifecycle(t *testing.T) {
	mode := os.Getenv("WENDY_LOCALMESH_KERNEL_TEST_MODE")
	if mode == "" {
		t.Skip("isolated test subprocess")
	}
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	peer := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	switch mode {
	case "abandon":
		kernel, err := NewKernel(device)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = kernel.AddPeer(1, peer); err != nil {
			t.Fatal(err)
		}
		if err = kernel.Apply([]babel.Route{{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: 1, Metric: 0, Local: true}}); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ABANDONED")
		// No Stop, no Close: exactly what an abrupt exit leaves in the kernel.
		if err = syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		select {}
	case "recover":
		kernel, err := NewKernel(device)
		if err != nil {
			t.Fatalf("restart over its own leftover state: %v", err)
		}
		defer kernel.Close()
		if _, err = kernel.AddPeer(1, peer); err != nil {
			t.Fatal(err)
		}
		assertDerivedLinkLocals(t, device, peer)
		fmt.Println("RECLAIMED")
	case "reset":
		if err := CleanupStaleKernel(); err != nil {
			t.Fatalf("unenrolled startup cleanup: %v", err)
		}
		links, err := netlink.LinkList()
		if err != nil {
			t.Fatal(err)
		}
		for _, link := range links {
			if link.Attrs().Name == InterfaceName || strings.HasPrefix(link.Attrs().Name, peerInterfacePrefix) {
				t.Fatalf("reset retained mesh interface %s", link.Attrs().Name)
			}
		}
		rules, err := netlink.RuleList(netlink.FAMILY_ALL)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range rules {
			if rule.Table == RouteTable {
				t.Fatalf("reset retained mesh rule: %#v", rule)
			}
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
		if err != nil || len(routes) != 0 {
			t.Fatalf("reset retained mesh routes: %v, %v", routes, err)
		}
		if meshForwardingCommand("-C") == nil {
			t.Fatal("reset retained transit forwarding grant")
		}
		fmt.Println("RESET CLEANED")
	case "foreign":
		foreign := netlink.Route{Table: RouteTable, Family: netlink.FAMILY_V6, Protocol: 187, Type: unix.RTN_UNREACHABLE, Dst: ipNet(netip.MustParsePrefix("fd45:b891:567f::1/128"))}
		if err := netlink.RouteAdd(&foreign); err != nil {
			t.Fatal(err)
		}
		if _, err := NewKernel(device); err == nil {
			t.Fatal("NewKernel adopted a route table it does not own")
		} else {
			fmt.Println("REFUSED", err)
		}
		if err := CleanupStaleKernel(); err == nil {
			t.Fatal("reset cleanup adopted a foreign route")
		}
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}

// TestKernelCrashRecoveryIsolatedNamespace proves an agent restart reclaims the
// dummy link, rules and routes its crashed predecessor left, while a route it
// never owned stays a hard conflict.
func TestKernelCrashRecoveryIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	if os.Getenv("WENDY_LOCALMESH_KERNEL_TEST_MODE") != "" {
		t.Skip("parent test only")
	}
	name := fmt.Sprintf("wlmc%d", os.Getpid())
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
	}
	run("netns", "add", name)
	defer exec.Command("ip", "netns", "del", name).Run()
	run("-n", name, "link", "set", "lo", "up")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode := func(mode string) (string, error) {
		cmd := exec.Command("ip", "netns", "exec", name, binary, "-test.run", "^TestKernelProcessLifecycle$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_LOCALMESH_KERNEL_TEST_MODE="+mode)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := mode("abandon")
	if err == nil || !strings.Contains(out, "ABANDONED") {
		t.Fatalf("the crash scenario did not die mid-life (err=%v):\n%s", err, out)
	}
	if out, err = mode("recover"); err != nil || !strings.Contains(out, "RECLAIMED") {
		t.Fatalf("a restart did not reclaim its own leftover state (err=%v):\n%s", err, out)
	}
	if out, err = mode("abandon"); err == nil || !strings.Contains(out, "ABANDONED") {
		t.Fatalf("reset crash setup did not abandon state (err=%v):\n%s", err, out)
	}
	for range 2 {
		if out, err = mode("reset"); err != nil || !strings.Contains(out, "RESET CLEANED") {
			t.Fatalf("unenrolled recovery did not clean owned state idempotently (err=%v):\n%s", err, out)
		}
	}
	if out, err = mode("foreign"); err != nil || !strings.Contains(out, "REFUSED") {
		t.Fatalf("a foreign route in the reserved table was not refused (err=%v):\n%s", err, out)
	}
}

// assertDerivedLinkLocals shows that a mesh interface carries exactly the
// link-local this package derived from the device UUIDs. addr_gen_mode is set
// to none before any address is added, so the kernel never manufactures a
// MAC-derived link-local beside it, and the host mesh link carries no
// link-local at all - only the device's own ULA.
func assertDerivedLinkLocals(t *testing.T, self, peer meshid.Device) {
	t.Helper()
	want, _, err := meshid.LinkLocalAddresses(self, peer)
	if err != nil {
		t.Fatal(err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	tunnels := 0
	for _, l := range links {
		name := l.Attrs().Name
		if name != InterfaceName && !strings.HasPrefix(name, peerInterfacePrefix) {
			continue
		}
		addrs, err := netlink.AddrList(l, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		var derived, own int
		for _, a := range addrs {
			addr, ok := netip.AddrFromSlice(a.IP)
			if !ok {
				t.Fatalf("%s carries an unparseable address %s", name, a.IP)
			}
			addr = addr.Unmap()
			switch {
			case addr.IsLinkLocalUnicast():
				derived++
				if addr != want {
					t.Fatalf("%s carries the link-local %s, want the derived %s", name, addr, want)
				}
			case addr == self.MeshAddress():
				own++
			default:
				t.Fatalf("%s carries %s, which is neither the derived link-local nor the device's own wendy net address", name, addr)
			}
		}
		if name == InterfaceName {
			// The host mesh link is a routing anchor: the device's own ULA and
			// nothing else. A kernel link-local here would be a second,
			// MAC-derived identity on the same host.
			if derived != 0 || own != 1 {
				t.Fatalf("%s carries %d link-local and %d wendy net addresses, want 0 and 1", name, derived, own)
			}
			continue
		}
		tunnels++
		// A peer TUN carries the derived link-local for the link and the device's
		// own ULA as the payload source - exactly one of each.
		if derived != 1 || own != 1 {
			t.Fatalf("%s carries %d link-local and %d wendy net addresses, want 1 and 1", name, derived, own)
		}
	}
	if tunnels == 0 {
		t.Fatal("no wendy net TUN exists")
	}
}
