package meshsession

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

func TestNormalTLSDatagramEchoAndRouteLifecycle(t *testing.T) {
	testNormalDatagramEchoAndRouteLifecycle(t, true)
}

func TestNormalQUICDatagramEchoAndRouteLifecycle(t *testing.T) {
	testNormalDatagramEchoAndRouteLifecycle(t, false)
}

func testNormalDatagramEchoAndRouteLifecycle(t *testing.T, plainTLS bool) {
	a, b := fixtureCredentials(t)
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	go func() {
		buf := make([]byte, MaxUDPMessageSize+1)
		for {
			n, address, err := app.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = app.WriteToUDP(buf[:n], address)
		}
	}()
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	registry := meshingress.NewRegistry()
	if err := registry.ClaimUDPForApp("echo", "com.wendy.echo", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	var address netip.AddrPort
	if plainTLS {
		_, address = startTLSAppTest(t, b, testUDPAuthorization{registry})
	} else {
		server, err := NewServer(b, testUDPAuthorization{registry})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
		t.Cleanup(func() {
			cancel()
			server.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("UDP server did not stop")
			}
		})
		until := time.Now().Add(3 * time.Second)
		for server.Addr() == nil {
			if time.Now().After(until) {
				t.Fatal("UDP listener did not start")
			}
			time.Sleep(time.Millisecond)
		}
		address = netip.MustParseAddrPort(server.Addr().String())
	}
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	setup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	var flow interface {
		Send([]byte) error
		Receive(context.Context) (*udpmessage.Message, error)
		Close() error
		Done() <-chan struct{}
	}
	if plainTLS {
		flow, err = client.DialUDPTLS(setup, b.Device, address, port)
	} else {
		flow, err = client.DialUDP(setup, b.Device, address, port)
	}
	cancel() // A setup context does not own the admitted flow's lifetime.
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	for _, size := range []int{0, 1, 255, 256, 1232, 4096, localEchoMaximum()} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i*31 + size)
		}
		if err := flow.Send(payload); err != nil {
			t.Fatalf("send %d: %v", size, err)
		}
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		got, err := readUDPMessage(flow, ctx)
		stop()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("message %d: len=%d error=%v", size, len(got), err)
		}
	}
	// Ordinary idle reads may be canceled without losing frame state.
	idle, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, _ = flow.Receive(idle)
	stop()
	if err := flow.Send([]byte("after idle")); err != nil {
		t.Fatal(err)
	}
	read, stopRead := context.WithTimeout(context.Background(), 3*time.Second)
	got, err := readUDPMessage(flow, read)
	stopRead()
	if err != nil || string(got) != "after idle" {
		t.Fatal("ordinary idle resume", err)
	}
	client.WithdrawRoute(b.Device)
	select {
	case <-flow.Done():
	case <-time.After(time.Second):
		t.Fatal("withdrawn UDP flow did not retire")
	}
}
