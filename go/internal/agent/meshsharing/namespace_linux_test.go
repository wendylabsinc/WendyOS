//go:build linux

package meshsharing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type namespaceIdentity struct{ Certificate, Chain, Key string }

func writeNamespaceIdentity(t *testing.T, path, root string, credentials *localmesh.Credentials) {
	t.Helper()
	identity := namespaceIdentity{Chain: root, Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: credentials.Certificate.Certificate[0]}))}
	for _, der := range credentials.Certificate.Certificate[1:] {
		identity.Chain += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	key, err := x509.MarshalPKCS8PrivateKey(credentials.Signer)
	if err != nil {
		t.Fatal(err)
	}
	identity.Key = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func namespacePolicy(t *testing.T) *linuxTunnelPolicy {
	t.Helper()
	// The namespaces share a mount namespace, including hostnetwork's persisted
	// IPv4 forwarding marker. Use an already-enabled baseline in each test netns.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("WENDY_SHARING_IDENTITY"))
	if err != nil {
		t.Fatal(err)
	}
	var identity namespaceIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	credentials, err := localmesh.NewCredentials(identity.Certificate, identity.Chain, identity.Key)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newLinuxTunnelPolicy(context.Background(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := policy.Close(); err != nil {
			t.Errorf("close policy: %v", err)
		}
	})
	return policy
}

func namespaceReady(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("WENDY_SHARING_SYNC"), name), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
}

func namespaceWait(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(os.Getenv("WENDY_SHARING_SYNC"), name)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func namespaceNoLeaseConntrack(t *testing.T) {
	t.Helper()
	for _, family := range []netlink.InetFamily{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, family)
		if err != nil {
			t.Fatal(err)
		}
		for _, flow := range flows {
			for _, raw := range []net.IP{flow.Forward.SrcIP, flow.Forward.DstIP, flow.Reverse.SrcIP, flow.Reverse.DstIP} {
				address, ok := netip.AddrFromSlice(raw)
				if ok && (netip.MustParsePrefix(leaseIPv4Pool).Contains(address.Unmap()) || netip.MustParsePrefix(leaseIPv6Pool).Contains(address)) {
					t.Fatalf("withdrawal retained conntrack state for lease address %s", address)
				}
			}
		}
	}
}

