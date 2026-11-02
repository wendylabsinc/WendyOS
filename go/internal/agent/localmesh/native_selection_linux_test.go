//go:build linux

package localmesh

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// An authenticated Ethernet replacement must release the native Wi-Fi
// attachment and its shared LAN lease, as the QUIC LAN carrier already does.
func TestNativeTLSRetiresWiFiAfterEthernetReplacement(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	self, peer := testCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := NewNode(ctx, t.TempDir(), self, "native-selection", 50052)
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
	policy := NewConnectionPolicy(self.Device, true, ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections})
	for _, cost := range []uint16{WiFiLinkCost, EthernetLinkCost} {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listenerDone := make(chan error, 1)
		go func() { listenerDone <- runNetTLSListener(ctx, node, listener, cost, policy, strconv.Itoa(int(cost))) }()
		defer func() { listener.Close(); <-listenerDone }()
		config, err := peer.PeerTLS(self.Device)
		if err != nil {
			t.Fatal(err)
		}
		config.NextProtos = []string{StreamALPN}
		raw, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		secure := tls.Client(raw, config)
		defer secure.Close()
		if err := secure.HandshakeContext(ctx); err != nil {
			t.Fatal(err)
		}
		if err := OpenStreamHello(ctx, secure, peer.Device, self.Device); err != nil {
			t.Fatal(err)
		}
		link := newStreamNodeLink(secure)
		defer link.Close()
		deadline := time.Now().Add(time.Second)
		for policy.Counts().Ready < 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if cost == WiFiLinkCost && policy.Counts().Ready != 1 {
			t.Fatal("Wi-Fi attachment missing")
		}
	}
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		links := node.Snapshot().Links
		if len(links) == 1 && links[0].Cost == EthernetLinkCost && policy.Counts().Total == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("native Wi-Fi survived Ethernet replacement: links=%+v leases=%+v", node.Snapshot().Links, policy.Counts())
}
