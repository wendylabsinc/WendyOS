package main

import (
	"context"
	"crypto/tls"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoopbackAddressRejectsDNSAndNamedPorts(t *testing.T) {
	for _, value := range []string{"localhost:1234", "example.invalid:80", "127.0.0.1:http", "127.0.0.1:+80", "127.0.0.1:-1", "127.0.0.1:65536", ":1234", "0.0.0.0:80", "192.168.1.2:80", "[::]:80", "[::1%lo0]:80", "127.0.0.1:"} {
		if _, err := loopbackAddress(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"127.0.0.1:0", "127.0.0.2:18882", "[::1]:65535"} {
		if _, err := loopbackAddress(value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
}
func TestInputFilesBoundedPrivateAndNoSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private.json")
	secret := "test-secret-do-not-echo"
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := readFile(path, len(secret), true); err != nil || string(b) != secret {
		t.Fatal(err)
	}
	check := func(path string, limit int, private bool) {
		t.Helper()
		_, err := readFile(path, limit, private)
		if err == nil {
			t.Fatalf("accepted %s", path)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), path) {
			t.Fatalf("input details leaked: %s", err)
		}
	}
	check(path, len(secret)-1, true)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	check(path, 1024, true)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	check(link, 1024, false)
	check(dir, 1024, false)
	check(filepath.Join(dir, secret), 1024, false)
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readFile(fifo, 1024, false); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted fifo")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked startup")
	}
}
func TestBoundedListenerLimitsAcceptedConnectionsAndReleasesOnce(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &boundedListener{Listener: base, slots: make(chan struct{}, 1), done: make(chan struct{})}
	defer listener.Close()
	first, err := net.DialTimeout("tcp", base.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	second, err := net.DialTimeout("tcp", base.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() { c, e := listener.Accept(); ch <- result{c, e} }()
	select {
	case r := <-ch:
		if r.conn != nil {
			r.conn.Close()
		}
		t.Fatal("accepted over limit")
	case <-time.After(30 * time.Millisecond):
	}
	if err := accepted.Close(); err != nil {
		t.Fatal(err)
	}
	_ = accepted.Close()
	var next net.Conn
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		next = r.conn
	case <-time.After(time.Second):
		t.Fatal("slot not released")
	}
	defer next.Close()
	go func() { c, e := listener.Accept(); ch <- result{c, e} }()
	_ = listener.Close()
	select {
	case r := <-ch:
		if !errors.Is(r.err, net.ErrClosed) {
			t.Fatalf("close did not wake accept: %v", r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("close blocked")
	}
}
func TestBoundedHTTPShutdownClosesIdleConnections(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &boundedListener{Listener: base, slots: make(chan struct{}, 2), done: make(chan struct{})}
	tlsFixture := httptest.NewTLSServer(http.NotFoundHandler())
	tlsConfig := tlsFixture.TLS.Clone()
	tlsFixture.Close()
	tlsConfig.MinVersion = tls.VersionTLS13
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }), ReadHeaderTimeout: 100 * time.Millisecond, IdleTimeout: 100 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(listener, tlsConfig)) }()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", base.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: local\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 1024)
	if _, err = conn.Read(b); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	if len(listener.slots) != 0 {
		t.Fatalf("connections retained: %d", len(listener.slots))
	}
}
