//go:build linux

package meshsession

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type routeSwitchConfig struct{ Cert, Chain, Key, Ready string }

func TestAppRouteSwitchServerProcess(t *testing.T) {
	path := os.Getenv("WENDY_APP_ROUTE_SWITCH_CHILD")
	if path == "" {
		t.Skip("isolated subprocess")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg routeSwitchConfig
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	credentials, err := localmesh.NewCredentials(64, 460, cfg.Cert, cfg.Chain, cfg.Key)
	if err != nil {
		t.Fatal(err)
	}
	app, err := net.Listen("tcp4", "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		for {
			c, err := app.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	server, err := NewServer(credentials, allowPort(func(p uint16) bool { return p == 18080 }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, ":43021") }()
	for server.Addr() == nil {
		select {
		case err := <-done:
			t.Fatal(err)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err = os.WriteFile(cfg.Ready, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	<-done
}

// A route-selected egress filter models Node.readTUN's production forwarding
// gate: packets forced onto the former, still-live link must not leave there.
// Only the destination /32 changes; both peers' VIPs and UDP tuples stay stable.
func TestPooledAppSurvivesSameAddressRouteSwitch(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	ns := fmt.Sprintf("app-reply-%d", os.Getpid())
	run("ip", "netns", "add", ns)
	defer exec.Command("ip", "netns", "del", ns).Run()
	run("ip", "link", "set", "lo", "up")
	run("ip", "-n", ns, "link", "set", "lo", "up")
	for _, suffix := range []string{"old", "new"} {
		c, s := "arc"+suffix, "ars"+suffix
		run("ip", "link", "add", c, "type", "veth", "peer", "name", s)
		defer exec.Command("ip", "link", "del", c).Run()
		run("ip", "link", "set", s, "netns", ns)
		run("ip", "addr", "add", "10.88.1.189/32", "dev", c)
		run("ip", "link", "set", c, "up")
		run("ip", "-n", ns, "addr", "add", "10.88.1.204/32", "dev", s)
		run("ip", "-n", ns, "link", "set", s, "up")
	}
	for _, scope := range []string{"all", "default", "arcold", "arcnew"} {
		run("sysctl", "-qw", "net.ipv4.conf."+scope+".rp_filter=0")
	}
	for _, scope := range []string{"all", "default", "arsold", "arsnew"} {
		run("ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv4.conf."+scope+".rp_filter=0")
	}
	switchRoute := func(suffix string) {
		run("ip", "route", "replace", "10.88.1.204/32", "dev", "arc"+suffix, "src", "10.88.1.189")
		run("ip", "-n", ns, "route", "replace", "10.88.1.189/32", "dev", "ars"+suffix, "src", "10.88.1.204")
	}
	switchRoute("old")
	a, b, chain := fixtureCredentialExpiryWithChain(t, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	var certs []byte
	for _, der := range b.Certificate.Certificate {
		certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(b.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := routeSwitchConfig{Cert: string(certs), Chain: chain, Key: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})), Ready: filepath.Join(dir, "ready")}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "child.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	binary, _ := os.Executable()
	child := exec.Command("ip", "netns", "exec", ns, binary, "-test.run=^TestAppRouteSwitchServerProcess$", "-test.v")
	child.Env = append(os.Environ(), "WENDY_APP_ROUTE_SWITCH_CHILD="+path)
	var log bytes.Buffer
	child.Stdout = &log
	child.Stderr = &log
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait(); t.Log(log.String()) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(cfg.Ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	endpoint := netip.MustParseAddrPort("10.88.1.204:43021")
	exchange := func(c *Client) (*pooledSession, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		defer cancel()
		flow, err := c.Dial(ctx, 460, endpoint, 18080)
		if err != nil {
			return nil, err
		}
		defer flow.Close()
		flow.SetDeadline(time.Now().Add(time.Second))
		if _, err = flow.Write([]byte("route-switch")); err != nil {
			return nil, err
		}
		var got [12]byte
		_, err = io.ReadFull(flow, got[:])
		if err == nil && string(got[:]) != "route-switch" {
			err = fmt.Errorf("wrong echo")
		}
		c.mu.Lock()
		session := c.peers[460]
		c.mu.Unlock()
		return session, err
	}
	first, err := exchange(client)
	if err != nil {
		t.Fatal("initial", err)
	}
	t.Log("initial stream succeeded")
	switchRoute("new")
	run("ip", "netns", "exec", ns, "iptables", "-A", "OUTPUT", "-o", "arsold", "-p", "udp", "--sport", "43021", "-j", "DROP")
	after, pooledErr := exchange(client)
	fresh, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	_, freshErr := exchange(fresh)
	out, _ := exec.Command("ip", "netns", "exec", ns, "iptables", "-nvxL", "OUTPUT").CombinedOutput()
	t.Logf("retained interface old; switched /32 route; egress proof: %s", out)
	t.Logf("same connection error=%v; fresh connection error=%v", pooledErr, freshErr)
	if freshErr != nil {
		t.Fatal("fresh connection", freshErr)
	}
	if pooledErr != nil {
		t.Fatal("established session lost return path after route switch", pooledErr)
	}
	if after != first {
		t.Fatal("route switch repaired via replacement session rather than original")
	}
}
