//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func writeTestLeafPreface(conn net.Conn, device meshid.Device) error {
	tenant, id := device.Tenant().UUID(), device.UUID()
	data := append([]byte("WML3"), tenant[:]...)
	return writeFull(conn, append(data, id[:]...))
}

func TestLeafTLSRequiresExactPeerAndProtocol(t *testing.T) {
	router, leaf := testCredentials(t) // Higher UUID leaf must still initiate.
	other := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceCID))
	foreign := testCredentialsFor(t, testPrincipal(testOtherTenant, testDeviceBID))
	for _, tc := range []struct {
		name          string
		credentials   *Credentials
		announced     meshid.Device
		alpn          string
		version       uint16
		noCertificate bool
		valid         bool
	}{
		{"valid higher UUID", leaf, leaf.Device, LeafALPN, tls.VersionTLS13, false, true},
		{"spoofed preface", other, leaf.Device, LeafALPN, tls.VersionTLS13, false, false},
		{"foreign tenant", foreign, leaf.Device, LeafALPN, tls.VersionTLS13, false, false},
		{"wrong ALPN", leaf, leaf.Device, StreamALPN, tls.VersionTLS13, false, false},
		{"legacy ALPN", leaf, leaf.Device, "wendy-local-mesh-leaf/2", tls.VersionTLS13, false, false},
		{"TLS 1.2", leaf, leaf.Device, LeafALPN, tls.VersionTLS12, false, false},
		{"no certificate", leaf, leaf.Device, LeafALPN, tls.VersionTLS13, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				peer, secure, err := acceptLeafTLS(ctx, router, conn)
				if err == nil && tc.valid {
					// A leaf handshake must not be admitted by the BLE entry point.
					if err = OpenStreamHello(ctx, secure, router.Device, peer); err == nil {
						err = errors.New("BLE hello accepted leaf ALPN")
					} else {
						err = OpenStreamHelloWithALPN(ctx, secure, router.Device, peer, LeafALPN)
					}
				}
				serverDone <- err
			}()
			conn, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err = writeTestLeafPreface(conn, tc.announced); err != nil {
				t.Fatal(err)
			}
			cfg, err := leaf.PeerTLS(router.Device)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Certificates = []tls.Certificate{tc.credentials.Certificate}
			cfg.NextProtos = []string{tc.alpn}
			cfg.MinVersion, cfg.MaxVersion = tc.version, tc.version
			if tc.noCertificate {
				cfg.Certificates = nil
			}
			client := tls.Client(conn, cfg)
			err = client.HandshakeContext(ctx)
			if err == nil && tc.valid {
				if client.ConnectionState().DidResume {
					t.Fatal("leaf TLS session resumed")
				}
				err = OpenStreamHelloWithALPN(ctx, client, leaf.Device, router.Device, LeafALPN)
			}
			serverErr := <-serverDone
			if tc.valid && (err != nil || serverErr != nil) {
				t.Fatalf("valid peer: client=%v server=%v", err, serverErr)
			}
			if !tc.valid && serverErr == nil {
				t.Fatal("unauthorized peer passed TLS admission")
			}
		})
	}
}

func TestLeafPrefaceCannotUseFullNodeProtocolOrForeignTenant(t *testing.T) {
	self, peer := testDevices(t)
	for _, device := range []meshid.Device{peer, self, testDevice(t, testPrincipal(testOtherTenant, testDeviceBID))} {
		for _, magic := range []string{"WML3", "WML2", "WMT1"} {
			left, right := net.Pipe()
			go func() {
				tenant, id := device.Tenant().UUID(), device.UUID()
				_, _ = left.Write(append(append([]byte(magic), tenant[:]...), id[:]...))
				_ = left.Close()
			}()
			got, err := readLeafPreface(right, self)
			_ = right.Close()
			valid := device == peer && magic == "WML3"
			if (err == nil) != valid || (valid && got != peer) {
				t.Fatalf("%s %s: %v", magic, device.Principal(), err)
			}
		}
	}
}

