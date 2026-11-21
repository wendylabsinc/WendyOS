package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testPanel(t *testing.T, socket string, open bool) (*panel, *httptest.Server) {
	t.Helper()
	password := "test-password"
	if open {
		password = ""
	}
	p, err := newPanel(socket, "", password, open, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "device panel assets") }))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, s := range p.sessions {
			s.cancel()
		}
	})
	return p, server
}
func request(t *testing.T, server *httptest.Server, method, path, origin, body string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}
func login(t *testing.T, server *httptest.Server) *http.Cookie {
	t.Helper()
	response := request(t, server, "POST", "/api/session", server.URL, `{"password":"test-password"}`, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("login: %s", response.Status)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie is not protected")
	}
	return cookies[0]
}
func TestAuthenticationAndOrigins(t *testing.T) {
	p, server := testPanel(t, "/missing/socket", false)
	for _, test := range []struct {
		method, path, origin, body string
		status                     int
	}{
		{"GET", "/", "", "", 200},
		{"GET", "/api/agent", server.URL, "", 401},
		{"POST", "/api/session", "", `{"password":"test-password"}`, 403},
		{"POST", "/api/session", "https://attacker.example", `{"password":"test-password"}`, 403},
		{"POST", "/api/session", server.URL, `{"password":"wrong"}`, 401},
		{"POST", "/api/session", server.URL, `{`, 400},
	} {
		r := request(t, server, test.method, test.path, test.origin, test.body, nil)
		if r.StatusCode != test.status {
			t.Errorf("%s %s origin=%q: %s", test.method, test.path, test.origin, r.Status)
		}
	}
	cookie := login(t, server)
	if r := request(t, server, "GET", "/api/agent", server.URL, "", cookie); r.StatusCode != 502 {
		t.Errorf("expected upstream failure, got %s", r.Status)
	}
	if r := request(t, server, "GET", "/api/agent", "https://attacker.example", "", cookie); r.StatusCode != 403 {
		t.Errorf("cross-origin socket: %s", r.Status)
	}
	if r := request(t, server, "DELETE", "/api/session", server.URL, "", cookie); r.StatusCode != 204 {
		t.Errorf("logout: %s", r.Status)
	}
	if r := request(t, server, "GET", "/api/agent", server.URL, "", cookie); r.StatusCode != 401 {
		t.Errorf("revoked cookie accepted: %s", r.Status)
	}
	req := httptest.NewRequest("GET", "http://attacker.example/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("DNS rebinding host accepted")
	}
}
func TestExplicitOpenAndConfiguration(t *testing.T) {
	files := http.NotFoundHandler()
	for _, args := range []struct {
		socket, origin, password string
		open                     bool
	}{
		{"", "", "password", false}, {"/socket", "", "", false}, {"/socket", "", "password", true}, {"/socket", "http://device/path", "password", false}, {"/socket", "http://user@device", "password", false},
	} {
		if _, err := newPanel(args.socket, args.origin, args.password, args.open, files); err == nil {
			t.Errorf("accepted invalid configuration: %+v", args)
		}
	}
	_, server := testPanel(t, "/missing/socket", true)
	if r := request(t, server, "GET", "/api/agent", server.URL, "", nil); r.StatusCode != 502 {
		t.Errorf("open socket should dial upstream: %s", r.Status)
	}
	if r := request(t, server, "GET", "/api/agent", "https://attacker.example", "", nil); r.StatusCode != 403 {
		t.Errorf("open mode lost origin check: %s", r.Status)
	}
	p, err := newPanel("/socket", "https://panel.example", "password", false, files)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "https://panel.example/api/session", nil)
	if origin, ok := p.requestOrigin(req); !ok || origin != "https://panel.example" {
		t.Fatal("configured origin rejected")
	}
	req.Host = "attacker.example"
	if _, ok := p.requestOrigin(req); ok {
		t.Fatal("wrong host accepted with explicit origin")
	}
}
func TestTunnelLargeFramesAndLogout(t *testing.T) {
	// Keep the path short enough for macOS sockaddr_un.
	dir := t.TempDir()
	socket := filepath.Join(dir, "a.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ended := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer close(ended)
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	_, server := testPanel(t, socket, false)
	cookie := login(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL+"/api/agent", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {server.URL}, "Cookie": {cookie.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	stream := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	payload := bytes.Repeat([]byte("grpc-stream"), 32768)
	written := make(chan error, 1)
	go func() { _, err := stream.Write(payload); written <- err }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatal("relay changed bytes")
	}
	request(t, server, "DELETE", "/api/session", server.URL, "", cookie)
	select {
	case <-ended:
	case <-ctx.Done():
		t.Fatal("logout left an admin socket open")
	}
}
func TestExpiredSessionClosesAuthority(t *testing.T) {
	p, server := testPanel(t, "/missing/socket", false)
	cookie := login(t, server)
	p.mu.Lock()
	s := p.sessions[cookie.Value]
	s.expires = time.Now().Add(-time.Second)
	p.sessions[cookie.Value] = s
	p.mu.Unlock()
	if r := request(t, server, "GET", "/api/agent", server.URL, "", cookie); r.StatusCode != 401 {
		t.Fatal("expired session accepted")
	}
	if s.ctx.Err() == nil {
		t.Fatal("expired session was not cancelled")
	}
}