func TestSharingNamespaceAppProcess(t *testing.T) {
	if os.Getenv("WENDY_SHARING_ROLE") != "app" {
		t.Skip("namespace subprocess")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	response, err := client.Get("http://198.51.100.7:18081")
	if os.Getenv("WENDY_SHARING_APP_ALLOWED") != "1" {
		if err == nil {
			response.Body.Close()
			t.Fatal("app without Internet entitlement reached the donor uplink")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != strings.Repeat("mesh Internet\n", 4096) {
		t.Fatalf("entitled app failed across both NATs: status=%d err=%v body=%q", response.StatusCode, err, body)
	}
}

// Each endpoint runs in a subprocess: its network namespace is selected before
// Go creates threads. Both processes use the production TLS control and policy.
func TestSharingNamespaceDonorProcess(t *testing.T) {
	if os.Getenv("WENDY_SHARING_ROLE") != "donor" {
		t.Skip("namespace subprocess")
	}
	policy := namespacePolicy(t)
	policy.probeIPv6 = func(context.Context, string, string) bool { return true }
	before := map[string]string{}
	for _, path := range []string{"/proc/sys/net/ipv6/conf/all/forwarding", "/proc/sys/net/ipv6/conf/uplink1/forwarding", "/proc/sys/net/ipv6/conf/uplink1/accept_ra"} {
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(value)
	}
	if strings.TrimSpace(before["/proc/sys/net/ipv6/conf/all/forwarding"]) != "0" {
		t.Fatal("test must start with IPv6 forwarding disabled")
	}
	// A real host also has the Babel routing owner. Sharing withdrawal must
	// leave that owner's forwarding active until the final owner releases it.
	const routingOwner = "namespace-mesh-routing"
	if err := hostnetwork.AcquireIPv6Forwarding(routingOwner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hostnetwork.ReleaseIPv6Forwarding(routingOwner) })
	if err := policy.SetSharing(context.Background(), "uplink1", "10.201.0.1"); err != nil {
		t.Fatal(err)
	}
	namespaceReady(t, "donor.ready")
	commands := bufio.NewScanner(os.Stdin)
	for commands.Scan() {
		if commands.Text() != "withdraw" {
			t.Fatalf("unknown donor command %q", commands.Text())
		}
		if err := policy.SetSharing(context.Background(), "", ""); err != nil {
			t.Fatal(err)
		}
		namespaceNoLeaseConntrack(t)
		if value, err := os.ReadFile("/proc/sys/net/ipv6/conf/all/forwarding"); err != nil || strings.TrimSpace(string(value)) != "1" {
			t.Fatalf("sharing withdrawal disabled the live routing owner: %q %v", value, err)
		}
		if err := hostnetwork.ReleaseIPv6Forwarding(routingOwner); err != nil {
			t.Fatal(err)
		}
		for path, expected := range before {
			value, err := os.ReadFile(path)
			if err != nil || string(value) != expected {
				t.Errorf("withdrawal changed baseline %s: got %q, want %q: %v", path, value, expected, err)
			}
		}
		namespaceReady(t, "donor.withdrawn")
	}
	if err := commands.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestSharingNamespaceBorrowerProcess(t *testing.T) {
	if os.Getenv("WENDY_SHARING_ROLE") != "borrower" {
		t.Skip("namespace subprocess")
	}
	policy := namespacePolicy(t)
	donor, err := meshid.DeviceFromPrincipal(os.Getenv("WENDY_SHARING_DONOR"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := policy.SetGateway(ctx, donor); err != nil {
		t.Fatal(err)
	}
	cancel() // reconciliation's context must not kill the established lease.
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	for _, upstream := range []string{"198.51.100.7", "2001:db8:feed::7"} {
		response, err := client.Get("http://" + net.JoinHostPort(upstream, "18081"))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || string(body) != strings.Repeat("mesh Internet\n", 4096) {
			t.Fatalf("dual-stack NAT failed at %s: status=%d body=%q err=%v", upstream, response.StatusCode, body, err)
		}
	}
	query := new(dns.Msg)
	query.SetQuestion("mesh-sharing.test.", dns.TypeA)
	answer, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(query, net.JoinHostPort(donor.MeshAddress().String(), "53"))
	if err != nil || len(answer.Answer) != 1 || answer.Answer[0].String() != "mesh-sharing.test.\t30\tIN\tA\t198.51.100.7" {
		t.Fatalf("donor ULA DNS proxy: %+v %v", answer, err)
	}
	// These addresses are reachable through the donor's uplink, but Internet
	// sharing must not expose application VIPs or another ULA network.
	for _, forbidden := range []string{"10.99.1.1", "fd99::1"} {
		if response, err := client.Get("http://" + net.JoinHostPort(forbidden, "18081")); err == nil {
			response.Body.Close()
			t.Fatalf("reserved destination %s escaped the Internet tunnel", forbidden)
		}
	}
	namespaceReady(t, "borrower.ready")
	namespaceWait(t, "app.denied.checked")
	if err := hostnetwork.EnsureBridgeInternetAccess("namespace-internet-app", "app0", []net.IP{net.ParseIP("10.42.0.2")}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hostnetwork.RevokeBridgeInternetAccess("namespace-internet-app") })
	namespaceReady(t, "app.allowed")
	namespaceWait(t, "app.allowed.checked")
	if err := hostnetwork.RevokeBridgeInternetAccess("namespace-internet-app"); err != nil {
		t.Fatal(err)
	}
	namespaceReady(t, "app.revoked")
	namespaceWait(t, "app.revoked.checked")
	namespaceReady(t, "app.done")
	deadline := time.Now().Add(20 * time.Second)
	for {
		policy.mu.Lock()
		withdrawn := policy.borrowed == nil
		policy.mu.Unlock()
		if withdrawn {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("donor withdrawal retained borrowed tunnel/defaults")
		}
		time.Sleep(25 * time.Millisecond)
	}
	for _, family := range []string{"-4", "-6"} {
		out, err := exec.Command("ip", family, "route", "show", "default").CombinedOutput()
		if err != nil || len(bytes.TrimSpace(out)) != 0 {
			t.Fatalf("borrowed default survived withdrawal: %s %v %s", family, err, out)
		}
	}
	out, err := exec.Command("ip", "-o", "link", "show", "type", "ip6tnl").CombinedOutput()
	if err != nil || bytes.Contains(out, []byte("wlmi")) {
		t.Fatalf("borrower tunnel survived withdrawal: %v %s", err, out)
	}
	namespaceNoLeaseConntrack(t)
	t.Log("mutual TLS, two-hop IPv6 transit, NAT44/NAT66, DNS and withdrawal passed")
}

func TestSharingPolicyDualStackAcrossIPv6Namespaces(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux container with iptables, ip6tnl and dnsmasq")
	}
	for _, tool := range []string{"ip", "ping", "iptables", "ip6tables", "dnsmasq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	base := fmt.Sprintf("wmsp%d", os.Getpid())
	borrower, transit, donor, app := base+"b", base+"t", base+"d", base+"a"
	for _, ns := range []string{borrower, transit, donor, app} {
		run("netns", "add", ns)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
		run("-n", ns, "link", "set", "lo", "up")
	}
	run("-n", borrower, "link", "add", "app0", "type", "veth", "peer", "name", "eth0", "netns", app)
	run("-n", borrower, "link", "set", "app0", "up")
	run("-n", borrower, "addr", "add", "10.42.0.1/24", "dev", "app0")
	run("-n", app, "link", "set", "eth0", "up")
	run("-n", app, "addr", "add", "10.42.0.2/24", "dev", "eth0")
	run("-n", app, "route", "add", "default", "via", "10.42.0.1")
	for i, endpoint := range []string{borrower, donor} {
		mesh, hop := fmt.Sprintf("wlmp%d", i), fmt.Sprintf("hop%d", i)
		run("-n", endpoint, "link", "add", mesh, "type", "veth", "peer", "name", hop, "netns", transit)
		run("-n", endpoint, "link", "set", mesh, "mtu", "1320", "up")
		run("-n", transit, "link", "set", hop, "mtu", "1320", "up")
		run("-n", endpoint, "-6", "addr", "add", fmt.Sprintf("fd00:%d::1/64", i+1), "dev", mesh, "nodad")
		run("-n", transit, "-6", "addr", "add", fmt.Sprintf("fd00:%d::2/64", i+1), "dev", hop, "nodad")
	}
	makeCredentials, root := controlTestCredentialsWithRoot(t)
	borrowerCredentials := makeCredentials(controlTestTenant, controlTestClient)
	donorCredentials := makeCredentials(controlTestTenant, controlTestGateway)
	for i, endpoint := range []struct {
		ns          string
		credentials *localmesh.Credentials
	}{{borrower, borrowerCredentials}, {donor, donorCredentials}} {
		run("-n", endpoint.ns, "-6", "addr", "add", endpoint.credentials.Device.MeshAddress().String()+"/128", "dev", "lo", "nodad")
		run("-n", endpoint.ns, "-6", "route", "add", "fd45:b891:567f::/48", "via", fmt.Sprintf("fd00:%d::2", i+1), "dev", fmt.Sprintf("wlmp%d", i), "src", endpoint.credentials.Device.MeshAddress().String())
		run("-n", transit, "-6", "route", "add", endpoint.credentials.Device.MeshAddress().String()+"/128", "via", fmt.Sprintf("fd00:%d::1", i+1), "dev", fmt.Sprintf("hop%d", i))
	}
	run("netns", "exec", transit, "sysctl", "-w", "net.ipv6.conf.all.forwarding=1")
	rootIface := "wu" + fmt.Sprint(os.Getpid())
	run("link", "add", rootIface, "type", "veth", "peer", "name", "uplink1", "netns", donor)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", rootIface).Run() })
	run("link", "set", rootIface, "up")
	run("addr", "add", "10.201.0.1/30", "dev", rootIface)
	run("-6", "addr", "add", "2001:db8:201::1/64", "dev", rootIface, "nodad")
	run("addr", "add", "198.51.100.7/32", "dev", rootIface)
	run("-6", "addr", "add", "2001:db8:feed::7/128", "dev", rootIface, "nodad")
	run("addr", "add", "10.99.1.1/32", "dev", rootIface)
	run("-6", "addr", "add", "fd99::1/128", "dev", rootIface, "nodad")
	run("-n", donor, "link", "set", "uplink1", "up")
	run("-n", donor, "addr", "add", "10.201.0.2/30", "dev", "uplink1")
	run("-n", donor, "-6", "addr", "add", "2001:db8:201::2/64", "dev", "uplink1", "nodad")
	run("-n", donor, "route", "add", "default", "via", "10.201.0.1", "metric", "100")
	run("-n", donor, "-6", "route", "add", "default", "via", "2001:db8:201::1", "metric", "100")
	// A healthy selected Wi-Fi uplink must win for leases even when the host's
	// native defaults prefer another interface. The public targets are off-link,
	// so connected routes cannot accidentally make this regression pass.
	run("-n", donor, "link", "add", "uplink0", "type", "dummy")
	run("-n", donor, "link", "set", "uplink0", "up")
	run("-n", donor, "addr", "add", "192.0.2.2/24", "dev", "uplink0")
	run("-n", donor, "-6", "addr", "add", "2001:db8:bad::2/64", "dev", "uplink0", "nodad")
	run("-n", donor, "route", "add", "default", "via", "192.0.2.1", "metric", "5")
	run("-n", donor, "-6", "route", "add", "default", "via", "2001:db8:bad::1", "metric", "5")
	run("-6", "route", "add", "fd00:2::2/128", "via", "2001:db8:201::2", "dev", rootIface)
	run("-n", transit, "-6", "route", "add", "2001:db8:201::/64", "via", "fd00:2::1", "dev", "hop1")
	// The transit has no IPv4 addressing or routes: successful IPv4 application
	// traffic must be carried inside the authenticated overlay's IPv6 tunnel.
	if out, err := exec.Command("ip", "-n", transit, "-4", "route", "show").CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("unexpected IPv4 transit route: %v %s", err, out)
	}
	for _, endpoint := range []struct{ address, source string }{{"198.51.100.7", "10.201.0.2"}, {"2001:db8:feed::7", "2001:db8:201::2"}, {"10.99.1.1", "10.201.0.2"}, {"fd99::1", "2001:db8:201::2"}} {
		listener, err := net.Listen("tcp", net.JoinHostPort(endpoint.address, "18081"))
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			source, _, _ := net.SplitHostPort(r.RemoteAddr)
			if source != endpoint.source {
				http.Error(w, "missing donor source NAT: "+source, http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, strings.Repeat("mesh Internet\n", 4096))
		})}
		t.Cleanup(func() { _ = server.Close() })
		go server.Serve(listener)
	}
	packet, err := net.ListenPacket("udp4", "10.201.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	dnsServer := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		if len(request.Question) > 0 {
			reply.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("198.51.100.7")}}
		}
		_ = w.WriteMsg(reply)
	})}
	t.Cleanup(func() { _ = dnsServer.Shutdown() })
	go dnsServer.ActivateAndServe()
	dir := t.TempDir()
	for _, item := range []struct {
		role        string
		credentials *localmesh.Credentials
	}{{"donor", donorCredentials}, {"borrower", borrowerCredentials}} {
		writeNamespaceIdentity(t, filepath.Join(dir, item.role+".json"), root, item.credentials)
	}
	type child struct {
		stdin io.WriteCloser
		wait  func() error
		log   string
	}
	start := func(ns, role, test string) child {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, os.Args[0], "-test.run=^"+test+"$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_SHARING_ROLE="+role, "WENDY_SHARING_IDENTITY="+filepath.Join(dir, role+".json"), "WENDY_SHARING_SYNC="+dir, "WENDY_SHARING_DONOR="+donorCredentials.Device.Principal())
		output, err := os.Create(filepath.Join(dir, role+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = output, output
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		var waitErr error
		wait := func() error { once.Do(func() { waitErr = cmd.Wait(); output.Close(); cancel() }); return waitErr }
		t.Cleanup(func() {
			stdin.Close()
			if err := wait(); err != nil {
				out, _ := os.ReadFile(output.Name())
				t.Errorf("%s process: %v: %s", role, err, out)
			}
		})
		return child{stdin, wait, output.Name()}
	}
	await := func(name string, process child) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				out, _ := os.ReadFile(process.log)
				for _, ns := range []string{borrower, transit, donor} {
					for _, args := range [][]string{{"ip", "-d", "link"}, {"ip", "-6", "route"}, {"ip6tables", "-v", "-n", "-L"}, {"ip6tables", "-t", "nat", "-v", "-n", "-L"}, {"sysctl", "net.ipv6.conf.all.forwarding", "net.ipv6.conf.uplink1.forwarding"}} {
						diagnostics, _ := exec.Command("ip", append([]string{"netns", "exec", ns}, args...)...).CombinedOutput()
						t.Logf("%s %v: %s", ns, args, diagnostics)
					}
				}
				t.Fatalf("%s did not become ready: %s", name, out)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	donorProcess := start(donor, "donor", "TestSharingNamespaceDonorProcess")
	await("donor.ready", donorProcess)
	// Enabling donor forwarding must not turn it into a general IPv6 router.
	// Both route directions exist, so a missing guard would make this ping work.
	if out, err := exec.Command("ping", "-6", "-n", "-c", "1", "-W", "1", "fd00:2::2").CombinedOutput(); err == nil {
		t.Fatalf("sharing enabled unrelated uplink-to-mesh forwarding: %s", out)
	}
	borrowerProcess := start(borrower, "borrower", "TestSharingNamespaceBorrowerProcess")
	await("borrower.ready", borrowerProcess)
	for _, stage := range []string{"denied", "allowed", "revoked"} {
		if stage != "denied" {
			await("app."+stage, borrowerProcess)
		}
		probe := exec.Command("ip", "netns", "exec", app, os.Args[0], "-test.run=^TestSharingNamespaceAppProcess$", "-test.v")
		allowed := "0"
		if stage == "allowed" {
			allowed = "1"
		}
		probe.Env = append(os.Environ(), "WENDY_SHARING_ROLE=app", "WENDY_SHARING_APP_ALLOWED="+allowed)
		if out, err := probe.CombinedOutput(); err != nil {
			t.Fatalf("app %s: %v: %s", stage, err, out)
		}
		if err := os.WriteFile(filepath.Join(dir, "app."+stage+".checked"), []byte("done"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	await("app.done", borrowerProcess)
	if _, err := io.WriteString(donorProcess.stdin, "withdraw\n"); err != nil {
		t.Fatal(err)
	}
	await("donor.withdrawn", donorProcess)
	if err := borrowerProcess.wait(); err != nil {
		out, _ := os.ReadFile(borrowerProcess.log)
		t.Fatalf("borrower: %v %s", err, out)
	}
	donorProcess.stdin.Close()
	if err := donorProcess.wait(); err != nil {
		out, _ := os.ReadFile(donorProcess.log)
		t.Fatalf("donor: %v %s", err, out)
	}
	out, _ := os.ReadFile(borrowerProcess.log)
	t.Log(strings.TrimSpace(string(out)))
}
