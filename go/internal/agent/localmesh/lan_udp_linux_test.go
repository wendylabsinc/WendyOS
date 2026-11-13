//go:build linux

package localmesh

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
)

func TestLANUDPBindUsesSelectedInterface(t *testing.T) {
	selected := lanInterface{iface: net.Interface{Name: "lo"}, ip: net.IPv4(127, 0, 0, 1)}
	socket, err := listenLANQUIC(context.Background(), selected, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	udp, ok := socket.(*net.UDPConn)
	if !ok {
		t.Fatalf("production listener is not real UDP: %T", socket)
	}
	raw, err := udp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var bound string
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		bound, sockErr = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
	}); err != nil {
		t.Fatal(err)
	}
	if sockErr != nil || bound != "lo" {
		t.Fatalf("wrong interface binding: %q %v", bound, sockErr)
	}
	selected.iface.Name = "not-a-real-lan"
	if bad, err := listenLANQUIC(context.Background(), selected, "127.0.0.1:0"); err == nil {
		bad.Close()
		t.Fatal("invalid interface fell back to default routing")
	}
}

func TestLANUDPRejectedPeerRetainsWorkerSocket(t *testing.T) {
	a, b := testCredentials(t)
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	endpoint := socket.LocalAddr()
	mdnsConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer mdnsConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	selected := lanInterface{iface: net.Interface{Name: "not-a-real-lan"}, ip: net.IPv4(127, 0, 0, 1), net: &net.IPNet{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}, cost: EthernetLinkCost}
	claims := lanTestClaims(b)
	done := make(chan error, 1)
	ready := make(chan CarrierStatus, 1)
	// An incumbent-ready duplicate is rejected at Node's authenticated
	// Ready boundary after its real hello. No kernel setup is needed.
	node := &Node{Credentials: b, ctx: ctx, events: make(chan nodeEvent)}
	admitted := make(chan meshid.Device, 1)
	go func() {
		for {
			select {
			case event := <-node.events:
				if event.add != nil {
					admitted <- event.add.device
				}
				event.result <- nil
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		done <- serveLANInterface(ctx, LANConfig{Credentials: b, Node: node, Status: func(status CarrierStatus) { ready <- status }}, selected, claims, socket, mdnsConn, nil)
	}()
	select {
	case status := <-ready:
		if !status.Ready || status.Err != nil {
			t.Fatalf("native UDP setup readiness: %+v", status)
		}
	case <-ctx.Done():
		t.Fatal("native UDP setup did not report readiness")
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	clientSocket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientSocket.Close()
	clientTransport := &quic.Transport{Conn: clientSocket}
	defer clientTransport.Close()
	// An already claimed identity/cost must be rejected after real mTLS,
	// without closing the endpoint used by the following independent attempts.
	incumbent := claims.policy.ReserveGroup("incumbent", a.Device, false, strconv.Itoa(int(EthernetLinkCost)))
	if incumbent == nil || !incumbent.Ready(a.Device, func() { t.Error("incumbent closed") }) {
		t.Fatal("initial ready lease failed")
	}
	defer incumbent.Release()
	config, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := clientTransport.Dial(ctx, endpoint, config, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = OpenControl(ctx, duplicate, a.Device, b.Device)
	// Ready can reject the authenticated duplicate before the client consumes
	// the server hello. It must leave both routing and the shared socket alone.
	select {
	case <-duplicate.Context().Done():
	case <-ctx.Done():
		t.Fatal("ready duplicate not promptly rejected")
	}
	select {
	case peer := <-admitted:
		t.Fatalf("rejected duplicate published a routing link: %v", peer)
	default:
	}
	incumbent.Release()
	for trial := 0; trial < 2; trial++ {
		config, err := a.PeerTLS(b.Device)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := clientTransport.Dial(ctx, endpoint, config, QUICConfig())
		if err != nil {
			t.Fatalf("shared UDP endpoint unavailable on trial%d: %v", trial, err)
		}
		if _, err := OpenControl(ctx, conn, lanDevice(t, 444), b.Device); err == nil {
			t.Fatal("certificate445 admitted as hello444")
		}
		conn.CloseWithError(0, "rejected peer finished")
		deadline := time.Now().Add(time.Second)
		for {
			count := claims.policy.Counts().Total
			if count == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("rejected peer retained its duplicate claim")
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func TestLANUDPAllowsSoleReverseInitiator(t *testing.T) {
	a, b := testCredentials(t)
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	mdnsConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer mdnsConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	selected := lanInterface{iface: net.Interface{Name: "not-a-real-lan"}, ip: net.IPv4(127, 0, 0, 1), net: &net.IPNet{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}, cost: EthernetLinkCost}
	claims := lanTestClaims(a)
	done := make(chan error, 1)
	node := &Node{Credentials: a, ctx: ctx, events: make(chan nodeEvent)}
	go func() {
		done <- serveLANInterface(ctx, LANConfig{Credentials: a, Node: node}, selected, claims, socket, mdnsConn, nil)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client, err := b.PeerTLS(a.Device)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, socket.LocalAddr().String(), client, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "crossed attempt done")
	if _, err := OpenControl(ctx, conn, b.Device, a.Device); err != nil {
		t.Fatalf("sole higher-identity initiator rejected before node admission: %v", err)
	}
	select {
	case event := <-node.events:
		if event.add == nil || event.add.device != b.Device {
			t.Fatalf("wrong authenticated peer: %+v", event)
		}
		// Stop at the kernel admission seam after proving native TLS/hello.
		event.result <- errors.New("test stops at node admission")
	case <-ctx.Done():
		t.Fatal("reverse connection never reached authenticated node admission")
	}
	select {
	case <-conn.Context().Done():
	case <-ctx.Done():
		t.Fatal("node refusal did not close peer")
	}
}

func lanTestClaims(c *Credentials) *lanClaims {
	return &lanClaims{policy: NewConnectionPolicy(c.Device, true, ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections})}
}

func TestLANUDPAdmissionSharesPendingBudgetAcrossInterfacesAndDirections(t *testing.T) {
	self, peer := testCredentials(t)
	claims := lanTestClaims(self)
	outgoing := claims.policy.ReserveGroup("outgoing", peer.Device, true, strconv.Itoa(int(EthernetLinkCost)))
	if outgoing == nil {
		t.Fatal("outgoing reservation failed")
	}
	defer outgoing.Release()
	selected := lanInterface{net: &net.IPNet{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)}, cost: EthernetLinkCost}
	ethernet := lanIncomingContext(selected, claims)
	selected.cost = WiFiLinkCost
	wifi := lanIncomingContext(selected, claims)
	info := &quic.ClientInfo{RemoteAddr: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: LANPort}}
	var cancels []context.CancelFunc
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for i := 0; i < LANMaxConnections-1; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		gate := ethernet
		if i%2 != 0 {
			gate = wifi
		}
		accepted, err := gate(ctx, info)
		if err != nil {
			t.Fatalf("pending admission%d: %v", i, err)
		}
		lease, _ := accepted.Value(connectionLeaseKey{}).(*ConnectionLease)
		if lease == nil || lease.peer != (meshid.Device{}) {
			t.Fatal("pre-TLS admission has no lease or invented peer identity")
		}
	}
	if got := claims.policy.Counts(); got.Total != LANMaxConnections || got.Outgoing != 1 || got.Ready != 0 {
		t.Fatalf("shared pending count: %+v", got)
	}
	for _, gate := range []func(context.Context, *quic.ClientInfo) (context.Context, error){ethernet, wifi} {
		if _, err := gate(context.Background(), info); err == nil {
			t.Fatal("second interface bypassed shared pending cap")
		}
	}
	outgoing.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := wifi(ctx, info); err != nil {
		t.Fatalf("released outgoing capacity unavailable to incoming: %v", err)
	}
}
