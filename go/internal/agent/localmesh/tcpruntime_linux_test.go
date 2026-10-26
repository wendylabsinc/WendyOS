//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// tcpProcessConfig describes one re-executed peer of the configured topology.
// Identity is the certificate's device principal; the addresses are underlay
// transport and never an identity.
type tcpProcessConfig struct {
	Identity TCPIdentity
	Device   string
	Peer     string
	Dir      string
}

func TestConfiguredTCPPeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_LOCALMESH_TCP_TEST_CONFIG")
	if path == "" {
		t.Skip("isolated test subprocess")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg tcpProcessConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(cfg.Identity.Certificate, cfg.Identity.Chain, cfg.Identity.Key)
	if err != nil {
		t.Fatal(err)
	}
	self := credentials.Device
	if self.Principal() != cfg.Device {
		t.Fatalf("identity certificate carries %s, config names %s", self.Principal(), cfg.Device)
	}
	peer := testDevice(t, cfg.Peer)
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunConfiguredTCP(ctx, cfg.Dir, cfg.Identity) }()
	// The higher UUID serves on its own mesh address; the lower one reaches it
	// through the Babel-installed route.
	if meshid.Compare(self, peer) > 0 {
		var listener net.Listener
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			listener, err = net.Listen("tcp", fmt.Sprintf("[%s]:18080", self.MeshAddress()))
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		seen := make(chan struct{}, 1)
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "tcp topology through Babel")
			select {
			case seen <- struct{}{}:
			default:
			}
		})}
		go server.Serve(listener)
		select {
		case <-seen:
			// A request can arrive while the reverse route is still settling.
			// Closing the listener the instant one lands turns a slow first
			// exchange into a failure the client cannot retry.
			hold := time.NewTimer(3 * time.Second)
			select {
			case <-hold.C:
			case <-ctx.Done():
			}
			hold.Stop()
		case <-ctx.Done():
			t.Fatal("no routed request")
		}
		server.Close()
	} else {
		deadline := time.Now().Add(15 * time.Second)
		reached := false
		for time.Now().Before(deadline) {
			client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 4 * time.Second}
			response, err := client.Get(fmt.Sprintf("http://[%s]:18080/", peer.MeshAddress()))
			if err == nil {
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr == nil && response.StatusCode == 200 && string(body) == "tcp topology through Babel" {
					reached = true
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !reached {
			t.Fatal("TCP topology did not route HTTP")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredTCPPairIsolatedNamespaces(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	if os.Getenv("WENDY_LOCALMESH_TCP_TEST_CONFIG") != "" {
		t.Skip("parent test only")
	}
	names := []string{fmt.Sprintf("wltca%d", os.Getpid()), fmt.Sprintf("wltcb%d", os.Getpid())}
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
	run("link", "add", "wltca", "type", "veth", "peer", "name", "wltcb")
	run("link", "set", "wltca", "netns", names[0])
	run("link", "set", "wltcb", "netns", names[1])
	for i, name := range names {
		iface := []string{"wltca", "wltcb"}[i]
		run("-n", name, "addr", "add", fmt.Sprintf("10.200.0.%d/30", i+1), "dev", iface)
		run("-n", name, "link", "set", iface, "up")
		run("-n", name, "link", "set", "lo", "up")
	}
	lower := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	higher := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	dir := t.TempDir()
	configs := map[string]string{}
	testCredentialsWithPEM(t, func(device meshid.Device, cert, chain, key string) {
		// Each peer listens on its own underlay address and dials the other's;
		// the lower UUID initiates, so the edge forms once.
		peer, address, listen := higher, "10.200.0.2:43020", "10.200.0.1:43020"
		if device.Principal() == higher.Principal() {
			peer, address, listen = lower, "10.200.0.1:43020", "10.200.0.2:43020"
		}
		configDir := filepath.Join(dir, device.UUID().String())
		if err := os.MkdirAll(configDir, 0700); err != nil {
			t.Fatal(err)
		}
		tcpData, _ := json.Marshal(TCPConfig{Listen: listen, Peers: []TCPPeer{{Device: peer.UUID().String(), Address: address}}})
		if err := os.WriteFile(filepath.Join(configDir, tcpConfigName), tcpData, 0600); err != nil {
			t.Fatal(err)
		}
		processData, _ := json.Marshal(tcpProcessConfig{Identity: TCPIdentity{Name: "test", AgentPort: 50052, Certificate: cert, Chain: chain, Key: key}, Device: device.Principal(), Peer: peer.Principal(), Dir: configDir})
		path := filepath.Join(dir, device.UUID().String()+".json")
		if err := os.WriteFile(path, processData, 0600); err != nil {
			t.Fatal(err)
		}
		configs[device.Principal()] = path
	})
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, "ip", "netns", "exec", names[1], binary, "-test.run", "^TestConfiguredTCPPeerProcess$", "-test.v")
	server.Env = append(os.Environ(), "WENDY_LOCALMESH_TCP_TEST_CONFIG="+configs[higher.Principal()])
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	client := exec.CommandContext(ctx, "ip", "netns", "exec", names[0], binary, "-test.run", "^TestConfiguredTCPPeerProcess$", "-test.v")
	client.Env = append(os.Environ(), "WENDY_LOCALMESH_TCP_TEST_CONFIG="+configs[lower.Principal()])
	out, clientErr := client.CombinedOutput()
	serverErr := server.Wait()
	if clientErr != nil || serverErr != nil {
		t.Fatalf("client: %v\n%s\nserver: %v\n%s", clientErr, out, serverErr, serverOutput.String())
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatal(string(out))
	}
}
