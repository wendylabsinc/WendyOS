//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sync/errgroup"
)

// The incumbent's remote endpoint is controlled. Its local attachment, TUN,
// route installation and packet forwarding are the production implementations.
type duplicateRouteLink struct {
	*teardownTestLink
	packets chan []byte
}

func (l *duplicateRouteLink) SendIP(_ uint32, packet []byte) error {
	select {
	case l.packets <- append([]byte(nil), packet...):
		return nil
	case <-l.done:
		return net.ErrClosed
	}
}

// Reject real authenticated QUIC duplicates while an incumbent forwards host
// UDP through its real kernel route. Observe the actor's registration boundary
// as well as the final state: a transient add followed by removal is a failure.
func TestDuplicateCarrierPreservesLiveRoute(t *testing.T) {
	if os.Getenv("WENDY_PACKET_BACKEND_NAMESPACE") != "1" {
		t.Skip("requires the disposable Linux network CI fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("kernel fixture requires root")
	}
	for _, ns := range []string{"mnt", "net", "pid"} {
		parent := os.Getenv("WENDY_PACKET_PARENT_" + ns)
		current, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("kernel fixture lacks an isolated %s namespace", ns)
		}
	}
	peer, self := testCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node, err := NewNode(ctx, t.TempDir(), self, "duplicate-route", 50052)
	if err != nil {
		t.Fatal(err)
	}
	link := &duplicateRouteLink{teardownTestLink: &teardownTestLink{done: make(chan struct{}), reading: make(chan struct{})}, packets: make(chan []byte, 16)}
	claims := lanTestClaims(self)
	incumbent := claims.policy.ReserveGroup("incumbent", peer.Device, false, strconv.Itoa(int(EthernetLinkCost)))
	if incumbent == nil {
		t.Fatal("initial connection reservation failed")
	}
	defer incumbent.Release()
	attached := make(chan error, 1)
	var serverDone chan error
	t.Cleanup(func() {
		cancel()
		node.cancel()
		_ = link.Close()
		if err := node.kernel.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-attached:
		case <-time.After(3 * time.Second):
			t.Error("incumbent attachment did not stop")
		}
		if serverDone != nil {
			select {
			case err := <-serverDone:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("QUIC listener did not stop")
			}
		}
	})
	go func() {
		attached <- node.attachLink(WithConnectionLease(ctx, incumbent), peer.Device, EthernetLinkCost, link)
	}()
	var registration nodeEvent
	select {
	case registration = <-node.events:
	case <-ctx.Done():
		t.Fatal("incumbent registration did not arrive")
	}
	if registration.add == nil {
		t.Fatal("incumbent did not register a link")
	}
	err = node.handle(registration)
	registration.result <- err
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-link.reading:
	case <-ctx.Done():
		t.Fatal("incumbent workers did not start")
	}
	_, nextHop, err := meshid.LinkLocalAddresses(self.Device, peer.Device)
	if err != nil {
		t.Fatal(err)
	}
	// The route is an input fixture. Apply/Resume install it in both the real
	// kernel and the forwarding projection used by the production TUN worker.
	route := babel.Route{Prefix: netip.PrefixFrom(peer.Device.MeshAddress(), 128), Link: registration.add.id, NextHop: nextHop, Metric: EthernetLinkCost}
	err = node.Apply([]babel.Route{route})
	resumeErr := node.Resume()
	if err != nil || resumeErr != nil {
		t.Fatal(errors.Join(err, resumeErr))
	}
	node.refresh()
	incumbentID := registration.add.id
	kernelLink := node.kernel.(*hostNetwork).peers[incumbentID].link
	index := kernelLink.Attrs().Index
	rejectEvent := func(event nodeEvent) {
		t.Helper()
		if event.result != nil {
			event.result <- errors.New("test rejects unexpected registration")
		}
		t.Fatalf("rejected duplicate published a routing link: add=%v remove=%v", event.add != nil, event.remove)
	}
	udp, err := net.DialUDP("udp6", &net.UDPAddr{IP: net.IP(self.Device.MeshAddress().AsSlice())}, &net.UDPAddr{IP: net.IP(peer.Device.MeshAddress().AsSlice()), Port: 43210})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	forward := func(marker string) {
		t.Helper()
		payload := []byte("duplicate-route:" + marker)
		if _, err := udp.Write(payload); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case packet := <-link.packets:
				if len(packet) < 48 || packet[6] != 17 {
					continue // Ignore kernel neighbor discovery.
				}
				if !bytes.Equal(packet[8:24], self.Device.MeshAddress().AsSlice()) || !bytes.Equal(packet[24:40], peer.Device.MeshAddress().AsSlice()) || !bytes.Equal(packet[48:], payload) {
					t.Fatalf("incumbent forwarded the wrong UDP packet for %s: %x", marker, packet)
				}
				return
			case event := <-node.events:
				rejectEvent(event)
			case <-deadline.C:
				t.Fatalf("incumbent route stopped forwarding: %s", marker)
			}
		}
	}
	forward("before-duplicate")
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	endpoint := socket.LocalAddr()
	mdns, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer mdns.Close()
	selected := lanInterface{iface: net.Interface{Name: "not-a-real-lan"}, ip: net.IPv4(127, 0, 0, 1), net: &net.IPNet{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}, cost: EthernetLinkCost}
	ready := make(chan CarrierStatus, 1)
	serverDone = make(chan error, 1)
	go func() {
		serverDone <- serveLANInterface(ctx, LANConfig{Credentials: self, Node: node, Status: func(status CarrierStatus) { ready <- status }}, selected, claims, socket, mdns, nil)
	}()
	select {
	case status := <-ready:
		if !status.Ready || status.Err != nil {
			t.Fatalf("QUIC listener not ready: %+v", status)
		}
	case <-ctx.Done():
		t.Fatal("QUIC listener did not become ready")
	}
	clientSocket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientSocket.Close()
	client := &quic.Transport{Conn: clientSocket}
	defer client.Close()
	var hellos errgroup.Group
	hellos.SetLimit(1)
	defer func() { cancel(); _ = hellos.Wait() }()
	for trial := 1; trial <= 3; trial++ {
		tls, err := peer.PeerTLS(self.Device)
		if err != nil {
			t.Fatal(err)
		}
		duplicate, err := client.Dial(ctx, endpoint, tls, QUICConfig())
		if err != nil {
			t.Fatalf("authenticated duplicate %d could not reach the shared listener: %v", trial, err)
		}
		helloDone := make(chan struct{})
		hellos.Go(func() error {
			// The peer may close before the hello response is consumed.
			_, _ = OpenControl(ctx, duplicate, peer.Device, self.Device)
			close(helloDone)
			return nil
		})
		forward(fmt.Sprintf("during-duplicate-%d", trial))
		select {
		case <-duplicate.Context().Done():
		case event := <-node.events:
			rejectEvent(event)
		case <-ctx.Done():
			t.Fatal("authenticated duplicate was not promptly rejected")
		}
		select {
		case <-helloDone:
		case <-ctx.Done():
			t.Fatal("rejected duplicate retained its hello worker")
		}
		forward(fmt.Sprintf("after-duplicate-%d", trial))
		select {
		case event := <-node.events:
			rejectEvent(event)
		default:
		}
		if claims.policy.Counts().Ready != 1 || len(node.peers) != 1 || node.nextLink != incumbentID {
			t.Fatal("duplicate changed incumbent readiness or published another link")
		}
		live, err := netlink.LinkByName(kernelLink.Name)
		if err != nil || live.Attrs().Index != index {
			t.Fatalf("duplicate replaced the incumbent TUN: %v", err)
		}
		if _, err := registration.add.tun.(*os.File).Stat(); err != nil {
			t.Fatalf("duplicate closed the incumbent TUN: %v", err)
		}
		routes, err := netlink.RouteGet(net.IP(peer.Device.MeshAddress().AsSlice()))
		if err != nil || len(routes) != 1 || routes[0].LinkIndex != index {
			t.Fatalf("duplicate disturbed the live route: %v, %v", routes, err)
		}
		if snapshot := node.Snapshot(); snapshot.Peers != 1 || len(snapshot.Links) != 1 || snapshot.Links[0].ID != incumbentID {
			t.Fatalf("duplicate changed the published incumbent: %+v", snapshot.Links)
		}
		t.Logf("authenticated duplicate %d rejected; original TUN and route still forward UDP", trial)
	}
	// Prove the same socket, credentials and hello can reach registration once
	// the incumbent claim is explicitly released. A broken handshake or an
	// always-rejecting listener must not make the duplicate checks pass.
	incumbent.Release()
	tls, err := peer.PeerTLS(self.Device)
	if err != nil {
		t.Fatal(err)
	}
	control, err := client.Dial(ctx, endpoint, tls, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	hellos.Go(func() error {
		_, _ = OpenControl(ctx, control, peer.Device, self.Device)
		return nil
	})
	select {
	case event := <-node.events:
		if event.add == nil || event.add.device != peer.Device {
			t.Fatal("released claim did not admit the authenticated control peer")
		}
		// Stop the positive control at registration after proving admission.
		event.result <- errors.New("positive control stops before allocating another TUN")
	case <-ctx.Done():
		t.Fatal("listener rejected the control after the incumbent claim was released")
	}
	select {
	case <-control.Context().Done():
	case <-ctx.Done():
		t.Fatal("positive control did not finish")
	}
	t.Log("same authenticated peer reached registration after the incumbent claim was released")
}
