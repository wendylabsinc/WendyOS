//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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

	"github.com/hashicorp/mdns"
	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

type lanProcessConfig struct {
	Identity    TCPIdentity
	Dir         string
	Dual        bool
	Concurrent  int
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
		_, transport, _ := classifyLANCarrier(tt.name, root, tt.eth, tt.wifi, managed)
		wantTransport := ""
		if tt.wantOK {
			wantTransport = map[string]string{"eth0": "ethernet", "wlan0": "infrastructure-wifi"}[tt.name]
		}
		if transport != wantTransport {
			t.Errorf("%s transport = %q, want %q", tt.name, transport, wantTransport)
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
	if lanIdentityDevice(t, cfg.Identity) == lanDevice(t, 460) {
		ip = net.IPv4(10, 201, 0, 2)
	}
	payload := "authenticated LAN mesh"
	if cfg.Concurrent > 0 {
		payload = strings.Repeat(payload, 4096)
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
						result := []lanInterface{{iface: *iface, ip: ip, net: &net.IPNet{IP: net.IPv4(10, 201, 0, 0), Mask: net.CIDRMask(30, 32)}, cost: EthernetLinkCost, transport: "ethernet"}}
						if cfg.Dual {
							if _, err := os.Stat(cfg.DisableFile); err == nil {
								result = nil
							}
							wifiIface, err := net.InterfaceByName("wllan1")
							if err != nil {
								return nil, err
							}
							wifiIP := net.IPv4(10, 202, 0, 1)
							if lanIdentityDevice(t, cfg.Identity) == lanDevice(t, 460) {
								wifiIP = net.IPv4(10, 202, 0, 2)
							}
							result = append(result, lanInterface{iface: *wifiIface, ip: wifiIP, net: &net.IPNet{IP: net.IPv4(10, 202, 0, 0), Mask: net.CIDRMask(30, 32)}, cost: WiFiLinkCost, transport: "infrastructure-wifi"})
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
	case err := <-meshDone:
		t.Fatalf("mesh node setup failed before readiness: %v", err)
	case <-ctx.Done():
		t.Fatal("mesh node did not start")
	}
	waitCost := func(cost uint16) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			links := node.Snapshot().Links
			if len(links) == 1 && links[0].Device == map[bool]meshid.Device{true: lanDevice(t, 445), false: lanDevice(t, 460)}[lanIdentityDevice(t, cfg.Identity) == lanDevice(t, 460)] && links[0].Cost == cost && links[0].Transport == map[uint16]string{EthernetLinkCost: "ethernet", WiFiLinkCost: "infrastructure-wifi"}[cost] {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatalf("authenticated LAN link did not settle to cost/carrier %d: %+v", cost, node.Snapshot().Links)
	}
	if lanIdentityDevice(t, cfg.Identity) == lanDevice(t, 460) {
		var listener net.Listener
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			listener, err = net.Listen("tcp", net.JoinHostPort(lanDevice(t, 460).MeshAddress().String(), "18080"))
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		seen := make(chan struct{}, max(1, cfg.Concurrent))
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := fmt.Fprint(w, payload); err != nil {
				return
			}
			select {
			case seen <- struct{}{}:
			default:
			}
		})}
		go server.Serve(listener)
		requests := max(1, cfg.Concurrent)
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
		request := func() error {
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				timeout := time.Second
				if cfg.Concurrent > 0 {
					timeout = 3 * time.Second
				}
				client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: timeout}
				response, err := client.Get("http://" + net.JoinHostPort(lanDevice(t, 460).MeshAddress().String(), "18080") + "/")
				if err == nil {
					body, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr == nil && response.StatusCode == 200 && string(body) == payload {
						return nil
					}
				}
				time.Sleep(150 * time.Millisecond)
			}
			return errors.New("LAN mDNS/QUIC link did not route HTTP")
		}
		waitCost(EthernetLinkCost)
		if cfg.Concurrent > 0 {
			results := make(chan error, cfg.Concurrent)
			for range cfg.Concurrent {
				go func() { results <- request() }()
			}
			for range cfg.Concurrent {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			waitCost(EthernetLinkCost) // Routing and authenticated link survive the loss burst.
		} else if err := request(); err != nil {
			t.Fatal(err)
		}
		if cfg.Dual {
			if err := os.WriteFile(cfg.DisableFile, []byte("off"), 0600); err != nil {
				t.Fatal(err)
			}
			waitCost(WiFiLinkCost)
			if err := request(); err != nil {
				t.Fatal(err)
			}
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

func TestLANConcurrentRoutedFlowsUnderUDPLoss(t *testing.T) {
	testLANPairNamespacesWithLoss(t, false, true)
}

func testLANPairNamespaces(t *testing.T, dual bool) {
	testLANPairNamespacesWithLoss(t, dual, false)
}

func testLANPairNamespacesWithLoss(t *testing.T, dual, loss bool) {
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
	for _, name := range names {
		out, err := exec.Command("ip", "netns", "exec", name, "iptables", "-I", "OUTPUT", "-p", "tcp", "--dport", strconv.Itoa(LANPort), "-j", "REJECT").CombinedOutput()
		if err != nil {
			t.Fatalf("block simulator TCP: %v %s", err, out)
		}
	}
	if loss {
		for _, name := range names {
			// Deterministic underlay loss applies to real QUIC, carrier
			// DATAGRAMs and Babel traffic; TCP43023 remains rejected.
			out, err := exec.Command("ip", "netns", "exec", name, "tc", "qdisc", "add", "dev", "wllan0", "root", "netem", "delay", "2ms", "limit", "1000").CombinedOutput()
			if err != nil {
				t.Fatalf("configure underlay delay: %v %s", err, out)
			}
			out, err = exec.Command("ip", "netns", "exec", name, "iptables", "-I", "INPUT", "-p", "udp", "--sport", strconv.Itoa(LANPort), "--dport", strconv.Itoa(LANPort), "-m", "statistic", "--mode", "nth", "--every", "31", "--packet", "0", "-m", "comment", "--comment", "wendy-lan-udp-loss-test", "-j", "DROP").CombinedOutput()
			if err != nil {
				t.Fatalf("configure deterministic UDP loss: %v %s", err, out)
			}
		}
	}
	dir := t.TempDir()
	disableFile := filepath.Join(dir, "ethernet-disabled")
	configs := map[meshid.Device]string{}
	concurrent := 0
	if loss {
		concurrent = 4
	}
	testCredentialsWithPEM(t, func(asset meshid.Device, cert, chain, key string) {
		configDir := filepath.Join(dir, fmt.Sprint(asset))
		if err := os.MkdirAll(configDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, tcpConfigName), []byte("{\"ethernet\":true}"), 0600); err != nil {
			t.Fatal(err)
		}
		processData, _ := json.Marshal(lanProcessConfig{Identity: TCPIdentity{Name: "lan-test", AgentPort: 50052, Certificate: cert, Chain: chain, Key: key}, Dir: configDir, Dual: dual, DisableFile: disableFile, Concurrent: concurrent})
		path := filepath.Join(dir, fmt.Sprintf("%s.json", asset.UUID()))
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
	server.Env = append(os.Environ(), "WENDY_LOCALMESH_LAN_TEST_CONFIG="+configs[lanDevice(t, 460)])
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	client := exec.CommandContext(ctx, "ip", "netns", "exec", names[0], binary, "-test.run", "^TestLANPeerProcess$", "-test.v")
	client.Env = append(os.Environ(), "WENDY_LOCALMESH_LAN_TEST_CONFIG="+configs[lanDevice(t, 445)])
	out, clientErr := client.CombinedOutput()
	serverErr := server.Wait()
	if clientErr != nil || serverErr != nil {
		t.Fatalf("client: %v\n%s\nserver: %v\n%s", clientErr, out, serverErr, serverOutput.String())
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatal(string(out))
	}
	if loss {
		for _, name := range names {
			stats, err := exec.Command("ip", "netns", "exec", name, "iptables", "-L", "INPUT", "-v", "-n", "-x").CombinedOutput()
			if err != nil {
				t.Fatalf("inspect loss counters: %v %s", err, stats)
			}
			dropped := 0
			for _, line := range strings.Split(string(stats), "\n") {
				if strings.Contains(line, "wendy-lan-udp-loss-test") {
					fields := strings.Fields(line)
					if len(fields) > 0 {
						dropped, _ = strconv.Atoi(fields[0])
					}
				}
			}
			if dropped == 0 {
				t.Fatalf("loss fixture dropped no physical packets: %s", stats)
			}
			t.Logf("%s underlay: %s", name, stats)
		}
	}
}

func TestLANAdvertisementFilters(t *testing.T) {
	self, peer := lanDevice(t, 445), lanDevice(t, 460)
	base := mdns.ServiceEntry{Port: LANPort, AddrV4: net.IPv4(192, 168, 2, 3), InfoFields: []string{"principal=" + peer.Principal(), "mesh=" + lanMeshHash(self.Tenant()), "protocol=" + lanProtocol}}
	if device, ip, ok := parseLANEntry(&base, self); !ok || device != peer || !ip.Equal(base.AddrV4) {
		t.Fatalf("valid entry rejected: %v %v", device, ok)
	}
	for _, mutate := range []func(*mdns.ServiceEntry){
		func(e *mdns.ServiceEntry) { e.Port++ },
		func(e *mdns.ServiceEntry) { e.InfoFields[2] = "protocol=tcp" },
		func(e *mdns.ServiceEntry) { e.InfoFields = e.InfoFields[:2] },
		func(e *mdns.ServiceEntry) { e.AddrV4 = net.IPv4(127, 0, 0, 1) },
		func(e *mdns.ServiceEntry) {
			e.InfoFields[0] = "principal=" + testPrincipal(testOtherTenant, testDeviceBID)
		},
		func(e *mdns.ServiceEntry) { e.InfoFields[0] = "principal=" + self.Principal() },
		func(e *mdns.ServiceEntry) { e.InfoFields[1] = "mesh=wrong" },
		func(e *mdns.ServiceEntry) { e.InfoFields = append(e.InfoFields, "principal="+peer.Principal()) },
	} {
		entry := base
		entry.InfoFields = append([]string(nil), base.InfoFields...)
		mutate(&entry)
		if _, _, ok := parseLANEntry(&entry, self); ok {
			t.Fatalf("invalid entry accepted: %+v", entry)
		}
	}
}

func TestLANCandidateSubnetFilterPrecedesPrincipalDeduplication(t *testing.T) {
	self, peer := lanDevice(t, 445), lanDevice(t, 460)
	valid := &mdns.ServiceEntry{Port: LANPort, AddrV4: net.IPv4(192, 0, 2, 6), InfoFields: []string{"principal=" + peer.Principal(), "mesh=" + lanMeshHash(self.Tenant()), "protocol=" + lanProtocol}}
	offSubnet := *valid
	offSubnet.AddrV4 = net.IPv4(198, 51, 100, 9)
	subnet := &net.IPNet{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)}
	for _, entries := range [][]*mdns.ServiceEntry{
		{valid, &offSubnet},
		{&offSubnet, valid},
		{nil, valid, &offSubnet, valid},
	} {
		byKey, keys := lanCandidateEntries(entries, self, subnet)
		if len(keys) != 1 || keys[0] != peer.Principal() || len(byKey) != 1 || byKey[peer.Principal()] != valid {
			t.Fatalf("off-subnet duplicate hid eligible peer: keys=%v entries=%v", keys, byKey)
		}
	}
	if entries, keys := lanCandidateEntries([]*mdns.ServiceEntry{valid, &offSubnet}, self, nil); len(entries) != 0 || len(keys) != 0 {
		t.Fatal("interface without a selected subnet retained candidates")
	}
}

func lanDevice(t *testing.T, number int) meshid.Device {
	return testDevice(t, testPrincipal(testTenantUUID, fmt.Sprintf("2b1c0000-0000-4000-8000-%012d", number)))
}
func lanIdentityDevice(t *testing.T, identity TCPIdentity) meshid.Device {
	t.Helper()
	c, err := NewCredentials(identity.Certificate, identity.Chain, identity.Key)
	if err != nil {
		t.Fatal(err)
	}
	return c.Device
}

func TestLANInterfaceKeyTracksSubnetAndName(t *testing.T) {
	original := lanInterface{iface: net.Interface{Index: 1, Name: "eth0"}, ip: net.IPv4(10, 0, 0, 1), net: &net.IPNet{IP: net.IPv4(10, 0, 0, 1), Mask: net.CIDRMask(24, 32)}, cost: EthernetLinkCost}
	changed := original
	changed.net = &net.IPNet{IP: original.ip, Mask: net.CIDRMask(16, 32)}
	if original.key() == changed.key() {
		t.Fatal("subnet change would retain stale discovery filters")
	}
	changed = original
	changed.iface.Name = "renamed0"
	if original.key() == changed.key() {
		t.Fatal("interface rename would retain stale device binding")
	}
}

func TestLANWorkerFailureClosesPendingConnections(t *testing.T) {
	for _, failure := range []string{"responder", "socket", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			a, b := testCredentials(t)
			socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			address := socket.LocalAddr().String()
			mdnsConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer mdnsConn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				selected := lanInterface{iface: net.Interface{Name: "no-such-lan"}, ip: net.IPv4(127, 0, 0, 1), net: &net.IPNet{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}, cost: EthernetLinkCost}
				done <- serveLANInterface(ctx, LANConfig{Credentials: b}, selected, lanTestClaims(b), socket, mdnsConn, nil)
			}()
			tlsConfig, err := a.PeerTLS(b.Device)
			if err != nil {
				t.Fatal(err)
			}
			pending, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			tlsConfig.GetClientCertificate = func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				close(pending)
				select {
				case <-release:
				case <-info.Context().Done():
				}
				return nil, errors.New("test leaves mTLS incomplete")
			}
			handshake, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			clientDone := make(chan error, 1)
			go func() {
				conn, err := quic.DialAddr(handshake, address, tlsConfig, QUICConfig())
				if conn != nil {
					conn.CloseWithError(0, "test done")
				}
				clientDone <- err
			}()
			select {
			case <-pending:
			case <-time.After(3 * time.Second):
				t.Fatal("mTLS did not reach pending client certificate")
			}
			switch failure {
			case "responder":
				mdnsConn.Close()
			case "socket":
				socket.Close()
			case "cancel":
				cancel()
			}
			select {
			case err := <-done:
				if failure != "cancel" && err == nil {
					t.Fatal("worker failure hidden")
				}
				if failure == "cancel" && err != nil {
					t.Fatalf("cancel returned failure: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("worker did not close pending QUIC handshake")
			}
			// The external test client deliberately blocks its certificate callback.
			// Release that fixture only after proving the server worker exited.
			stop()
			close(release)
			select {
			case <-clientDone:
			case <-time.After(time.Second):
				t.Fatal("client handshake retained after cancellation")
			}
			rebound, err := net.ListenPacket("udp4", address)
			if err != nil {
				t.Fatalf("worker retained UDP socket: %v", err)
			}
			rebound.Close()
		})
	}
}

func TestLANIncomingAdmissionBoundAndSubnet(t *testing.T) {
	selected := lanInterface{net: &net.IPNet{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)}}
	claims := lanTestClaims(&Credentials{Device: lanDevice(t, 460)})
	gate := lanIncomingContext(selected, claims)
	info := &quic.ClientInfo{RemoteAddr: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 43023}}
	var cancels []context.CancelFunc
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for i := 0; i < maxLANIncoming; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		if _, err := gate(ctx, info); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gate(context.Background(), info); err == nil {
		t.Fatal("unbounded unauthenticated state")
	}
	if _, err := gate(context.Background(), &quic.ClientInfo{RemoteAddr: &net.UDPAddr{IP: net.IPv4(198, 51, 100, 2)}}); err == nil {
		t.Fatal("off-subnet source admitted")
	}
	cancels[0]()
	deadline := time.Now().Add(time.Second)
	for {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := gate(ctx, info)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed handshake did not release admission slot")
		}
		time.Sleep(time.Millisecond)
	}
}
