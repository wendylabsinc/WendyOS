//go:build linux

package localmesh

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hashicorp/mdns"
)

// Exercise the real TLS listener, ordinary stream hello, shared admission,
// Node/TUN registration, and draining in a disposable Linux network namespace.
func TestNormalNativeTLSListenerAttachmentAndDrain(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	for _, closeListener := range []bool{false, true} {
		name := "context cancellation"
		if closeListener {
			name = "listener close"
		}
		t.Run(name, func(t *testing.T) {
			self, peer := testCredentials(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			node, err := NewNode(ctx, t.TempDir(), self, "native-fixture", 50052)
			if err != nil {
				t.Fatal(err)
			}
			nodeDone := make(chan error, 1)
			go func() { nodeDone <- node.Run() }()
			defer func() {
				cancel()
				if err := <-nodeDone; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			linkCtx, stopLink := context.WithCancel(ctx)
			defer stopLink()
			policy := NewConnectionPolicy(self.Device, true, ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections})
			done := make(chan error, 1)
			go func() { done <- runNetTLSListener(linkCtx, node, listener, EthernetLinkCost, policy, "fixture") }()
			defer listener.Close()
			config, err := peer.PeerTLS(self.Device)
			if err != nil {
				t.Fatal(err)
			}
			config.NextProtos = []string{StreamALPN}
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			secure := tls.Client(raw, config)
			if err := secure.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			if err := OpenStreamHello(ctx, secure, peer.Device, self.Device); err != nil {
				t.Fatal(err)
			}
			for len(node.Snapshot().Links) == 0 && ctx.Err() == nil {
				time.Sleep(5 * time.Millisecond)
			}
			view := node.Snapshot()
			if len(view.Links) != 1 || view.Links[0].Device != peer.Device || view.Links[0].Transport != "tls-stream" || policy.Counts().Ready != 1 {
				t.Fatalf("ordinary peer attachment missing: %+v, policy %+v", view.Links, policy.Counts())
			}
			if closeListener {
				_ = listener.Close()
			} else {
				stopLink()
			}
			select {
			case err := <-done:
				if !closeListener && err != nil {
					t.Fatal(err)
				}
				if closeListener && err == nil {
					t.Fatal("listener failure was not returned")
				}
			case <-ctx.Done():
				t.Fatal("native listener did not drain", ctx.Err())
			}
			if len(node.Snapshot().Links) != 0 || policy.Counts().Total != 0 {
				t.Fatal("native attachment or lease survived listener shutdown")
			}
		})
	}
}

func TestNormalNativeTLSLANDialsLitePeer(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	self, peer := testCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	node, err := NewNode(ctx, t.TempDir(), self, "native-dial-fixture", 50052)
	if err != nil {
		t.Fatal(err)
	}
	nodeDone := make(chan error, 1)
	go func() { nodeDone <- node.Run() }()
	defer func() {
		cancel()
		if err := <-nodeDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ip", "addr", "add", "192.0.2.1/24", "dev", "lo").CombinedOutput(); err != nil {
		t.Fatalf("fixture address: %v %s", err, out)
	}
	defer exec.Command("ip", "addr", "del", "192.0.2.1/24", "dev", "lo").Run()
	selected := lanInterface{iface: *iface, ip: net.IPv4(192, 0, 2, 1), net: &net.IPNet{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)}, cost: EthernetLinkCost, transport: "ethernet"}
	listener, err := net.Listen("tcp4", "192.0.2.1:43024")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer raw.Close()
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		config, err := nativeServerTLS(peer)
		if err != nil {
			serverDone <- err
			return
		}
		secure := tls.Server(raw, config)
		if err = secure.HandshakeContext(ctx); err == nil {
			err = OpenStreamHello(ctx, secure, peer.Device, self.Device)
		}
		if err != nil {
			serverDone <- err
			return
		}
		link := newStreamNodeLink(secure)
		defer link.Close()
		// Reading a real bootstrap control witnesses that scanner admission
		// completed and ordinary peer publication started on this carrier.
		_, err = link.ReadControl()
		serverDone <- err
		<-ctx.Done()
	}()
	entry := &mdns.ServiceEntry{Port: TLSLANPort, AddrV4: selected.ip, InfoFields: []string{"platform=Lite", "protocol=tls-v1", "version=1", "role=router", "principal=" + peer.Device.Principal(), "mesh=" + lanMeshHash(peer.Device.Tenant())}}
	claims := &lanClaims{policy: NewConnectionPolicy(self.Device, true, ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections})}
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanTLSLANWithQuery(ctx, LANConfig{Credentials: self, Node: node}, selected, claims, func(context.Context, lanInterface, string) ([]*mdns.ServiceEntry, error) {
			return []*mdns.ServiceEntry{entry}, nil
		})
	}()
	defer func() { cancel(); <-scanDone }()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Lite discovery did not attach", ctx.Err())
	}
	view := node.Snapshot()
	if len(view.Links) != 1 || view.Links[0].Device != peer.Device || view.Links[0].Transport != "ethernet" || claims.policy.Counts().Ready != 1 {
		t.Fatalf("Lite TCP carrier not admitted: %+v", view.Links)
	}
}
