//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func dialTestLeaf(ctx context.Context, address string, leaf, router *Credentials) (*tls.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	if err := writeTestLeafPreface(conn, leaf.Device); err != nil {
		return nil, err
	}
	config, err := leaf.PeerTLS(router.Device)
	if err != nil {
		return nil, err
	}
	config.NextProtos = []string{LeafALPN}
	secure := tls.Client(conn, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	if err := OpenStreamHelloWithALPN(ctx, secure, leaf.Device, router.Device, LeafALPN); err != nil {
		return nil, err
	}
	ok = true
	return secure, nil
}

// This endpoint uses the same Babel engine and stream framing as a leaf, with
// no kernel interface or forwarding capability. Only the Linux router creates
// TUNs/FIB entries; replies must cross its production kernel route to this link.
func runTestLeafEndpoint(ctx context.Context, link *streamNodeLink, leaf, router *Credentials, packets chan<- []byte, generation uint64) error {
	routerID := babel.RouterID(0x1000000000042 + generation)
	engine, err := babel.New(babel.Config{RouterID: routerID, HelloInterval: time.Second, UpdateInterval: 4 * time.Second,
		AcceptRoute: func(babel.RouterID, netip.Prefix) bool { return false }})
	if err != nil {
		return err
	}
	if err := link.WriteControl(ControlMessage{Kind: "bundle", Bundle: leaf.Certificate.Certificate}); err != nil {
		return err
	}
	revision := generation << 32
	lastSeq := uint16(0)
	started := time.Now()
	step := func(event babel.Event) error {
		effects, err := engine.Step(time.Since(started), event)
		if err != nil {
			return err
		}
		if effects.Revision != 0 {
			committed, err := engine.Commit(effects.Revision, true)
			if err != nil {
				return err
			}
			effects.Datagrams = append(effects.Datagrams, committed.Datagrams...)
		}
		seq := engine.Snapshot().Seqno
		if revision == generation<<32 || seq != lastSeq {
			revision++
			now := time.Now()
			proof, err := SignManifest(Manifest{Version: ManifestVersionRoute, Device: leaf.Device.UUID(), Revision: revision,
				Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052,
				Route: &RouteOrigin{Prefix: netip.PrefixFrom(leaf.Device.MeshAddress(), 128), RouterID: routerID, Seqno: seq}}, leaf.Certificate.Certificate, leaf.Signer, now)
			if err != nil {
				return err
			}
			if err := link.WriteControl(ControlMessage{Kind: "manifest", Manifest: &proof}); err != nil {
				return err
			}
			lastSeq = seq
		}
		for _, packet := range effects.Datagrams {
			if err := link.SendDatagram(append([]byte{PacketBabel}, packet.Payload...)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := step(babel.Originate{Prefix: netip.PrefixFrom(leaf.Device.MeshAddress(), 128)}); err != nil {
		return err
	}
	local, peer, err := meshid.LinkLocalAddresses(leaf.Device, router.Device)
	if err != nil {
		return err
	}
	if err := step(babel.AddLink{Link: babel.Link{ID: 1, Local: local, Peer: peer, Cost: 256, MaxPacket: DatagramLimit - 1}}); err != nil {
		return err
	}
	var assembler IPAssembler
	for ctx.Err() == nil {
		receive, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		packet, err := link.ReceiveDatagram(receive)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			if err := step(babel.Tick{}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if len(packet) == 0 {
			return errors.New("empty leaf packet")
		}
		if packet[0] == PacketBabel {
			if err := step(babel.Receive{ID: 1, Packet: packet[1:]}); err != nil {
				return err
			}
		} else if packet[0] == PacketIP {
			ip, err := assembler.Receive(packet, time.Now())
			if err != nil {
				return err
			}
			if ip != nil {
				select {
				case packets <- ip:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return ctx.Err()
}

func checksumLeafPacket(packet []byte, offset int) {
	// IPv6 pseudo-header plus the complete upper-layer body.
	pseudo := append([]byte(nil), packet[8:40]...)
	length := uint32(len(packet) - 40)
	pseudo = binary.BigEndian.AppendUint32(pseudo, length)
	pseudo = append(pseudo, 0, 0, 0, packet[6])
	pseudo = append(pseudo, packet[40:]...)
	if len(pseudo)%2 != 0 {
		pseudo = append(pseudo, 0)
	}
	var sum uint32
	for i := 0; i < len(pseudo); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pseudo[i:]))
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	checksum := ^uint16(sum)
	if checksum == 0 {
		checksum = 65535
	}
	binary.BigEndian.PutUint16(packet[40+offset:], checksum)
}

func TestLeafRoutedEndpointIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable Linux network namespace with NET_ADMIN and TUN")
	}
	var identity TCPIdentity
	router, leaf := testCredentialsWithPEM(t, func(device meshid.Device, cert, chain, key string) {
		if device.UUID().String() == testDeviceAID {
			identity = TCPIdentity{Name: "leaf-router", AgentPort: 50052, Certificate: cert, Chain: chain, Key: key}
		}
	})
	reserved, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()
	dir := t.TempDir()
	data, err := json.Marshal(TCPConfig{LeafListen: address})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tcpConfigName), data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ready := make(chan *Node, 1)
	runtimeDone := make(chan error, 1)
	go func() {
		runtimeDone <- RunConfiguredWithNode(ctx, dir, identity, func(node *Node, _ *TCPConfig) {
			if node != nil {
				ready <- node
			}
		})
	}()
	stopped := false
	defer func() {
		cancel()
		if !stopped {
			<-runtimeDone
		}
	}()
	var node *Node
	select {
	case node = <-ready:
	case err := <-runtimeDone:
		stopped = true
		t.Fatalf("leaf runtime startup: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	conn, err := dialTestLeaf(ctx, address, leaf, router)
	if err != nil {
		t.Fatal(err)
	}
	link := newStreamNodeLink(conn)
	defer link.Close()
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), leaf.Verify, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := NewDirectory(leaf.Device.Tenant(), 32, cache)
	if err != nil {
		t.Fatal(err)
	}
	routerProof := make(chan struct{}, 1)
	controlErrors := make(chan error, 2)
	consumeControls := func(link *streamNodeLink) {
		for {
			message, err := link.ReadControl()
			if err != nil {
				return
			}
			// Match Lite's production control capabilities. In particular, a
			// descriptor/chunk message must fail instead of being silently ignored.
			switch message.Kind {
			case "bundle":
				_, err = cache.Put(message.Bundle, time.Now())
			case "manifest":
				_, err = directory.Accept(*message.Manifest, time.Now())
				if err == nil && message.Manifest.Principal == router.Device.Principal() {
					select {
					case routerProof <- struct{}{}:
					default:
					}
				}
			case "identity-request":
				if message.Principal != leaf.Device.Principal() {
					err = errors.New("unexpected leaf identity lookup")
				} else {
					err = link.WriteControl(ControlMessage{Kind: "bundle", Bundle: leaf.Certificate.Certificate})
				}
			default:
				err = fmt.Errorf("unsupported Lite control message %q", message.Kind)
			}
			if err != nil {
				controlErrors <- err
				_ = link.Close()
				return
			}
		}
	}
	controlDone := make(chan struct{})
	go func() { defer close(controlDone); consumeControls(link) }()
	packets := make(chan []byte, 8)
	endpointDone := make(chan error, 1)
	go func() { endpointDone <- runTestLeafEndpoint(ctx, link, leaf, router, packets, 0) }()
	defer func() { _ = link.Close(); <-controlDone; <-endpointDone }()
	leafPrefix := netip.PrefixFrom(leaf.Device.MeshAddress(), 128)
	waitFor := func(description string, condition func() bool) {
		t.Helper()
		for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); {
			if condition() {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
		t.Fatal(description)
	}
	waitFor("signed leaf route never installed", func() bool {
		for _, route := range node.Snapshot().Routes {
			if route.Prefix == leafPrefix && !route.Local && !route.Unreachable {
				return true
			}
		}
		return false
	})
	select {
	case <-routerProof:
	case err := <-controlErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("leaf did not authenticate the router's route proof")
	}
	snapshotLinks := node.Snapshot().Links
	if len(snapshotLinks) != 1 || snapshotLinks[0].Transport != "leaf" || snapshotLinks[0].SendQueue == nil {
		t.Fatalf("missing leaf carrier diagnostics: %#v", snapshotLinks)
	}
	if peers := node.ControlHub().Peers(); len(peers) != 0 {
		t.Fatal("endpoint-only leaf registered as a catalogue peer")
	}
	// A second authenticated connection cannot replace the working leaf.
	duplicate, err := dialTestLeaf(ctx, address, leaf, router)
	if duplicate != nil {
		_ = duplicate.Close()
	}
	if err == nil || len(node.Snapshot().Links) != 1 {
		t.Fatal("duplicate leaf replaced or multiplied the active link")
	}
	udp, err := net.ListenUDP("udp6", &net.UDPAddr{IP: router.Device.MeshAddress().AsSlice(), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	request := leafTestPacket(leaf.Device.MeshAddress(), router.Device.MeshAddress(), 17, TunnelMTU)
	binary.BigEndian.PutUint16(request[40:42], 18089)
	binary.BigEndian.PutUint16(request[42:44], uint16(udp.LocalAddr().(*net.UDPAddr).Port))
	binary.BigEndian.PutUint16(request[44:46], uint16(len(request)-40))
	copy(request[48:], bytes.Repeat([]byte("leaf-mtu"), (TunnelMTU-48)/8))
	checksumLeafPacket(request, 6)
	if err := link.SendIP(1, request); err != nil {
		t.Fatal(err)
	}
	_ = udp.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, TunnelMTU)
	size, source, err := udp.ReadFromUDP(buffer)
	if err != nil || !bytes.Equal(buffer[:size], request[48:]) || !source.IP.Equal(leaf.Device.MeshAddress().AsSlice()) {
		t.Fatalf("routed UDP input: size=%d source=%v err=%v", size, source, err)
	}
	if _, err := udp.WriteToUDP(buffer[:size], source); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-packets:
		if len(reply) != TunnelMTU || reply[6] != 17 || !bytes.Equal(reply[48:], request[48:]) || !bytes.Equal(reply[24:40], leaf.Device.MeshAddress().AsSlice()) {
			t.Fatal("kernel UDP reply did not follow leaf route")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no routed UDP reply")
	}
	echo := leafTestPacket(leaf.Device.MeshAddress(), router.Device.MeshAddress(), 58, 56)
	echo[40], echo[44], echo[46] = 128, 1, 1
	copy(echo[48:], []byte("leafping"))
	checksumLeafPacket(echo, 2)
	if err := link.SendIP(2, echo); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-packets:
		if reply[6] != 58 || reply[40] != 129 || !bytes.Equal(reply[48:], echo[48:]) {
			t.Fatal("invalid kernel ICMPv6 echo reply")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no routed ICMPv6 echo reply")
	}
	testLeafIPv4Tunnel(t, router.Device, leaf.Device, link, packets)
	// A source spoof passes framing/reassembly but must close this leaf before
	// reaching the kernel. Its selected route then becomes unreachable.
	spoof := leafTestPacket(router.Device.MeshAddress(), router.Device.MeshAddress(), 17, 48)
	if err := link.SendIP(3, spoof); err != nil {
		t.Fatal(err)
	}
	waitFor("source spoof did not remove leaf", func() bool { return len(node.Snapshot().Links) == 0 })
	for _, route := range node.Snapshot().Routes {
		if route.Prefix == leafPrefix && !route.Unreachable {
			t.Fatal("disconnected leaf kept a usable route")
		}
	}
	// A policy violation must leave the listener and node able to admit a
	// fresh authenticated session with a newer signed proof and new origin ID.
	<-controlDone
	recovery, err := dialTestLeaf(ctx, address, leaf, router)
	if err != nil {
		t.Fatal(err)
	}
	recoveryLink := newStreamNodeLink(recovery)
	defer recoveryLink.Close()
	recoveryControlsDone := make(chan struct{})
	go func() { defer close(recoveryControlsDone); consumeControls(recoveryLink) }()
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- runTestLeafEndpoint(ctx, recoveryLink, leaf, router, packets, 1) }()
	defer func() { _ = recoveryLink.Close(); <-recoveryControlsDone; <-recoveryDone }()
	waitFor("fresh signed leaf route did not recover", func() bool {
		for _, route := range node.Snapshot().Routes {
			if route.Prefix == leafPrefix && !route.Unreachable {
				return true
			}
		}
		return false
	})
	select {
	case <-routerProof:
	case err := <-controlErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("reconnected leaf did not authenticate the router")
	}
	if err := recoveryLink.SendIP(4, echo); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-packets:
		if reply[6] != 58 || reply[40] != 129 || !bytes.Equal(reply[48:], echo[48:]) {
			t.Fatal("invalid recovered ICMPv6 echo reply")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no routed reply after reconnect")
	}
	cancel()
	if err := <-runtimeDone; err != nil {
		t.Fatal(err)
	}
	stopped = true
	assertNoOwnedLoopbackAddresses(t)
	if _, err := netlink.LinkByName(legacyInterfaceName); err == nil {
		t.Fatal("shutdown retained mesh host interface")
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if ownedInterface(link.Attrs().Name) {
			t.Fatalf("shutdown retained %s", link.Attrs().Name)
		}
	}
	if conn, err := net.DialTimeout("tcp4", address, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("shutdown retained leaf listener")
	}
	t.Log(fmt.Sprintf("signed leaf /128, UDP %d-byte MTU, ICMPv6, IPv4-in-IPv6, duplicate/source rejection, reconnect and owned cleanup passed", TunnelMTU))
}

// The runtime must preserve an unexpected listener failure while it cancels
// and joins the shared routing node and its owned kernel resources.
type failingLeafTestListener struct {
	net.Listener
	failure error
	failed  chan struct{}
}

func (l *failingLeafTestListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		select {
		case <-l.failed:
			return nil, l.failure
		default:
		}
	}
	return conn, err
}

func TestLeafListenerFailureReportsRuntimeError(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable Linux network namespace with NET_ADMIN and TUN")
	}
	var identity TCPIdentity
	testCredentialsWithPEM(t, func(device meshid.Device, cert, chain, key string) {
		if device.UUID().String() == testDeviceAID {
			identity = TCPIdentity{Name: "leaf-error-router", AgentPort: 50052, Certificate: cert, Chain: chain, Key: key}
		}
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, tcpConfigName), []byte(`{"leafListen":"127.0.0.1:43024"}`), 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected leaf listener failure")
	var listener *failingLeafTestListener
	listen := func(network, address string) (net.Listener, error) {
		real, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		listener = &failingLeafTestListener{Listener: real, failure: failure, failed: make(chan struct{})}
		return listener, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- runConfiguredWithNode(ctx, dir, identity, func(node *Node, _ *TCPConfig) {
			if node != nil {
				ready <- struct{}{}
			}
		}, listen)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("runtime startup: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(listener.failed)
	_ = listener.Close()
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("listener failure was lost during node cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("runtime did not drain after listener failure")
	}
	assertNoOwnedLoopbackAddresses(t)
	if _, err := netlink.LinkByName(legacyInterfaceName); err == nil {
		t.Fatal("failed runtime retained mesh host interface")
	}
}

// Exercise both leaf packet gates around a real kernel IP-in-IPv6 tunnel.
// Lease authentication and exact per-tunnel firewall grants are separately
// exercised by meshsharing's control and namespace tests.
func testLeafIPv4Tunnel(t *testing.T, router, leaf meshid.Device, link *streamNodeLink, packets <-chan []byte) {
	t.Helper()
	tunnel := &netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{Name: "leafip4", MTU: 1280},
		Local: router.MeshAddress().AsSlice(), Remote: leaf.MeshAddress().AsSlice(), Ttl: 64,
		Flags: uint32(netlink.IP6_TNL_F_IGN_ENCAP_LIMIT)}
	if err := netlink.LinkAdd(tunnel); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(tunnel)
	if err := netlink.LinkSetUp(tunnel); err != nil {
		t.Fatal(err)
	}
	serverIP, borrowerIP := net.IPv4(203, 0, 113, 1), net.IPv4(10, 88, 0, 17)
	if err := netlink.AddrAdd(tunnel, &netlink.Addr{IPNet: &net.IPNet{IP: serverIP, Mask: net.CIDRMask(32, 32)}}); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: tunnel.Index, Dst: &net.IPNet{IP: borrowerIP, Mask: net.CIDRMask(32, 32)}}); err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: serverIP})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	body := []byte("leaf-cloud-uplink")
	packet := leafTestPacket(leaf.MeshAddress(), router.MeshAddress(), 4, 40+20+8+len(body))
	inner := packet[40:]
	inner[0], inner[8], inner[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(inner[2:4], uint16(len(inner)))
	copy(inner[12:16], borrowerIP.To4())
	copy(inner[16:20], serverIP.To4())
	var sum uint32
	for offset := 0; offset < 20; offset += 2 {
		sum += uint32(binary.BigEndian.Uint16(inner[offset:]))
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(inner[10:12], ^uint16(sum))
	binary.BigEndian.PutUint16(inner[20:22], 18090)
	binary.BigEndian.PutUint16(inner[22:24], uint16(udp.LocalAddr().(*net.UDPAddr).Port))
	binary.BigEndian.PutUint16(inner[24:26], uint16(8+len(body)))
	copy(inner[28:], body) // IPv4 UDP permits an omitted checksum.
	if err := link.SendIP(4, packet); err != nil {
		t.Fatal(err)
	}
	_ = udp.SetDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 1280)
	n, source, err := udp.ReadFromUDP(buffer)
	if err != nil || !source.IP.Equal(borrowerIP) || !bytes.Equal(buffer[:n], body) {
		t.Fatalf("leaf IPv4 tunnel input: source=%v err=%v", source, err)
	}
	if _, err := udp.WriteToUDP(buffer[:n], source); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-packets:
		if len(reply) != len(packet) || reply[6] != 4 || !bytes.Equal(reply[56:60], borrowerIP.To4()) || !bytes.Equal(reply[68:], body) {
			t.Fatalf("invalid leaf IPv4 tunnel reply: %x", reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no leaf IPv4 tunnel reply")
	}
}
