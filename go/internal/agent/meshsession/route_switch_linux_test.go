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
	credentials, err := localmesh.NewCredentials(cfg.Cert, cfg.Chain, cfg.Key)
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
// Only the destination /128 changes; both peers' mesh addresses and UDP tuples stay stable.
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
	a, b, _, chain := fixtureCredentialExpiryWithChain(t, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	clientAddress, serverAddress := a.Device.MeshAddress().String(), b.Device.MeshAddress().String()
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
		run("ip", "-6", "addr", "add", clientAddress+"/128", "dev", c, "nodad")
		run("ip", "link", "set", c, "up")
		run("ip", "-n", ns, "-6", "addr", "add", serverAddress+"/128", "dev", s, "nodad")
		run("ip", "-n", ns, "link", "set", s, "up")
	}
	switchRoute := func(suffix string) {
		run("ip", "-6", "route", "replace", serverAddress+"/128", "dev", "arc"+suffix, "src", clientAddress)
		run("ip", "-n", ns, "-6", "route", "replace", clientAddress+"/128", "dev", "ars"+suffix, "src", serverAddress)
	}
	switchRoute("old")
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
	endpoint := netip.AddrPortFrom(b.Device.MeshAddress(), Port)
	exchange := func(c *Client) (*pooledSession, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		defer cancel()
		flow, err := c.Dial(ctx, b.Device, endpoint, 18080)
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
		session := c.peers[b.Device]
		c.mu.Unlock()
		return session, err
	}
	first, err := exchange(client)
	if err != nil {
		t.Fatal("initial", err)
	}
	t.Log("initial stream succeeded")
	// Production removes the selected route behind a permanent unreachable
	// fallback. A send during repair must remain packet loss rather than
	// permanently closing the authenticated end-to-end QUIC session.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	live, err := client.Dial(ctx, b.Device, endpoint, 18080)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	live.SetDeadline(time.Now().Add(4 * time.Second))
	for _, side := range []string{"client", "server"} {
		if side == "client" {
			run("ip", "-6", "route", "replace", "unreachable", serverAddress+"/128")
		} else {
			run("ip", "-n", ns, "-6", "route", "replace", "unreachable", clientAddress+"/128")
		}
		if _, err := live.Write([]byte("repair")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
		switchRoute("old")
		var repaired [6]byte
		if _, err := io.ReadFull(live, repaired[:]); err != nil {
			t.Fatalf("%s route repair killed the original stream: %v (QUIC cause: %v)", side, err, context.Cause(first.conn.Context()))
		}
		if string(repaired[:]) != "repair" {
			t.Fatalf("wrong repair reply: %q", repaired)
		}
	}
	switchRoute("new")
	run("ip", "netns", "exec", ns, "ip6tables", "-A", "OUTPUT", "-o", "arsold", "-p", "udp", "--sport", "43021", "-j", "DROP")
	after, pooledErr := exchange(client)
	fresh, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	_, freshErr := exchange(fresh)
	out, _ := exec.Command("ip", "netns", "exec", ns, "ip6tables", "-nvxL", "OUTPUT").CombinedOutput()
	t.Logf("retained interface old; switched /128 route; egress proof: %s", out)
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
