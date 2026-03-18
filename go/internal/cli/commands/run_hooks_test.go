package commands

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func TestWaitForReadiness_NilConfig(t *testing.T) {
	err := waitForReadiness(context.Background(), nil, "localhost")
	if err != nil {
		t.Fatalf("expected nil error for nil config, got %v", err)
	}
}

func TestWaitForReadiness_NilTCPSocket(t *testing.T) {
	cfg := &appconfig.ReadinessConfig{}
	err := waitForReadiness(context.Background(), cfg, "localhost")
	if err != nil {
		t.Fatalf("expected nil error for nil tcpSocket, got %v", err)
	}
}

func TestWaitForReadiness_PortAlreadyListening(t *testing.T) {
	// Start a TCP listener on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 5,
	}

	start := time.Now()
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected near-instant for already listening port", elapsed)
	}
}

func TestWaitForReadiness_PortBecomesAvailable(t *testing.T) {
	// Find a free port by binding and immediately closing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	addr := ln.Addr().String()
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	ln.Close() // Port is now free (nothing listening).

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 10,
	}

	// Start listening after a short delay.
	go func() {
		time.Sleep(1 * time.Second)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer l.Close()
		// Keep listening until test completes.
		<-time.After(10 * time.Second)
	}()

	start := time.Now()
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	// Should take ~1s (the delay) plus polling interval.
	if elapsed < 500*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~1-2s", elapsed)
	}
}

func TestWaitForReadiness_Timeout(t *testing.T) {
	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: 19999}, // Nothing listening.
		TimeoutSeconds: 2,
	}

	start := time.Now()
	err := waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed < 1*time.Second || elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~2s", elapsed)
	}
}

func TestWaitForReadiness_ContextCancelled(t *testing.T) {
	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: 19999},
		TimeoutSeconds: 30,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := waitForReadiness(ctx, cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v, expected ~500ms (context cancel)", elapsed)
	}
}

func TestStartPostStartHook_NilHooks(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "test"}
	cmd := startPostStartHook(context.Background(), cfg, "localhost")
	if cmd != nil {
		t.Error("expected nil cmd for nil hooks")
	}
}

func TestStartPostStartHook_EmptyCLI(t *testing.T) {
	cfg := &appconfig.AppConfig{
		AppID: "test",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{Agent: "echo agent-only"},
		},
	}
	cmd := startPostStartHook(context.Background(), cfg, "localhost")
	if cmd != nil {
		t.Error("expected nil cmd when CLI is empty")
	}
}
