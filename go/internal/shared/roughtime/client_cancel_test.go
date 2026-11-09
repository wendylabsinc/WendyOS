package roughtime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"testing"
	"time"
)

func TestQueryServerWithNonceCancellationDuringDNS(t *testing.T) {
	old := net.DefaultResolver
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("fixture DNS released")
		}
	}}
	defer func() { net.DefaultResolver = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := QueryServerWithNonce(ctx, Server{Name: "fixture", Address: "wn-clock-cancel-fixture.invalid:2002", PublicKey: make(ed25519.PublicKey, 32)}, make([]byte, 32))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("DNS fixture not entered")
	}
	cancel()
	returned := false
	select {
	case err := <-done:
		returned = true
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancellation returned %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Error("query ignored caller cancellation while resolving fixed authority")
	}
	close(release)
	if !returned {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("query did not finish after fixture release")
		}
	}
}

func TestQueryServerWithNonceCancellationDuringUDPRead(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := QueryServerWithNonce(ctx, Server{Name: "fixture", Address: listener.LocalAddr().String(), PublicKey: make(ed25519.PublicKey, 32)}, make([]byte, 32))
		done <- err
	}()
	listener.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	_, client, err := listener.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	returned := false
	select {
	case err := <-done:
		returned = true
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancellation returned %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Error("query ignored caller cancellation while awaiting fixed-authority UDP reply")
	}
	if !returned {
		listener.WriteTo([]byte("release invalid response"), client)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("query did not finish after fixture release")
		}
	}
}
