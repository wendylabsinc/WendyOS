//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type multihopProcessConfig struct {
	Identity           TCPIdentity
	Dir, Address, Stop string
}

func TestMultihopForwardingProcess(t *testing.T) {
	path := os.Getenv("WENDY_MULTIHOP_PROCESS")
	if path == "" {
		t.Skip("namespace child")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg multihopProcessConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunConfiguredTCP(ctx, cfg.Dir, cfg.Identity) }()
	var listener net.Listener
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		listener, err = net.Listen("tcp6", net.JoinHostPort(cfg.Address, "18082"))
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, cfg.Address) })}
	go server.Serve(listener)
	defer server.Close()
	for {
		if _, err = os.Stat(cfg.Stop); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("router ended early: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

// No fixture enables forwarding: the actual middle Node must acquire its scoped
// transit policy, carry the request and reply, and restore the disabled baseline.
func TestThreeNodeForwardingFromDisabledBaseline(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v %s", name, args, err, out)
		}
		return string(out)
	}
	names := []string{fmt.Sprintf("wmha%d", os.Getpid()), fmt.Sprintf("wmhb%d", os.Getpid()), fmt.Sprintf("wmhc%d", os.Getpid())}
	for _, ns := range names {
		run("ip", "netns", "add", ns)
		defer exec.Command("ip", "netns", "del", ns).Run()
		run("ip", "-n", ns, "link", "set", "lo", "up")
		run("ip", "netns", "exec", ns, "sysctl", "-w", "net.ipv6.conf.all.forwarding=0")
	}
	for i, pair := range [][2]int{{0, 1}, {1, 2}} {
		a, b := fmt.Sprintf("mha%d", i), fmt.Sprintf("mhb%d", i)
		run("ip", "link", "add", a, "type", "veth", "peer", "name", b)
		for j, iface := range []string{a, b} {
			ns := names[pair[j]]
			run("ip", "link", "set", iface, "netns", ns)
			run("ip", "-n", ns, "addr", "add", fmt.Sprintf("10.%d.0.%d/30", 211+i, j+1), "dev", iface)
			run("ip", "-n", ns, "link", "set", iface, "up")
		}
	}
	ids := []string{testDeviceAID, testDeviceBID, testDeviceCID}
	peers := [][]TCPPeer{
		{{Device: ids[1], Address: "10.211.0.2:43020"}},
		{{Device: ids[0], Address: "10.211.0.1:43020"}, {Device: ids[2], Address: "10.212.0.2:43020"}},
		{{Device: ids[1], Address: "10.212.0.1:43020"}},
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	stops := []string{}
	waited := false
	defer func() {
		if !waited {
			cancel()
			for i, cmd := range commands {
				_ = cmd.Wait()
				t.Logf("node %d: %s", i, outputs[i].String())
			}
		}
	}()
	for i, id := range ids {
		principal := testPrincipal(testTenantUUID, id)
		device := testDevice(t, principal)
		cert, key := testIssue(t, principal)
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		write := func(path string, value any) {
			t.Helper()
			data, err := json.Marshal(value)
			if err == nil {
				err = os.WriteFile(path, data, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(dir, tcpConfigName), TCPConfig{Listen: "0.0.0.0:43020", Peers: peers[i]})
		cfg := multihopProcessConfig{Identity: TCPIdentity{Name: "multihop", AgentPort: 50052, Certificate: cert, Chain: testPKI().chainPEM, Key: key}, Dir: dir, Address: device.MeshAddress().String(), Stop: filepath.Join(dir, "stop")}
		path := filepath.Join(dir, "child.json")
		write(path, cfg)
		stops = append(stops, cfg.Stop)
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", names[i], os.Args[0], "-test.run=^TestMultihopForwardingProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_MULTIHOP_PROCESS="+path)
		out := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		outputs = append(outputs, out)
	}
	target := testDevice(t, testPrincipal(testTenantUUID, ids[2])).MeshAddress().String()
	reached := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		out, err := exec.CommandContext(ctx, "ip", "netns", "exec", names[0], "curl", "--noproxy", "*", "-g", "-6", "--fail", "--silent", "--max-time", "2", "http://["+target+"]:18082/").CombinedOutput()
		if err == nil && string(out) == target {
			reached = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !reached {
		t.Fatal("request did not traverse A-B-C from disabled IPv6 forwarding baseline")
	}
	for _, ns := range names {
		if got := strings.TrimSpace(run("ip", "netns", "exec", ns, "sysctl", "-n", "net.ipv6.conf.all.forwarding")); got != "1" {
			t.Fatal("node did not acquire forwarding", got)
		}
	}
	for _, path := range stops {
		if err := os.WriteFile(path, []byte("stop"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var failures []string
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			failures = append(failures, fmt.Sprintf("node %d: %v\n%s", i, err, outputs[i].String()))
		}
	}
	waited = true
	if len(failures) > 0 {
		t.Fatal(strings.Join(failures, "\n"))
	}
	for _, ns := range names {
		if got := strings.TrimSpace(run("ip", "netns", "exec", ns, "sysctl", "-n", "net.ipv6.conf.all.forwarding")); got != "0" {
			t.Fatal("node did not restore forwarding", got)
		}
	}
}
