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

	"github.com/hashicorp/mdns"
	"go.uber.org/zap"
)

type lanProcessConfig struct {
	Identity    TCPIdentity
	Dir         string
	Dual        bool
	DisableFile string
}

func TestLANInterfaceClassification(t *testing.T) {
	root := t.TempDir()
	makeIface := func(name, kind string, paths ...string) {
		t.Helper()
		base := filepath.Join(root, name)
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "type"), []byte(kind), 0600); err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if err := os.Mkdir(filepath.Join(base, path), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	makeIface("eth0", "1", "device")
	makeIface("wlan0", "1", "device", "wireless")
	makeIface("ap0", "1", "device", "wireless")
	makeIface("br0", "1")
	makeIface("mesh0", "65534", "device")
	managed := func(name string) bool { return name == "wlan0" }
	tests := []struct {
		name   string
		eth    bool
		wifi   bool
		want   uint16
		wantOK bool
	}{
		{"eth0", true, false, EthernetLinkCost, true},
		{"eth0", false, true, 0, false},
		{"wlan0", false, true, WiFiLinkCost, true},
		{"wlan0", true, false, 0, false},
		{"ap0", true, true, 0, false},
		{"br0", true, true, 0, false},
		{"mesh0", true, true, 0, false},
	}
	for _, tt := range tests {
		cost, ok := classifyLANInterface(tt.name, root, tt.eth, tt.wifi, managed)
		if cost != tt.want || ok != tt.wantOK {
			t.Errorf("%s Ethernet=%v WiFi=%v: got cost=%d ok=%v; want %d %v", tt.name, tt.eth, tt.wifi, cost, ok, tt.want, tt.wantOK)
		}
	}
}

func TestLANPeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_LOCALMESH_LAN_TEST_CONFIG")
	if path == "" {
		t.Skip("isolated LAN peer subprocess")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg lanProcessConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	iface, err := net.InterfaceByName("wllan0")
	if err != nil {
		t.Fatal(err)
	}
	ip := net.IPv4(10, 201, 0, 1)
	if cfg.Identity.Asset == 460 {
		ip = net.IPv4(10, 201, 0, 2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	meshDone := make(chan error, 1)
	nodeReady := make(chan *Node, 1)
	go func() {
		meshDone <- RunConfiguredWithNode(ctx, cfg.Dir, cfg.Identity, func(node *Node, config *TCPConfig) {
			if node != nil {
				nodeReady <- node
				go func() {
					logger, _ := zap.NewDevelopment()
					_ = runLANWithScan(ctx, LANConfig{Credentials: node.Credentials, Node: node, Ethernet: true, Selection: NewPeerSelection(node.Snapshot), Logger: logger}, func() ([]lanInterface, error) {
						result := []lanInterface{{iface: *iface, ip: ip, net: &net.IPNet{IP: net.IPv4(10, 201, 0, 0), Mask: net.CIDRMask(30, 32)}, cost: EthernetLinkCost}}
						if cfg.Dual {
							if _, err := os.Stat(cfg.DisableFile); err == nil {
								result = nil
							}
							wifiIface, err := net.InterfaceByName("wllan1")
							if err != nil {
								return nil, err
							}
							wifiIP := net.IPv4(10, 202, 0, 1)
							if cfg.Identity.Asset == 460 {
								wifiIP = net.IPv4(10, 202, 0, 2)
							}
							result = append(result, lanInterface{iface: *wifiIface, ip: wifiIP, net: &net.IPNet{IP: net.IPv4(10, 202, 0, 0), Mask: net.CIDRMask(30, 32)}, cost: WiFiLinkCost})
						}
						return result, nil
					})
				}()
			}
		})
	}()
	var node *Node
	select {
	case node = <-nodeReady:
	case <-ctx.Done():
		t.Fatal("mesh node did not start")
	}
	waitCost := func(cost uint16) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			links := node.Snapshot().Links
			if len(links) == 1 && links[0].Asset == map[bool]int32{true: 445, false: 460}[cfg.Identity.Asset == 460] && links[0].Cost == cost {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatalf("LAN link did not settle to cost %d: %+v", cost, node.Snapshot().Links)
	}
	if cfg.Identity.Asset == 460 {
		var listener net.Listener
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			listener, err = net.Listen("tcp", "10.88.1.204:18080")
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		seen := make(chan struct{}, 1)
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "authenticated LAN mesh")
			select {
			case seen <- struct{}{}:
			default:
			}
		})}
		go server.Serve(listener)
		requests := 1
		if cfg.Dual {
			requests = 2
		}
		for i := 0; i < requests; i++ {
			select {
			case <-seen:
			case <-ctx.Done():
				t.Fatal("no routed LAN request")
			}
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Fatal(err)
		}
		deadline = time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(cfg.DisableFile + ".client-done"); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	} else {
		request := func() {
			t.Helper()
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
				response, err := client.Get("http://10.88.1.204:18080/")
				if err == nil {
					body, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr == nil && response.StatusCode == 200 && string(body) == "authenticated LAN mesh" {
						return
					}
				}
				time.Sleep(150 * time.Millisecond)
			}
			t.Fatal("LAN mDNS/QUIC link did not route HTTP")
		}
		waitCost(EthernetLinkCost)
		request()
		if cfg.Dual {
			if err := os.WriteFile(cfg.DisableFile, []byte("off"), 0600); err != nil {
				t.Fatal(err)
			}
			waitCost(WiFiLinkCost)
			request()
		}
		if err := os.WriteFile(cfg.DisableFile+".client-done", []byte("done"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err := <-meshDone; err != nil {
		t.Fatal(err)
	}
}

func TestLANPairIsolatedNamespaces(t *testing.T) {
	testLANPairNamespaces(t, false)
}

func TestLANEthernetPreferenceAndWiFiFailover(t *testing.T) {
	testLANPairNamespaces(t, true)
}

func testLANPairNamespaces(t *testing.T, dual bool) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	if os.Getenv("WENDY_LOCALMESH_LAN_TEST_CONFIG") != "" {
		t.Skip("parent test only")
	}
	names := []string{fmt.Sprintf("wllana%d", os.Getpid()), fmt.Sprintf("wllanb%d", os.Getpid())}
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
	run("link", "add", "wllan0", "netns", names[0], "type", "veth", "peer", "name", "wllan0", "netns", names[1])
	if dual {
		run("link", "add", "wllan1", "netns", names[0], "type", "veth", "peer", "name", "wllan1", "netns", names[1])
	}
	for i, name := range names {
		run("-n", name, "addr", "add", fmt.Sprintf("10.201.0.%d/30", i+1), "dev", "wllan0")
		run("-n", name, "link", "set", "wllan0", "up")
		run("-n", name, "link", "set", "lo", "up")
		if dual {
			run("-n", name, "addr", "add", fmt.Sprintf("10.202.0.%d/30", i+1), "dev", "wllan1")
			run("-n", name, "link", "set", "wllan1", "up")
		}
	}
	dir := t.TempDir()
	disableFile := filepath.Join(dir, "ethernet-disabled")
	configs := map[int32]string{}
	testCredentialsWithPEM(t, func(asset int32, cert, chain, key string) {
		configDir := filepath.Join(dir, fmt.Sprint(asset))
		if err := os.MkdirAll(configDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, tcpConfigName), []byte("{\"ethernet\":true}"), 0600); err != nil {
			t.Fatal(err)
		}
		processData, _ := json.Marshal(lanProcessConfig{Identity: TCPIdentity{Org: 64, Asset: asset, Name: "lan-test", AgentPort: 50052, Certificate: cert, Chain: chain, Key: key}, Dir: configDir, Dual: dual, DisableFile: disableFile})
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
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, "ip", "netns", "exec", names[1], binary, "-test.run", "^TestLANPeerProcess$", "-test.v")
	server.Env = append(os.Environ(), "WENDY_LOCALMESH_LAN_TEST_CONFIG="+configs[460])
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	client := exec.CommandContext(ctx, "ip", "netns", "exec", names[0], binary, "-test.run", "^TestLANPeerProcess$", "-test.v")
	client.Env = append(os.Environ(), "WENDY_LOCALMESH_LAN_TEST_CONFIG="+configs[445])
	out, clientErr := client.CombinedOutput()
	serverErr := server.Wait()
	if clientErr != nil || serverErr != nil {
		t.Fatalf("client: %v\n%s\nserver: %v\n%s", clientErr, out, serverErr, serverOutput.String())
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatal(string(out))
	}
}

func TestLANAdvertisementFilters(t *testing.T) {
	base := mdns.ServiceEntry{
		Port:       LANPort,
		AddrV4:     net.IPv4(192, 168, 2, 3),
		InfoFields: []string{"org=64", "asset=460", "mesh=" + lanMeshHash(64)},
	}
	if asset, ip, ok := parseLANEntry(&base, 64, 445); !ok || asset != 460 || !ip.Equal(base.AddrV4) {
		t.Fatalf("valid entry rejected: asset=%d ip=%v ok=%v", asset, ip, ok)
	}
	for _, mutate := range []func(*mdns.ServiceEntry){
		func(e *mdns.ServiceEntry) { e.Port++ },
		func(e *mdns.ServiceEntry) { e.AddrV4 = net.IPv4(127, 0, 0, 1) },
		func(e *mdns.ServiceEntry) { e.InfoFields[0] = "org=65" },
		func(e *mdns.ServiceEntry) { e.InfoFields[1] = "asset=444" },
		func(e *mdns.ServiceEntry) { e.InfoFields[2] = "mesh=wrong" },
		func(e *mdns.ServiceEntry) { e.InfoFields = append(e.InfoFields, "asset=461") },
	} {
		entry := base
		entry.InfoFields = append([]string(nil), base.InfoFields...)
		mutate(&entry)
		if _, _, ok := parseLANEntry(&entry, 64, 445); ok {
			t.Fatalf("invalid entry accepted: %+v", entry)
		}
	}
}
