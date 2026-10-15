//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// Test-only gate: asset 445 owns a valid org 64 certificate but claims asset
// 444 in the unauthenticated TCP preface. The server must never admit the link.
func TestLANAuthGatePeer(t *testing.T) {
	path := os.Getenv("WENDY_LAN_AUTH_GATE_CONFIG")
	if path == "" {
		t.Skip("namespace child")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg lanProcessConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if cfg.Identity.Asset == 460 {
		iface, err := net.InterfaceByName("wllan0")
		if err != nil {
			t.Fatal(err)
		}
		nodeReady := make(chan *Node, 1)
		done := make(chan error, 1)
		go func() {
			done <- RunConfiguredWithNode(ctx, cfg.Dir, cfg.Identity, func(node *Node, _ *TCPConfig) {
				if node != nil {
					nodeReady <- node
					go runLANWithScan(ctx, LANConfig{Credentials: node.Credentials, Node: node, Ethernet: true}, func() ([]lanInterface, error) {
						return []lanInterface{{iface: *iface, ip: net.IPv4(10, 201, 0, 2), net: &net.IPNet{IP: net.IPv4(10, 201, 0, 0), Mask: net.CIDRMask(30, 32)}, cost: EthernetLinkCost}}, nil
					})
				}
			})
		}()
		var node *Node
		select {
		case node = <-nodeReady:
		case <-ctx.Done():
			t.Fatal("server node did not start")
		}
		deadline := time.Now().Add(11 * time.Second)
		for time.Now().Before(deadline) {
			if links := node.Snapshot().Links; len(links) != 0 {
				t.Fatalf("untrusted link admitted: %+v", links)
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return
	}
	creds, err := NewCredentials(cfg.Identity.Org, cfg.Identity.Asset, cfg.Identity.Certificate, cfg.Identity.Chain, cfg.Identity.Key)
	if err != nil {
		t.Fatal(err)
	}
	var tcp net.Conn
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
		tcp, err = net.DialTimeout("tcp4", "10.201.0.2:43023", time.Second)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	if err := writeTCPPreface(tcp, 64, 444); err != nil {
		t.Fatal(err)
	}
	pc, err := NewTCPPacketConn(tcp)
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{Conn: pc}
	defer transport.Close()
	tlsConfig, err := creds.PeerTLS(460)
	if err != nil {
		t.Fatal(err)
	}
	handshake, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	conn, err := transport.Dial(handshake, pc.RemoteAddr(), tlsConfig, QUICConfig())
	if err == nil {
		defer conn.CloseWithError(0, "gate done")
		_, err = OpenControl(handshake, conn, 64, 445, 460)
	}
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("spoofed preface was not promptly rejected: %v", err)
	}
}

func TestLANRejectsSpoofedPrefaceGate(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	nameA := fmt.Sprintf("wlauth%d", os.Getpid())
	nameB := fmt.Sprintf("wlpeer%d", os.Getpid())
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
	}
	run("netns", "add", nameA)
	defer exec.Command("ip", "netns", "del", nameA).Run()
	run("netns", "add", nameB)
	defer exec.Command("ip", "netns", "del", nameB).Run()
	run("link", "add", "wllan0", "netns", nameA, "type", "veth", "peer", "name", "wllan0", "netns", nameB)
	for i, name := range []string{nameA, nameB} {
		run("-n", name, "addr", "add", fmt.Sprintf("10.201.0.%d/30", i+1), "dev", "wllan0")
		run("-n", name, "link", "set", "wllan0", "up")
		run("-n", name, "link", "set", "lo", "up")
	}
	dir := t.TempDir()
	configs := map[int32]string{}
	testCredentialsWithPEM(t, func(asset int32, cert, chain, key string) {
		configDir := filepath.Join(dir, fmt.Sprint(asset))
		if err := os.MkdirAll(configDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, tcpConfigName), []byte("{\"ethernet\":true}"), 0600); err != nil {
			t.Fatal(err)
		}
		processData, _ := json.Marshal(lanProcessConfig{Identity: TCPIdentity{Org: 64, Asset: asset, Name: "auth-gate", AgentPort: 50052, Certificate: cert, Chain: chain, Key: key}, Dir: configDir})
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, "ip", "netns", "exec", nameB, binary, "-test.run", "^TestLANAuthGatePeer$", "-test.v")
	server.Env = append(os.Environ(), "WENDY_LAN_AUTH_GATE_CONFIG="+configs[460])
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	client := exec.CommandContext(ctx, "ip", "netns", "exec", nameA, binary, "-test.run", "^TestLANAuthGatePeer$", "-test.v")
	client.Env = append(os.Environ(), "WENDY_LAN_AUTH_GATE_CONFIG="+configs[445])
	out, clientErr := client.CombinedOutput()
	serverErr := server.Wait()
	if clientErr != nil || serverErr != nil {
		t.Fatalf("attacker: %v\n%s\nserver: %v\n%s", clientErr, out, serverErr, serverOutput.String())
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("attacker did not pass: %s", out)
	}
}
