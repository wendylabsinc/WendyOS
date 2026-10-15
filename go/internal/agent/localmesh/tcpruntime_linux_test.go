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
)

type tcpProcessConfig struct {
	Identity TCPIdentity
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
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunConfiguredTCP(ctx, cfg.Dir, cfg.Identity) }()
	if cfg.Identity.Asset == 460 {
		var listener net.Listener
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			listener, err = net.Listen("tcp", "10.88.1.204:18080")
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
		case <-ctx.Done():
			t.Fatal("no routed request")
		}
		server.Close()
	} else {
		deadline := time.Now().Add(15 * time.Second)
		reached := false
		for time.Now().Before(deadline) {
			client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
			response, err := client.Get("http://10.88.1.204:18080/")
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
	dir := t.TempDir()
	configs := map[int32]string{}
	testCredentialsWithPEM(t, func(asset int32, cert, chain, key string) {
		peer := int32(445)
		address := "10.200.0.1:43020"
		listen := "10.200.0.2:43020"
		if asset == 445 {
			peer = 460
			address, listen = listen, address
		}
		configDir := filepath.Join(dir, fmt.Sprint(asset))
		if err := os.MkdirAll(configDir, 0700); err != nil {
			t.Fatal(err)
		}
		tcpData, _ := json.Marshal(TCPConfig{Listen: listen, Peers: []TCPPeer{{Asset: peer, Address: address}}})
		if err := os.WriteFile(filepath.Join(configDir, tcpConfigName), tcpData, 0600); err != nil {
			t.Fatal(err)
		}
		processData, _ := json.Marshal(tcpProcessConfig{Identity: TCPIdentity{Org: 64, Asset: asset, Name: "test", AgentPort: 50052, Certificate: cert, Chain: chain, Key: key}, Dir: configDir})
		path := filepath.Join(dir, fmt.Sprintf("%d.json", asset))
		if err := os.WriteFile(path, processData, 0600); err != nil {
			t.Fatal(err)
		}
		configs[asset] = path
	})
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, "ip", "netns", "exec", names[1], binary, "-test.run", "^TestConfiguredTCPPeerProcess$", "-test.v")
	server.Env = append(os.Environ(), "WENDY_LOCALMESH_TCP_TEST_CONFIG="+configs[460])
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	client := exec.CommandContext(ctx, "ip", "netns", "exec", names[0], binary, "-test.run", "^TestConfiguredTCPPeerProcess$", "-test.v")
	client.Env = append(os.Environ(), "WENDY_LOCALMESH_TCP_TEST_CONFIG="+configs[445])
	out, clientErr := client.CombinedOutput()
	serverErr := server.Wait()
	if clientErr != nil || serverErr != nil {
		t.Fatalf("client: %v\n%s\nserver: %v\n%s", clientErr, out, serverErr, serverOutput.String())
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatal(string(out))
	}
}
