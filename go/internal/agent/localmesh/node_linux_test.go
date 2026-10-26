//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// nodeTestConfig is handed to each re-executed peer process. Identity is the
// device principal; the underlay address is transport only and never an identity.
type nodeTestConfig struct {
	Device, Peer      string
	Cert, Chain, Key  string
	Dir               string
	Underlay, Address string
}

func TestNodePeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_LOCALMESH_NODE_TEST_CONFIG")
	if path == "" {
		t.Skip("isolated test subprocess")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config nodeTestConfig
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(config.Cert, config.Chain, config.Key)
	if err != nil {
		t.Fatal(err)
	}
	self := credentials.Device
	if self.Principal() != config.Device {
		t.Fatalf("credentials carry %s, config names %s", self.Principal(), config.Device)
	}
	peer := testDevice(t, config.Peer)
	if self.Principal() == peer.Principal() {
		t.Fatal("test config names itself as the peer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	node, err := NewNode(ctx, config.Dir, credentials, "test", 50052)
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- node.Run() }()
	defer func() { cancel(); <-runDone }()
	tlsConfig, err := credentials.PeerTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	// The higher UUID serves; the lower one drives the request. Which device is
	// which follows from the principals in the config, not from a numeric asset.
	if meshid.Compare(self, peer) > 0 {
		server := &http.Server{Addr: fmt.Sprintf("[%s]:18080", self.MeshAddress()), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.RemoteAddr, "["+peer.MeshAddress().String()+"]:") {
				http.Error(w, "request bypassed wendy net source addressing", http.StatusForbidden)
				return
			}
			fmt.Fprint(w, "hello over Babel and QUIC")
		})}
		defer server.Close()
		go server.ListenAndServe()
		udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(self.MeshAddress().AsSlice()), Port: 18081})
		if err != nil {
			t.Fatal(err)
		}
		defer udp.Close()
		go func() {
			buf := make([]byte, 2048)
			for {
				n, src, err := udp.ReadFrom(buf)
				if err != nil {
					return
				}
				// Payload arriving over the mesh must carry the peer's own ULA,
				// never a link-local or an underlay address.
				if !src.(*net.UDPAddr).IP.Equal(net.IP(peer.MeshAddress().AsSlice())) {
					t.Errorf("UDP datagram from %v bypassed wendy net source addressing", src)
					continue
				}
				_, _ = udp.WriteTo([]byte(strconv.Itoa(n)), src)
			}
		}()
		listener, err := quic.ListenAddr(config.Underlay, tlsConfig, QUICConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		conn, err := listener.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = node.Attach(ctx, peer, conn)
		return
	}
	var conn *quic.Conn
	for ctx.Err() == nil {
		attempt, cancelAttempt := context.WithTimeout(ctx, time.Second)
		conn, err = quic.DialAddr(attempt, config.Address, tlsConfig, QUICConfig())
		cancelAttempt()
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	linkDone := make(chan error, 1)
	go func() { linkDone <- node.Attach(ctx, peer, conn) }()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	for ctx.Err() == nil {
		response, err := client.Get(fmt.Sprintf("http://[%s]:18080/", peer.MeshAddress()))
		if err == nil {
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err == nil && string(body) == "hello over Babel and QUIC" {
				found := false
				for _, m := range node.Snapshot().Devices {
					if m.Device == peer.UUID() {
						found = true
					}
				}
				if found {
					verifyMeshTransport(t, node, self, peer)
					t.Log("HTTP, signed discovery, origination, MTU and ICMPv6 checks passed across live Babel/QUIC/TUN nodes")
					// End the carrier and wait for its routes to be withdrawn.
					conn.CloseWithError(0, "test ending link")
					<-linkDone
					cancel()
					return
				}
			}
		}
		select {
		case err := <-linkDone:
			t.Fatal("link ended", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("timed out", node.Snapshot())
}

// verifyMeshTransport checks what the routing core promises the host: the
// instance originates only its own /128 at metric 0, the peer's /128 is
// installed with a link-local next hop, the pool prefix is never reachability,
// a full link-MTU IPv6 packet crosses the tunnel while one byte more is
// refused to the application, and kernel-generated ICMPv6 travels back.
func verifyMeshTransport(t *testing.T, node *Node, self, peer meshid.Device) {
	t.Helper()
	own := netip.PrefixFrom(self.MeshAddress(), 128)
	theirs := netip.PrefixFrom(peer.MeshAddress(), 128)
	var local, learned *babel.Route
	routes := node.Snapshot().Routes
	for i, r := range routes {
		if r.Prefix.Bits() <= meshid.MeshPrefix.Bits() {
			t.Fatalf("node carries %s; the wendy net pool is an address pool, never advertised reachability", r.Prefix)
		}
		if r.Prefix == own {
			local = &routes[i]
		}
		if r.Prefix == theirs {
			learned = &routes[i]
		}
	}
	if local == nil || !local.Local || local.Metric != 0 {
		t.Fatalf("own wendy net address is not originated at metric 0: %+v", local)
	}
	// The origin advertises its own /128 at metric 0, so one hop across a link
	// configured with receive cost 256 must arrive as exactly 256.
	if learned == nil || learned.Local || learned.Metric != 256 || !learned.NextHop.IsLinkLocalUnicast() {
		t.Fatalf("peer wendy net address is not a metric-0 origin plus one 256-cost hop: %+v", learned)
	}

	conn, err := net.Dial("udp", net.JoinHostPort(peer.MeshAddress().String(), "18081"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Fill the logical mesh MTU after the 40-byte IPv6 and 8-byte UDP headers.
	const payload = TunnelMTU - 48
	if _, err = conn.Write(make([]byte, payload)); err != nil {
		t.Fatalf("a full link-MTU IPv6 packet did not cross the tunnel: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no UDP echo across the tunnel: %v", err)
	}
	if got := string(buf[:n]); got != strconv.Itoa(payload) {
		t.Fatalf("peer received %s bytes, want %d", got, payload)
	}
	// One byte more does not fit in a single packet. An ordinary socket lets the
	// kernel fragment the datagram at the source; the fragments travel the tunnel
	// and the peer's kernel reassembles them. A socket that told the kernel not
	// to fragment learns the logical link limit as EMSGSIZE. Both are correct;
	// a silent drop is not.
	if _, err = conn.Write(make([]byte, TunnelMTU-48+1)); err != nil {
		t.Fatalf("an over-MTU IPv6 datagram failed at the source: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err = conn.Read(buf); err != nil {
		t.Fatalf("no UDP echo for the fragmented datagram: %v", err)
	}
	if got := string(buf[:n]); got != strconv.Itoa(payload+1) {
		t.Fatalf("peer reassembled %s bytes, want %d", got, payload+1)
	}
	assertDFReachesPacketTooBig(t, peer)

	// ICMPv6 is ordinary payload on this link: the peer's own kernel answers the
	// echo request and its reply comes back through QUIC and the TUN.
	echo, err := icmp.ListenPacket("ip6:ipv6-icmp", "::")
	if err != nil {
		t.Fatalf("ICMPv6 socket: %v", err)
	}
	defer echo.Close()
	request := icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Code: 0, Body: &icmp.Echo{
		ID: os.Getpid() & 0xffff, Seq: 7, Data: []byte("wendy-mesh-probe"),
	}}
	wire, err := request.Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	dst := &net.IPAddr{IP: net.IP(peer.MeshAddress().AsSlice())}
	if _, err = echo.WriteTo(wire, dst); err != nil {
		t.Fatalf("ICMPv6 echo request: %v", err)
	}
	_ = echo.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf = make([]byte, 1024)
	for {
		n, src, err := echo.ReadFrom(buf)
		if err != nil {
			t.Fatalf("no ICMPv6 echo reply crossed the mesh: %v", err)
		}
		reply, err := icmp.ParseMessage(58, buf[:n])
		if err != nil || reply.Type != ipv6.ICMPTypeEchoReply {
			continue
		}
		if !src.(*net.IPAddr).IP.Equal(net.IP(peer.MeshAddress().AsSlice())) {
			t.Fatalf("echo reply came from %v, not the peer's wendy net address", src)
		}
		body, ok := reply.Body.(*icmp.Echo)
		if !ok || string(body.Data) != "wendy-mesh-probe" {
			t.Fatalf("echo reply body changed: %+v", reply.Body)
		}
		return
	}
}

func assertDFReachesPacketTooBig(t *testing.T, peer meshid.Device) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM, syscall.IPPROTO_UDP)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err = syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_MTU_DISCOVER, syscall.IP_PMTUDISC_DO); err != nil {
		t.Fatal(err)
	}
	destination := syscall.SockaddrInet6{Port: 18081}
	copy(destination.Addr[:], peer.MeshAddress().AsSlice())
	if err = syscall.Sendto(fd, make([]byte, TunnelMTU-48+1), 0, &destination); !errors.Is(err, syscall.EMSGSIZE) {
		t.Fatalf("an over-MTU IPv6 packet with DF set returned %v, want EMSGSIZE", err)
	}
}

// TestNodePairIsolatedNamespaces runs two real nodes in disposable namespaces on
// a veth underlay: authenticated QUIC link, Babel route exchange, TUN payload to
// the peer's ULA address, signed device discovery keyed by device principal, and
// the Stage-4 transport promises - origination, MTU and ICMPv6 behaviour.
func TestNodePairIsolatedNamespaces(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	if os.Getenv("WENDY_LOCALMESH_NODE_TEST_CONFIG") != "" {
		t.Skip("parent test only")
	}
	names := []string{fmt.Sprintf("wlma%d", os.Getpid()), fmt.Sprintf("wlmb%d", os.Getpid())}
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
		// An ICMP datagram socket is only permitted for groups inside the
		// namespace's ping group range, and the default range admits none.
		run("netns", "exec", name, "sh", "-c", "echo 0 2147483647 > /proc/sys/net/ipv4/ping_group_range")
	}
	run("link", "add", "wlmtesta", "type", "veth", "peer", "name", "wlmtestb")
	run("link", "set", "wlmtesta", "netns", names[0])
	run("link", "set", "wlmtestb", "netns", names[1])
	for i, name := range names {
		iface := []string{"wlmtesta", "wlmtestb"}[i]
		run("-n", name, "addr", "add", fmt.Sprintf("10.200.0.%d/30", i+1), "dev", iface)
		run("-n", name, "link", "set", iface, "up")
		run("-n", name, "link", "set", "lo", "up")
	}
	dir := t.TempDir()
	configs := map[string]string{}
	testCredentialsWithPEM(t, func(device meshid.Device, cert, chain, key string) {
		lower := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
		higher := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
		peer, underlay, address := higher, "10.200.0.2:18500", "10.200.0.2:18500"
		if device.Principal() == higher.Principal() {
			peer, underlay, address = lower, "10.200.0.2:18500", "10.200.0.1:18500"
		}
		cfg := nodeTestConfig{Device: device.Principal(), Peer: peer.Principal(), Cert: cert, Chain: chain, Key: key, Dir: filepath.Join(dir, device.UUID().String()), Underlay: underlay, Address: address}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, device.UUID().String()+".json")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		configs[device.Principal()] = path
	})
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lower := testPrincipal(testTenantUUID, testDeviceAID)
	higher := testPrincipal(testTenantUUID, testDeviceBID)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, "ip", "netns", "exec", names[1], binary, "-test.run", "^TestNodePeerProcess$", "-test.v")
	server.Env = append(os.Environ(), "WENDY_LOCALMESH_NODE_TEST_CONFIG="+configs[higher])
	var serverOutput bytes.Buffer
	server.Stdout = &serverOutput
	server.Stderr = &serverOutput
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	client := exec.CommandContext(ctx, "ip", "netns", "exec", names[0], binary, "-test.run", "^TestNodePeerProcess$", "-test.v")
	client.Env = append(os.Environ(), "WENDY_LOCALMESH_NODE_TEST_CONFIG="+configs[lower])
	out, clientErr := client.CombinedOutput()
	serverErr := server.Wait()
	if clientErr != nil || serverErr != nil {
		t.Fatalf("client: %v\n%s\nserver: %v\n%s", clientErr, out, serverErr, serverOutput.String())
	}
	if !strings.Contains(string(out), "origination, MTU and ICMPv6 checks passed") {
		t.Fatal(string(out))
	}
	t.Log(string(out))
}
