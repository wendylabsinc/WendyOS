package meshsession

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

type testUDPAuthorization struct{ *meshingress.Registry }

func (a testUDPAuthorization) DialAuthorized(port uint16, dial func() (net.Conn, error)) (net.Conn, error) {
	return a.Registry.DialAuthorized(port, dial)
}

func TestAuthenticatedUDPFlowEchoAndPortDenial(t *testing.T) {
	a, b := fixtureCredentials(t)
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		buf := make([]byte, MaxUDPPayload+1)
		for {
			n, addr, err := app.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = app.WriteToUDP(buf[:n], addr)
		}
	}()
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	registry := meshingress.NewRegistry()
	if err := registry.ClaimUDPForApp("echo", "com.wendy.echo", port); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(b, testUDPAuthorization{registry})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	var endpoint netip.AddrPort
	for i := 0; i < 100; i++ {
		if addr := server.Addr(); addr != nil {
			endpoint = netip.MustParseAddrPort(addr.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !endpoint.IsValid() {
		t.Fatal("QUIC listener did not start")
	}
	flow, err := DialUDP(ctx, a, b.Asset, endpoint, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	for _, payload := range [][]byte{[]byte("hello"), bytes.Repeat([]byte{42}, MaxUDPPayload)} {
		if err := flow.Send(payload); err != nil {
			t.Fatal(err)
		}
		got, err := flow.Receive(ctx)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("UDP echo = %v, %v", got, err)
		}
	}
	if err := flow.Send(bytes.Repeat([]byte{1}, MaxUDPPayload+1)); err == nil {
		t.Fatal("oversize UDP payload accepted")
	}
	if _, err := DialUDP(ctx, a, b.Asset, endpoint, port+1); !errors.Is(err, ErrDenied) {
		t.Fatalf("unpublished UDP port: %v", err)
	}
	if _, err := DialUDP(ctx, a, b.Asset+1, endpoint, port); err == nil {
		t.Fatal("wrong peer asset accepted")
	}
	registry.Release("echo")
	if err := registry.ClaimUDPForApp("replacement", "com.wendy.replacement", port); err != nil {
		t.Fatal(err)
	}
	if err := flow.Send([]byte("after stop")); err == nil {
		recvCtx, cancelRecv := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancelRecv()
		if got, err := flow.Receive(recvCtx); err == nil {
			t.Fatalf("revoked app received reply %q", got)
		}
	}
	replacement, err := DialUDP(ctx, a, b.Asset, endpoint, port)
	if err != nil {
		t.Fatalf("replacement UDP session: %v", err)
	}
	defer replacement.Close()
	if err := replacement.Send([]byte("new owner")); err != nil {
		t.Fatal(err)
	}
	if got, err := replacement.Receive(ctx); err != nil || !bytes.Equal(got, []byte("new owner")) {
		t.Fatalf("replacement echo = %q, %v", got, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
