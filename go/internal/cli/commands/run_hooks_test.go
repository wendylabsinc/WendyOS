package commands

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func testPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}
	return port
}

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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	port := testPort(t, ln)

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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	addr := ln.Addr().String()
	port := testPort(t, ln)
	ln.Close()

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 10,
	}

	go func() {
		time.Sleep(1 * time.Second)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer l.Close()
		<-time.After(10 * time.Second)
	}()

	start := time.Now()
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if elapsed < 500*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~1-2s", elapsed)
	}
}

func TestWaitForReadiness_Timeout(t *testing.T) {
	// Grab a free port from the OS, then release it immediately so nothing listens on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := testPort(t, ln)
	ln.Close()

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 2,
	}

	start := time.Now()
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed < 1*time.Second || elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~2s", elapsed)
	}
}

func TestWaitForReadiness_ContextCancelled(t *testing.T) {
	// Grab a free port from the OS, then release it immediately so nothing listens on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := testPort(t, ln)
	ln.Close()

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 30,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err = waitForReadiness(ctx, cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	// Should return context.Canceled, not a timeout error.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
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
