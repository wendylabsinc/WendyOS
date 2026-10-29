package meshsession

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

// This relay adds latency without packet loss or a throughput cap. The real
// client/server still negotiate mTLS, authorize a UDP port and echo a nonce.
// A learned RTT must replace the seed, and completed setup must become quiet.
func TestAppQUICLearnsDelayedPathAndQuiesces(t *testing.T) {
	a, b := fixtureCredentials(t)
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		buf := make([]byte, MaxUDPMessageSize)
		for {
			n, peer, err := app.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = app.WriteToUDP(buf[:n], peer)
		}
	}()
	t.Cleanup(func() { _ = app.Close(); <-appDone })
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	registry := meshingress.NewRegistry()
	if err := registry.ClaimUDPForApp("delayed", "com.wendy.delayed", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(b, testUDPAuthorization{registry})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, "127.0.0.1:0") }()
	t.Cleanup(func() { cancel(); _ = server.Close(); <-serverDone })
	for server.Addr() == nil {
		if ctx.Err() != nil {
			t.Fatal("server did not start", ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	endpoint := delayedAppRelay(t, server.Addr().(*net.UDPAddr), 250*time.Millisecond)
	flow, err := DialUDP(ctx, a, b.Device, endpoint, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	nonce := []byte("authorized delayed-path nonce")
	if err := flow.Send(nonce); err != nil {
		t.Fatal(err)
	}
	got, err := readUDPMessage(flow, ctx)
	if err != nil || !bytes.Equal(got, nonce) {
		t.Fatalf("delayed authorized echo failed: %q %v", got, err)
	}
	stats := flow.conn.ConnectionStats()
	if stats.MinRTT < 400*time.Millisecond || stats.MinRTT > 2*time.Second || stats.MeanDeviation <= 0 {
		t.Fatalf("real path RTT was not learned: %+v", stats)
	}
	if flow.conn.ConnectionState().Used0RTT {
		t.Fatal("app session unexpectedly used early data")
	}
	// Allow all setup ACKs two further round trips to settle, then observe
	// another two round trips. Repeating old reliable setup frames indefinitely
	// would keep increasing the packet count despite the idle application.
	time.Sleep(2 * time.Second)
	before := flow.conn.ConnectionStats().PacketsSent
	time.Sleep(time.Second)
	if after := flow.conn.ConnectionStats().PacketsSent; after-before > 2 {
		t.Fatalf("completed UDP setup keeps retransmitting: before=%d after=%d", before, after)
	}
}

func delayedAppRelay(t *testing.T, server *net.UDPAddr, delay time.Duration) netip.AddrPort {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	stop, readDone := make(chan struct{}), make(chan struct{})
	var sends sync.WaitGroup
	go func() {
		defer close(readDone)
		var client *net.UDPAddr
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			target := server
			if from.String() == server.String() {
				target = client
			} else {
				client = from
			}
			if target == nil {
				continue
			}
			packet := append([]byte(nil), buf[:n]...)
			sends.Add(1)
			go func() {
				defer sends.Done()
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-stop:
				case <-timer.C:
					_, _ = conn.WriteToUDP(packet, target)
				}
			}()
		}
	}()
	t.Cleanup(func() { close(stop); _ = conn.Close(); <-readDone; sends.Wait() })
	return conn.LocalAddr().(*net.UDPAddr).AddrPort()
}
