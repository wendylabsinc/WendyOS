package meshsession

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

func TestLocalUDPAppEchoAndIncarnationRevocation(t *testing.T) {
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		buf := make([]byte, MaxUDPPayload+1)
		for {
			n, a, e := app.ReadFromUDP(buf)
			if e != nil {
				return
			}
			_, _ = app.WriteToUDP(buf[:n], a)
		}
	}()
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	r := meshingress.NewRegistry()
	if _, err := DialLocalUDP(context.Background(), r, port); err == nil {
		t.Fatal("host UDP listener admitted")
	}
	if err := r.ClaimForApp("tcp", "com.wendy.tcp", port); err != nil {
		t.Fatal(err)
	}
	if _, err := DialLocalUDP(context.Background(), r, port); err == nil {
		t.Fatal("TCP claim admitted UDP")
	}
	if err := r.ClaimUDPForApp("echo", "com.wendy.echo", port); err != nil {
		t.Fatal(err)
	}
	flow, err := DialLocalUDP(context.Background(), r, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, payload := range [][]byte{[]byte("self VIP"), bytes.Repeat([]byte{42}, MaxUDPPayload)} {
		if err := flow.Send(payload); err != nil {
			t.Fatal(err)
		}
		got, err := flow.Receive(ctx)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("echo %v %v", got, err)
		}
	}
	for _, payload := range [][]byte{nil, bytes.Repeat([]byte{42}, MaxUDPPayload+1)} {
		if flow.Send(payload) == nil {
			t.Fatal("invalid payload accepted")
		}
	}
	if err := flow.Send([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	r.Release("echo")
	if err := r.ClaimUDPForApp("replacement", "com.wendy.replacement", port); err != nil {
		t.Fatal(err)
	}
	if err := flow.Send([]byte("old flow")); !errors.Is(err, meshingress.ErrPortDenied) {
		t.Fatalf("revoked send: %v", err)
	}
	if _, err := flow.Receive(ctx); !errors.Is(err, meshingress.ErrPortDenied) {
		t.Fatalf("revoked receive: %v", err)
	}
	fresh, err := DialLocalUDP(ctx, r, port)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Send([]byte("new flow")); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Receive(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLocalUDPReceiveCancellationAndClose(t *testing.T) {
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	r := meshingress.NewRegistry()
	if err := r.ClaimUDPForApp("silent", "com.wendy.silent", port); err != nil {
		t.Fatal(err)
	}
	flow, err := DialLocalUDP(context.Background(), r, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialLocalUDP(ctx, r, port); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := flow.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := flow.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := flow.Receive(context.Background()); done <- err }()
	_ = flow.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close failed to unblock receive")
	}
}
