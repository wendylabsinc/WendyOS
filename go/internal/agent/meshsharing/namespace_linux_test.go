//go:build linux

package meshsharing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// A subprocess is needed because ip netns exec changes the network namespace
// before Go starts any threads. This exercises the real owned host policy.
func TestSharingNamespaceDonorProcess(t *testing.T) {
	if os.Getenv("WENDY_MESH_DONOR_ASSET") == "" {
		t.Skip("isolated namespace subprocess")
	}
	asset, err := strconv.Atoi(os.Getenv("WENDY_MESH_DONOR_ASSET"))
	if err != nil {
		t.Fatal(err)
	}
	upstream := os.Getenv("WENDY_MESH_DONOR_DNS")
	ctx := context.Background()
	policy, err := localmesh.NewHostPolicy(ctx, int32(asset))
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	node := &fakeNode{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"uplink1": upstream}, uplink: true}
	controller, err := NewWithDeps(64, int32(asset), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	for step := range 2 {
		decision, err := controller.Step(ctx, Config{Participate: true, ShareUplink: true})
		if err != nil {
			t.Fatal(err)
		}
		if step == 1 && decision.Mode != "local" {
			t.Fatalf("wrong donor decision: %+v", decision)
		}
	}
	if err := os.WriteFile(os.Getenv("WENDY_MESH_DONOR_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestSharingNamespaceClientProcess(t *testing.T) {
	if os.Getenv("WENDY_MESH_CLIENT_UPSTREAM") == "" {
		t.Skip("isolated namespace subprocess")
	}
	upstream := os.Getenv("WENDY_MESH_CLIENT_UPSTREAM")
	donor := os.Getenv("WENDY_MESH_CLIENT_DONOR")
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Get("http://" + net.JoinHostPort(upstream, "18081"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != upstream {
		t.Fatalf("NAT did not reach selected uplink: %d %q %v", response.StatusCode, body, err)
	}
	query := new(dns.Msg)
	query.SetQuestion("mesh-sharing.test.", dns.TypeA)
	answer, _, err := (&dns.Client{Timeout: 2 * time.Second}).Exchange(query, net.JoinHostPort(donor, "53"))
	if err != nil || len(answer.Answer) != 1 || answer.Answer[0].String() != "mesh-sharing.test.\t30\tIN\tA\t198.51.100.7" {
		t.Fatalf("selected donor DNS proxy failed: %+v %v", answer, err)
	}
	t.Logf("default route, source NAT and DNS succeeded through donor %s", donor)
}

func TestSharingSwitchesDonorsInNamespaces(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("requires a disposable privileged Docker container with iptables, ip and dnsmasq")
	}
	base := fmt.Sprintf("wm%d", os.Getpid())
	borrower, donors := base+"b", [2]string{base + "a", base + "c"}
	all := []string{borrower, donors[0], donors[1]}
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	for _, ns := range all {
		run("netns", "add", ns)
		defer exec.Command("ip", "netns", "del", ns).Run()
		run("-n", ns, "link", "set", "lo", "up")
	}
	rootIfaces := [2]string{"wua" + base, "wuc" + base}
	borrowIfaces := [2]string{"wmba", "wmbc"}
	donorIfaces := [2]string{"wlmpa", "wlmpc"}
	assets := [2]int{460, 461}
	upstreams := [2]string{"10.201.0.1", "10.202.0.1"}
	donorAddresses := [2]string{"10.88.1.204", "10.88.1.205"}
	for i := range donors {
		run("link", "add", rootIfaces[i], "type", "veth", "peer", "name", "uplink1")
		defer exec.Command("ip", "link", "del", rootIfaces[i]).Run()
		run("link", "set", "uplink1", "netns", donors[i])
		run("addr", "add", upstreams[i]+"/30", "dev", rootIfaces[i])
		run("link", "set", rootIfaces[i], "up")
		run("-n", donors[i], "addr", "add", fmt.Sprintf("10.%d.0.2/30", 201+i), "dev", "uplink1")
		run("-n", donors[i], "link", "set", "uplink1", "up")
		run("-n", donors[i], "route", "add", "default", "via", upstreams[i])
		run("link", "add", donorIfaces[i], "type", "veth", "peer", "name", borrowIfaces[i])
		run("link", "set", donorIfaces[i], "netns", donors[i])
		run("link", "set", borrowIfaces[i], "netns", borrower)
		run("-n", donors[i], "addr", "add", donorAddresses[i]+"/32", "dev", donorIfaces[i])
		run("-n", donors[i], "link", "set", donorIfaces[i], "up")
		run("-n", donors[i], "route", "add", "10.88.1.189/32", "dev", donorIfaces[i])
		run("-n", borrower, "link", "set", borrowIfaces[i], "up")
		run("-n", borrower, "route", "add", donorAddresses[i]+"/32", "dev", borrowIfaces[i])
	}
	run("-n", borrower, "addr", "add", "10.88.1.189/32", "dev", "lo")
	for i := range donors {
		listener, err := net.Listen("tcp4", net.JoinHostPort(upstreams[i], "18081"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		wantSource := fmt.Sprintf("10.%d.0.2", 201+i)
		upstream := upstreams[i]
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			source, _, _ := net.SplitHostPort(r.RemoteAddr)
			if source != wantSource {
				http.Error(w, "missing donor source NAT", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, upstream)
		})}
		defer server.Close()
		go server.Serve(listener)
		packet, err := net.ListenPacket("udp4", net.JoinHostPort(upstream, "53"))
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
		defer dnsServer.Shutdown()
		go dnsServer.ActivateAndServe()
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	type child struct {
		cmd   *exec.Cmd
		stdin io.WriteCloser
		out   *bytes.Buffer
	}
	var children []child
	defer func() {
		for _, child := range children {
			_ = child.stdin.Close()
			if err := child.cmd.Wait(); err != nil {
				t.Errorf("donor process: %v: %s", err, child.out.String())
			}
		}
	}()
	for i := range donors {
		ready := filepath.Join(dir, donors[i]+".ready")
		cmd := exec.Command("ip", "netns", "exec", donors[i], binary, "-test.run", "^TestSharingNamespaceDonorProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_MESH_DONOR_ASSET="+strconv.Itoa(assets[i]), "WENDY_MESH_DONOR_DNS="+upstreams[i], "WENDY_MESH_DONOR_READY="+ready)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child{cmd, stdin, output})
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := os.Stat(ready); err != nil {
			t.Fatalf("donor did not prepare NAT/DNS: %s", output.String())
		}
	}
	for i := range donors {
		run("-n", borrower, "route", "replace", "default", "via", donorAddresses[i], "dev", borrowIfaces[i], "src", "10.88.1.189")
		client := exec.Command("ip", "netns", "exec", borrower, binary, "-test.run", "^TestSharingNamespaceClientProcess$", "-test.v")
		client.Env = append(os.Environ(), "WENDY_MESH_CLIENT_UPSTREAM="+upstreams[i], "WENDY_MESH_CLIENT_DONOR="+donorAddresses[i])
		out, err := client.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "default route, source NAT and DNS succeeded") {
			t.Fatalf("donor %d client failed: %v: %s", assets[i], err, out)
		}
		t.Log(strings.TrimSpace(string(out)))
	}
	// Both donors remain live; the borrower changed only its owned default.
	run("-n", borrower, "route", "del", "default")
	if out, err := exec.Command("ip", "-n", borrower, "route", "show", "default").CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("borrowed default remained after withdraw: %v %s", err, out)
	}
}