func TestLeafControlRejectsRelayedIdentitiesBeforeSynchronization(t *testing.T) {
	router, leaf := testCredentials(t)
	other := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceCID))
	now := time.Now()
	manifest, err := SignManifest(Manifest{Version: ManifestVersionRoute, Device: leaf.Device.UUID(), Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052,
		Route: &RouteOrigin{Prefix: netip.PrefixFrom(leaf.Device.MeshAddress(), 128), RouterID: 42}}, leaf.Certificate.Certificate, leaf.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	thirdParty := manifest
	thirdParty.Principal = other.Device.Principal()
	for _, tc := range []struct {
		name    string
		message ControlMessage
		valid   bool
	}{
		{"catalogue relay", ControlMessage{Kind: "catalog", Catalog: []byte("unsupported catalogue payload")}, false},
		{"own bundle", ControlMessage{Kind: "bundle", Bundle: leaf.Certificate.Certificate}, true},
		{"third-party bundle", ControlMessage{Kind: "bundle", Bundle: other.Certificate.Certificate}, false},
		{"own manifest", ControlMessage{Kind: "manifest", Manifest: &manifest}, true},
		{"third-party manifest", ControlMessage{Kind: "manifest", Manifest: &thirdParty}, false},
		{"tenant identity lookup", ControlMessage{Kind: "identity-request", Principal: other.Device.Principal()}, true},
		{"foreign identity lookup", ControlMessage{Kind: "identity-request", Principal: testPrincipal(testOtherTenant, testDeviceCID)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			link := &leafNodeLink{streamNodeLink: newStreamNodeLink(a), self: router.Device, leaf: leaf.Device}
			defer link.Close()
			writer := newStreamNodeLink(b)
			defer writer.Close()
			sent := make(chan error, 1)
			go func() { sent <- writer.WriteControl(tc.message) }()
			_, err := link.ReadControl()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func leafTestPacket(source, destination netip.Addr, protocol byte, size int) []byte {
	packet := make([]byte, size)
	packet[0], packet[6], packet[7] = 0x60, protocol, 64
	binary.BigEndian.PutUint16(packet[4:6], uint16(size-40))
	copy(packet[8:24], source.AsSlice())
	copy(packet[24:40], destination.AsSlice())
	return packet
}

func TestLeafReassembledSourceConfinementInNodeReceivePath(t *testing.T) {
	self, leaf := testDevices(t)
	for _, tc := range []struct {
		name                string
		source, destination netip.Addr
		protocol            byte
		valid               bool
	}{
		{"own UDP at MTU", leaf.MeshAddress(), self.MeshAddress(), 17, true},
		{"own TCP", leaf.MeshAddress(), self.MeshAddress(), 6, true},
		{"own ICMPv6", leaf.MeshAddress(), self.MeshAddress(), 58, true},
		{"spoofed source", self.MeshAddress(), self.MeshAddress(), 17, false},
		{"Internet destination", leaf.MeshAddress(), netip.MustParseAddr("2001:db8::1"), 17, false},
		{"IPv4 sharing tunnel", leaf.MeshAddress(), self.MeshAddress(), 4, false},
		{"IPv6 sharing tunnel", leaf.MeshAddress(), self.MeshAddress(), 41, false},
		{"IPv6 fragment header", leaf.MeshAddress(), self.MeshAddress(), 44, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			link := &leafNodeLink{streamNodeLink: newStreamNodeLink(a), self: self, leaf: leaf}
			defer link.Close()
			writer := newStreamNodeLink(b)
			defer writer.Close()
			readTUN, writeTUN, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer readTUN.Close()
			defer writeTUN.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			n := &Node{events: make(chan nodeEvent, 1)}
			done := make(chan error, 1)
			go func() { done <- n.readLink(ctx, &nodePeer{link: link, tun: writeTUN}) }()
			packet := leafTestPacket(tc.source, tc.destination, tc.protocol, TunnelMTU)
			if err := writer.SendIP(1, packet); err != nil {
				t.Fatal(err)
			}
			if tc.valid {
				_ = readTUN.SetReadDeadline(time.Now().Add(time.Second))
				got := make([]byte, len(packet))
				if _, err := io.ReadFull(readTUN, got); err != nil || !bytes.Equal(got, packet) {
					t.Fatalf("valid endpoint packet: %v", err)
				}
				_ = link.Close()
			} else {
				select {
				case err := <-done:
					if err == nil || !strings.Contains(err.Error(), "unauthorized IP") {
						t.Fatalf("source policy failed: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("unauthorized packet was not rejected")
				}
				_ = writeTUN.Close()
				if got, _ := io.ReadAll(readTUN); len(got) != 0 {
					t.Fatal("unauthorized packet reached TUN")
				}
				return
			}
			<-done
		})
	}
}

func leafTestBabelUpdate(prefix netip.Prefix, router babel.RouterID, seq uint16, metric uint16) []byte {
	rid := make([]byte, 12)
	rid[0], rid[1] = 6, 10
	binary.BigEndian.PutUint64(rid[4:], uint64(router))
	update := make([]byte, 28)
	update[0], update[1], update[2], update[4] = 8, 26, 2, 128
	binary.BigEndian.PutUint16(update[6:8], 400)
	binary.BigEndian.PutUint16(update[8:10], seq)
	binary.BigEndian.PutUint16(update[10:12], metric)
	copy(update[12:], prefix.Addr().AsSlice())
	packet := append([]byte{42, 2, 0, 40}, rid...)
	return append(packet, update...)
}

func TestLeafBabelRestrictionRunsBeforeNodeEvent(t *testing.T) {
	self, leaf := testDevices(t)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	link := &leafNodeLink{streamNodeLink: newStreamNodeLink(a), self: self, leaf: leaf}
	defer link.Close()
	writer := newStreamNodeLink(b)
	defer writer.Close()
	n := &Node{events: make(chan nodeEvent, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.readLink(ctx, &nodePeer{link: link}) }()
	if err := writer.SendDatagram(append([]byte{PacketBabel}, leafTestBabelUpdate(netip.PrefixFrom(self.MeshAddress(), 128), 42, 1, 0)...)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("leaf advertised another device's route")
	}
	if len(n.events) != 0 {
		t.Fatal("unauthorized routing update reached the shared engine")
	}
}

func TestLeafListenerCancellationClosesPendingHandshakes(t *testing.T) {
	self, _ := testCredentials(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runLeafListener(ctx, &Node{Credentials: self}, listener) }()
	var clients []net.Conn
	for i := 0; i < maxLeafHandshakes+1; i++ {
		conn, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		clients = append(clients, conn)
	}
	last := clients[len(clients)-1]
	_ = last.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := last.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("excess handshake was not closed: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("leaf listener retained pending handshakes on shutdown")
	}
}

// The production Lite parser accepts bundles, manifests and identity requests.
// It cannot participate in a router's descriptor/chunk certificate exchange.
func TestLeafDirectoryUsesVerifiedBundlesAndRepairsMissingIdentity(t *testing.T) {
	source, chain, key, device, now := directoryFixture(t)
	proof, err := SignManifest(Manifest{Version: ManifestVersionRoute, Device: device.UUID(), Revision: 1,
		Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 5054,
		Route: &RouteOrigin{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: 42}}, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Accept(proof, now); err != nil {
		t.Fatal(err)
	}
	sender := newLeafSynchronizer(source, source.cache)
	defer sender.Close()
	if got := sender.ReconcileBounded(now, 1); len(got) != 0 {
		t.Fatal("insufficient budget consumed the leaf's identity hint")
	}
	messages := sender.ContinueReconcile(now, 2)
	if len(messages) != 2 || messages[0].Kind != "bundle" || messages[1].Kind != "manifest" {
		t.Fatalf("unsupported Lite bootstrap: %#v", messages)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), source.cache.verify, now)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := NewDirectory(device.Tenant(), 8, cache)
	if err != nil {
		t.Fatal(err)
	}
	receiver := NewSynchronizer(directory, cache)
	defer receiver.Close()
	// Lose the initial bundle. A same-tenant identity lookup must also return
	// the complete verified bundle, with no descriptor/chunk negotiation.
	replies, _, err := receiver.Receive(messages[1], now)
	if err != nil || len(replies) != 1 || replies[0].Kind != "identity-request" {
		t.Fatalf("missing identity lookup: %#v, %v", replies, err)
	}
	bundles, _, err := sender.Receive(replies[0], now)
	if err != nil || len(bundles) != 1 || bundles[0].Kind != "bundle" {
		t.Fatalf("unsupported Lite identity repair: %#v, %v", bundles, err)
	}
	if _, err := cache.Put(bundles[0].Bundle, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := directory.Accept(*messages[1].Manifest, now); err != nil || !changed {
		t.Fatalf("leaf could not authenticate repaired route proof: %v", err)
	}
	if got := sender.Reconcile(now); len(got) != 1 || got[0].Kind != "manifest" {
		t.Fatalf("leaf bundle repeated unnecessarily: %#v", got)
	}
	for _, principal := range []string{
		testPrincipal(testTenantUUID, testDeviceCID),
		testPrincipal(testOtherTenant, device.UUID().String()),
	} {
		replies, _, err := sender.Receive(ControlMessage{Kind: "identity-request", Principal: principal}, now)
		if err != nil || len(replies) != 0 {
			t.Fatalf("unavailable identity answered: %#v, %v", replies, err)
		}
	}
	if replies, _, err := sender.Receive(ControlMessage{Kind: "identity-request", Principal: device.Principal()}, now.Add(time.Hour)); err != nil || len(replies) != 0 {
		t.Fatalf("expired identity answered: %#v, %v", replies, err)
	}
}

func TestLeafInternetTunnelConfinement(t *testing.T) {
	self, leaf := testDevices(t)
	link := &leafNodeLink{self: self, leaf: leaf}
	packet := leafTestPacket(leaf.MeshAddress(), self.MeshAddress(), 4, TunnelMTU)
	inner := packet[40:]
	inner[0], inner[9] = 0x45, 6
	binary.BigEndian.PutUint16(inner[2:4], uint16(len(inner)))
	copy(inner[12:16], []byte{10, 88, 1, 2})
	copy(inner[16:20], []byte{203, 0, 113, 1})
	if !link.admitsIP(packet, true) {
		t.Fatal("valid borrower IPv4 tunnel rejected")
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"spoofed outer source", func(p []byte) { copy(p[8:24], self.MeshAddress().AsSlice()) }},
		{"unleased source", func(p []byte) { p[52] = 192 }},
		{"zero slot", func(p []byte) { p[54], p[55] = 0, 0 }},
		{"reserved slot", func(p []byte) { p[54], p[55] = 255, 255 }},
		{"fragment", func(p []byte) { p[46] = 0x20 }},
		{"fragment offset", func(p []byte) { p[47] = 1 }},
		{"nested IPv4", func(p []byte) { p[49] = 4 }},
		{"nested IPv6", func(p []byte) { p[49] = 41 }},
		{"options", func(p []byte) { p[40] = 0x46 }},
		{"length mismatch", func(p []byte) { p[43] ^= 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := append([]byte(nil), packet...)
			tc.mutate(changed)
			if link.admitsIP(changed, true) {
				t.Fatal("invalid tunnel admitted")
			}
		})
	}
	copy(packet[8:24], self.MeshAddress().AsSlice())
	copy(packet[24:40], leaf.MeshAddress().AsSlice())
	copy(inner[12:16], []byte{203, 0, 113, 1})
	copy(inner[16:20], []byte{10, 88, 1, 2})
	if !link.admitsIP(packet, false) {
		t.Fatal("lease reply rejected")
	}
	inner[16] = 192
	if link.admitsIP(packet, false) {
		t.Fatal("unleased destination admitted")
	}
}
