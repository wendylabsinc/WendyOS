//go:build linux

package meshcatalog

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type catalogueChildConfig struct {
	Identity                         localmesh.TCPIdentity
	Dir, Origin, State, Remove, Stop string
	Publish                          bool
}
type catalogueChildState struct {
	Records, Peers int
	Ready          bool
}

func TestNeighbourCatalogueProcess(t *testing.T) {
	path := os.Getenv("WENDY_CATALOGUE_CHILD")
	if path == "" {
		t.Skip("namespace child")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg catalogueChildConfig
	if json.Unmarshal(data, &cfg) != nil {
		t.Fatal("child config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	var catalog *Catalog
	var runtime *Runtime
	var node *localmesh.Node
	started := make(chan struct{})
	done := make(chan error, 1)
	runtimeDone := make(chan error, 1)
	go func() {
		done <- localmesh.RunConfiguredWithNode(ctx, cfg.Dir, cfg.Identity, func(n *localmesh.Node, _ *localmesh.TCPConfig) {
			if n == nil {
				return
			}
			node = n
			now := time.Now()
			cache, e := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), n.Credentials.Verify, now)
			if e != nil {
				t.Error(e)
				cancel()
				return
			}
			store, e := NewReceiptStore(filepath.Join(cfg.Dir, "catalogue-receipts.json"), 4096)
			if e != nil {
				t.Error(e)
				cancel()
				return
			}
			catalog, e = NewCatalog("default", n.Credentials.Device, 4096, n.Credentials, cache, func(string, string, uint16) error { return nil }, nil, store.Save, now)
			if e != nil {
				t.Error(e)
				cancel()
				return
			}
			runtime, e = NewRuntime(catalog, n.Snapshot)
			if e != nil {
				t.Error(e)
				cancel()
				return
			}
			if cfg.Publish {
				w, e := catalog.Publish(testSpec(), now)
				if e != nil {
					t.Error(e)
					cancel()
					return
				}
				runtime.Broadcast(w)
			}
			go func() { runtimeDone <- runtime.Run(ctx, n.ControlHub()) }()
			close(started)
		})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	origin := principalOf(t, cfg.Origin)
	removed := false
	for ctx.Err() == nil {
		if cfg.Publish && !removed {
			if _, e := os.Stat(cfg.Remove); e == nil {
				w, e := catalog.Remove(testSpec().AppID, testSpec().ServiceID, time.Now())
				if e != nil {
					t.Fatal(e)
				}
				runtime.Broadcast(w)
				removed = true
			}
		}
		state := catalogueChildState{Records: len(catalog.Snapshot(time.Now())), Peers: len(node.Snapshot().Links), Ready: runtime.ProjectionReady(origin)}
		raw, _ := json.Marshal(state)
		if e := os.WriteFile(cfg.State+".tmp", raw, 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.Rename(cfg.State+".tmp", cfg.State); e != nil {
			t.Fatal(e)
		}
		if _, e := os.Stat(cfg.Stop); e == nil {
			cancel()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-runtimeDone; err != nil {
		t.Fatal(err)
	}
}

type catalogueOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *catalogueOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *catalogueOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
func TestNeighbourCatalogueLinuxNamespaces(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	run := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("ip", args...).CombinedOutput()
		if e != nil {
			t.Fatalf("ip %v: %v %s", args, e, b)
		}
		return string(b)
	}
	const count = 4
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("wcat%d%d", os.Getpid(), i)
		run("netns", "add", names[i])
		defer exec.Command("ip", "netns", "del", names[i]).Run()
		run("-n", names[i], "link", "set", "lo", "up")
	}
	for i := 0; i < count-1; i++ {
		a, b := fmt.Sprintf("ca%d", i), fmt.Sprintf("cb%d", i)
		run("link", "add", a, "type", "veth", "peer", "name", b)
		for j, iface := range []string{a, b} {
			run("link", "set", iface, "netns", names[i+j])
			run("-n", names[i+j], "addr", "add", fmt.Sprintf("10.%d.0.%d/30", 230+i, j+1), "dev", iface)
			run("-n", names[i+j], "link", "set", iface, "up")
		}
	}
	f := newFixture(t)
	principals := []string{p533, p534, p535, p536}
	root := t.TempDir()
	configs := make([]catalogueChildConfig, count)
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	commands := make([]*exec.Cmd, 0, count)
	outputs := make([]*catalogueOutput, 0, count)
	defer func() {
		for i := range commands {
			_ = os.WriteFile(configs[i].Stop, []byte("stop"), 0600)
		}
		for i, cmd := range commands {
			joined := make(chan error, 1)
			go func() { joined <- cmd.Wait() }()
			select {
			case e := <-joined:
				if e != nil {
					t.Errorf("node %d: %v\n%s", i, e, outputs[i].String())
				}
			case <-time.After(5 * time.Second):
				cancel()
				e := <-joined
				t.Errorf("node %d did not stop: %v\n%s", i, e, outputs[i].String())
			}
		}
	}()
	for i, p := range principals {
		cred := f.creds[p]
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		write := func(path string, v any) {
			t.Helper()
			raw, e := json.Marshal(v)
			if e == nil {
				e = os.WriteFile(path, raw, 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
		}
		peers := []localmesh.TCPPeer{}
		if i > 0 {
			peers = append(peers, localmesh.TCPPeer{Device: f.creds[principals[i-1]].Device.UUID().String(), Address: fmt.Sprintf("10.%d.0.1:43020", 229+i)})
		}
		if i < count-1 {
			peers = append(peers, localmesh.TCPPeer{Device: f.creds[principals[i+1]].Device.UUID().String(), Address: fmt.Sprintf("10.%d.0.2:43020", 230+i)})
		}
		write(filepath.Join(dir, "local-mesh.json"), localmesh.TCPConfig{Listen: "0.0.0.0:43020", Peers: peers})
		// Serialize the real test identity, including issuer trust and private key,
		// only into this ephemeral, owner-only namespace fixture.
		cert, key := credentialPEM(t, cred)
		cfg := catalogueChildConfig{Identity: localmesh.TCPIdentity{Name: "catalogue-test", AgentPort: 50052, Certificate: cert, Chain: f.chain, Key: key}, Dir: dir, Origin: p533, State: filepath.Join(dir, "state.json"), Remove: filepath.Join(dir, "remove"), Stop: filepath.Join(dir, "stop"), Publish: i == 0}
		configs[i] = cfg
		path := filepath.Join(dir, "child.json")
		write(path, cfg)
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", names[i], os.Args[0], "-test.run=^TestNeighbourCatalogueProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_CATALOGUE_CHILD="+path)
		out := new(catalogueOutput)
		cmd.Stdout = out
		cmd.Stderr = out
		if e := cmd.Start(); e != nil {
			t.Fatal(e)
		}
		commands = append(commands, cmd)
		outputs = append(outputs, out)
	}
	state := func(i int) catalogueChildState {
		raw, _ := os.ReadFile(configs[i].State)
		var s catalogueChildState
		_ = json.Unmarshal(raw, &s)
		return s
	}
	await := func(label string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		for i := range configs {
			t.Log(i, state(i), outputs[i].String())
		}
		t.Fatal(label)
	}
	await("four-node neighbour catalogue did not converge", func() bool {
		return state(3).Ready && state(3).Records == 1 && state(0).Peers == 1 && state(1).Peers == 2 && state(2).Peers == 2 && state(3).Peers == 1
	})
	for _, ns := range names {
		if out := run("netns", "exec", ns, "ss", "-lnt"); strings.Contains(out, ":43022") {
			t.Fatal("routed catalogue listener exists", out)
		}
	}
	run("-n", names[2], "link", "set", "ca2", "down")
	await("partition did not invalidate service projection", func() bool { return !state(3).Ready })
	if e := os.WriteFile(configs[0].Remove, []byte("remove"), 0600); e != nil {
		t.Fatal(e)
	}
	await("withdrawal did not reach connected relay", func() bool { return state(2).Records == 0 })
	run("-n", names[2], "link", "set", "ca2", "up")
	await("partition heal did not reconcile withdrawal", func() bool { return state(3).Ready && state(3).Records == 0 })
	for _, cfg := range configs {
		if e := os.WriteFile(cfg.Stop, []byte("stop"), 0600); e != nil {
			t.Fatal(e)
		}
	}
}

func credentialPEM(t *testing.T, c *localmesh.Credentials) (string, string) {
	t.Helper()
	var cert bytes.Buffer
	for _, der := range c.Certificate.Certificate {
		_ = pem.Encode(&cert, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	key, err := x509.MarshalPKCS8PrivateKey(c.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return cert.String(), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
}
