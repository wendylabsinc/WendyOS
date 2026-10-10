//go:build linux

package localmesh_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestLANForwardEndpoint(t *testing.T) {
	role := os.Getenv("WENDY_LAN_ENDPOINT")
	if role == "" {
		t.Skip("namespace child")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/push" {
			for i := 0; i < 100; i++ {
				if _, e := fmt.Fprintln(w, "push"); e != nil {
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
		fmt.Fprint(w, "LAN_BACKEND_OK")
	})
	ports := []string{"8080", "8081"}
	if role == "app" {
		ports = []string{"9000"}
	}
	for _, port := range ports {
		go http.ListenAndServe(":"+port, handler)
	}
	time.Sleep(60 * time.Second)
}

func TestLANForwardPolicyComposition(t *testing.T) {
	if os.Getenv("WENDY_LAN_COMPOSITION_TEST") != "1" {
		t.Skip("disposable privileged network-none container only")
	}
	for _, c := range []struct {
		name, forward string
		policyFirst   bool
		lanOnly       bool
	}{{"forward0-chain-first", "0", false, false}, {"forward0-policy-first", "0", true, false}, {"forward1", "1", false, false}, {"forward0-LAN-only", "0", false, true}} {
		t.Run(c.name, func(t *testing.T) {
			run := func(name string, args ...string) string {
				t.Helper()
				v, e := exec.Command(name, args...).CombinedOutput()
				if e != nil {
					t.Fatalf("%s %v: %v %s", name, args, e, v)
				}
				return string(v)
			}
			for _, name := range []string{"lf-app", "lf-peer", "lf-spoof"} {
				run("ip", "netns", "add", name)
				defer exec.Command("ip", "netns", "del", name).Run()
			}
			for _, pair := range [][]string{{"lfah", "lf-app", "10.42.2.1/28", "10.42.2.2/28"}, {"lflh", "lf-peer", "192.168.41.1/24", "192.168.41.22/24"}, {"lfsh", "lf-spoof", "192.168.42.1/24", "192.168.42.22/24"}} {
				run("ip", "link", "add", pair[0], "type", "veth", "peer", "name", pair[0]+"n")
				defer exec.Command("ip", "link", "del", pair[0]).Run()
				run("ip", "link", "set", pair[0]+"n", "netns", pair[1])
				run("ip", "addr", "add", pair[2], "dev", pair[0])
				run("ip", "link", "set", pair[0], "up")
				run("ip", "netns", "exec", pair[1], "ip", "addr", "add", pair[3], "dev", pair[0]+"n")
				run("ip", "netns", "exec", pair[1], "ip", "link", "set", pair[0]+"n", "up")
				run("ip", "netns", "exec", pair[1], "ip", "link", "set", "lo", "up")
			}
			run("ip", "netns", "exec", "lf-peer", "ip", "route", "add", "192.168.42.0/24", "via", "192.168.41.1")
			// Peer knows the test app subnet so a NEW inbound attempt is meaningful.
			run("ip", "netns", "exec", "lf-peer", "ip", "route", "add", "10.42.2.0/28", "via", "192.168.41.1")
			run("sysctl", "-w", "net.ipv4.ip_forward="+c.forward)
			run("iptables", "-P", "FORWARD", "ACCEPT")
			run("iptables", "-A", "FORWARD", "-s", "198.18.0.1/32", "-m", "comment", "--comment", "foreign-test-rule", "-j", "DROP")
			run("ip", "netns", "exec", "lf-spoof", "ip", "addr", "add", "10.42.2.2/32", "dev", "lfshn")
			run("ip", "netns", "exec", "lf-spoof", "ip", "route", "add", "192.168.41.0/24", "via", "192.168.42.1")
			for _, iface := range []string{"all", "lfsh"} {
				run("sysctl", "-w", "net.ipv4.conf."+iface+".rp_filter=0")
			}
			// No production/foreign namespace exists in this network-none test container.
			defer run("iptables", "-F", "FORWARD")
			defer func() {
				for _, chain := range []string{hostnetwork.MeshChainName, "WENDY-LAN"} {
					exec.Command("iptables", "-F", chain).Run()
				}
			}()
			var policy *localmesh.HostPolicy
			var err error
			if !c.policyFirst {
				if err = hostnetwork.InitMeshChain(); err != nil {
					t.Fatal(err)
				}
			}
			if !c.lanOnly {
				policy, err = localmesh.NewHostPolicy(context.Background(), 460)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				if policy != nil {
					if err := policy.Close(); err != nil {
						t.Error(err)
					}
				}
			}()
			if c.policyFirst {
				if err = hostnetwork.InitMeshChain(); err != nil {
					t.Fatal(err)
				}
			}
			for _, role := range []string{"app", "peer"} {
				cmd := exec.Command("ip", "netns", "exec", "lf-"+role, os.Args[0], "-test.run=^TestLANForwardEndpoint$")
				cmd.Env = append(os.Environ(), "WENDY_LAN_ENDPOINT="+role)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { cmd.Process.Kill(); cmd.Wait() }()
			}
			time.Sleep(300 * time.Millisecond)
			a := hostnetwork.LANServiceAccess{AppBridge: "lfah", NetNS: "/run/netns/lf-app", AppIP: "10.42.2.2", Gateway: "10.42.2.1", Destination: "192.168.41.22", Interface: "lflh", Protocol: "tcp", Port: 8080}
			if err := hostnetwork.EnsureLANServiceAccess(a); err != nil {
				t.Fatal(err)
			}
			defer hostnetwork.WithdrawLANServiceAccess(a)
			probe := func(ns, url string) (string, error) {
				v, e := exec.Command("ip", "netns", "exec", ns, "curl", "-fsS", "--noproxy", "*", "--connect-timeout", "1", "--max-time", "2", url).CombinedOutput()
				return string(v), e
			}
			assertAllowed := func() {
				t.Helper()
				v, e := probe("lf-app", "http://192.168.41.22:8080/")
				if e != nil || v != "LAN_BACKEND_OK" {
					t.Fatalf("allowed physical LAN forwarding shadowed: %v %s\n%s", e, v, run("iptables-save", "-c"))
				}
			}
			assertAllowed()
			grantCounter := func() string {
				for _, line := range strings.Split(run("iptables-save", "-c"), "\n") {
					if strings.Contains(line, "-A WENDY-LAN ") && strings.Contains(line, "-i lfah ") && strings.Contains(line, "--dport 8080 -j ACCEPT") {
						return strings.Fields(line)[0]
					}
				}
				t.Fatal("missing owned ingress grant")
				return ""
			}
			// With generic forwarding enabled, terminate the spoof after our
			// chain returns; otherwise a backend response makes the real app
			// emit a RST and legitimately increments its outbound counter.
			run("iptables", "-A", "FORWARD", "-i", "lfsh", "-s", a.AppIP+"/32", "-j", "DROP")
			time.Sleep(250 * time.Millisecond)
			beforeSpoof := grantCounter()
			// Same app source arriving on a foreign interface must never consume
			// this pre-fallback allow. Replies cannot reach the spoof namespace,
			// so inspect the grant counter rather than mistake timeout for proof.
			exec.Command("ip", "netns", "exec", "lf-spoof", "curl", "--interface", "10.42.2.2", "--noproxy", "*", "--connect-timeout", "0.4", "--max-time", "0.5", "http://192.168.41.22:8080/").Run()
			if after := grantCounter(); after != beforeSpoof {
				t.Fatalf("spoof consumed app-owned grant: %s -> %s", beforeSpoof, after)
			}
			if c.forward == "1" {
				if v, e := probe("lf-spoof", "http://192.168.41.22:8080/"); e != nil || v != "LAN_BACKEND_OK" {
					t.Fatalf("ordinary nonmesh forwarding changed: %v %s", e, v)
				}
			}
			if v, e := probe("lf-app", "http://192.168.41.22:8081/"); e == nil {
				t.Fatalf("undeclared sibling service allowed %s", v)
			}
			if v, e := probe("lf-peer", "http://10.42.2.2:9000/"); e == nil {
				t.Fatalf("unsolicited physical inbound allowed %s", v)
			}
			// Host-local connections still reach the app; they are not physical forwarding.
			if v, e := probe("lf-peer", "http://192.168.41.1:9/"); e == nil {
				t.Fatalf("unexpected test backend %s", v)
			}
			if v, e := exec.Command("curl", "-fsS", "--noproxy", "*", "--max-time", "2", "http://10.42.2.2:9000/").CombinedOutput(); e != nil || string(v) != "LAN_BACKEND_OK" {
				t.Fatalf("host-to-app path affected %v %s", e, v)
			}
			// The existing host-loopback published-port DNAT/masquerade path
			// uses OUTPUT, not a physical inbound forwarding exception.
			run("sysctl", "-w", "net.ipv4.conf.all.route_localnet=1")
			output := []string{"-d", "127.0.0.1/32", "-p", "tcp", "--dport", "18190", "-j", "DNAT", "--to-destination", "10.42.2.2:9000"}
			snat := []string{"-o", "lfah", "-d", "10.42.2.2/32", "-p", "tcp", "--dport", "9000", "-j", "SNAT", "--to-source", "10.42.2.1"}
			run("iptables", append([]string{"-t", "nat", "-A", "OUTPUT"}, output...)...)
			defer exec.Command("iptables", append([]string{"-t", "nat", "-D", "OUTPUT"}, output...)...).Run()
			run("iptables", append([]string{"-t", "nat", "-A", "POSTROUTING"}, snat...)...)
			defer exec.Command("iptables", append([]string{"-t", "nat", "-D", "POSTROUTING"}, snat...)...).Run()
			if v, e := exec.Command("curl", "-fsS", "--noproxy", "*", "--max-time", "2", "http://127.0.0.1:18190/").CombinedOutput(); e != nil || string(v) != "LAN_BACKEND_OK" {
				t.Fatalf("host-loopback published port affected: %v %s", e, v)
			}
			// Service withdrawal must stop server push even with generic FORWARD ACCEPT
			// and an existing reverse-NAT conntrack entry; deleting the route is insufficient.
			f, err := os.Create(filepath.Join(t.TempDir(), "push"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			push := exec.Command("ip", "netns", "exec", "lf-app", "curl", "-fsSN", "--noproxy", "*", "--max-time", "6", "http://192.168.41.22:8080/push")
			push.Stdout = f
			if err := push.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { push.Process.Kill(); push.Wait() }()
			time.Sleep(350 * time.Millisecond)
			first, _ := f.Stat()
			if first.Size() < 10 {
				t.Fatal("push flow never became live")
			}
			if err := hostnetwork.WithdrawLANServiceAccess(a); err != nil {
				t.Fatal(err)
			}
			if err := hostnetwork.WithdrawLANDestination(a); err != nil {
				t.Fatal(err)
			}
			time.Sleep(250 * time.Millisecond)
			cut, _ := f.Stat()
			time.Sleep(400 * time.Millisecond)
			after, _ := f.Stat()
			if after.Size() != cut.Size() {
				t.Fatalf("withdrawn established push continued: %d -> %d", cut.Size(), after.Size())
			}
			if v, e := probe("lf-app", "http://192.168.41.22:8080/"); e == nil {
				t.Fatalf("withdrawn service still reachable %s", v)
			}
			if err := hostnetwork.EnsureLANServiceAccess(a); err != nil {
				t.Fatal(err)
			}
			assertAllowed()
			// Reinitializing the host policy must neither shadow existing grants nor
			// duplicate untracked jumps. The exact allow is still bound to this app.
			if policy != nil {
				if err := policy.Close(); err != nil {
					t.Fatal(err)
				}
			}
			// Physical-LAN projection must survive disabling mesh participation.
			assertAllowed()
			if c.forward == "0" {
				if v, e := probe("lf-spoof", "http://192.168.41.22:8080/"); e == nil {
					t.Fatalf("mesh close opened unrelated forwarding: %s", v)
				}
			}

			policy, err = localmesh.NewHostPolicy(context.Background(), 460)
			if err != nil {
				t.Fatal(err)
			}
			assertAllowed()
			if err := hostnetwork.RevokeOrphanLANServiceAllows(); err != nil {
				t.Fatal(err)
			}
			if v, e := probe("lf-app", "http://192.168.41.22:8080/"); e == nil {
				t.Fatalf("orphan allow survived %s", v)
			}
			if err := hostnetwork.RevokeOrphanLANAppRoutes(a.NetNS, a.AppIP, a.Gateway); err != nil {
				t.Fatal(err)
			}
			if err := hostnetwork.EnsureLANServiceAccess(a); err != nil {
				t.Fatal(err)
			}
			assertAllowed()
			run("iptables", "-C", "FORWARD", "-s", "198.18.0.1/32", "-m", "comment", "--comment", "foreign-test-rule", "-j", "DROP")
			if strings.Count(run("iptables", "-S", "WENDY-LAN"), "wendy-lan-reply-guard-v1") != 1 {
				t.Fatal("reply guards accumulated")
			}
			if err := hostnetwork.WithdrawLANServiceAccess(a); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(run("sysctl", "-n", "net.ipv4.ip_forward")); got != "1" {
				t.Fatal("LAN withdrawal disabled live mesh forwarding", got)
			}
			if err := policy.Close(); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(run("sysctl", "-n", "net.ipv4.ip_forward")); got != c.forward {
				t.Fatal("original forwarding not restored", got)
			}

		})
	}
}
