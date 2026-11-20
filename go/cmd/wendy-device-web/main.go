// wendy-device-web serves the device panel and bridges browser gRPC to the
// admin-entitled container's Unix socket. It never chooses a remote device.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"
)

const sessionCookie = "wendy-device-session"

type session struct {
	expires time.Time
	ctx     context.Context
	cancel  context.CancelFunc
}
type panel struct {
	socket, origin string
	password       [32]byte
	open           bool
	files          http.Handler
	limiter        *rate.Limiter
	mu             sync.Mutex
	sessions       map[string]session
}

func newPanel(socket, origin, password string, open bool, files http.Handler) (*panel, error) {
	if socket == "" {
		return nil, errors.New("WENDY_AGENT_SOCKET is required; run with the admin entitlement")
	}
	if password == "" && !open {
		return nil, errors.New("set WENDY_WEB_PASSWORD or explicitly set WENDY_WEB_ALLOW_OPEN=true")
	}
	if password != "" && open {
		return nil, errors.New("choose password protection or open access, not both")
	}
	if origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("origin must be an exact HTTP(S) origin without a path")
		}
	}
	return &panel{socket: socket, origin: origin, password: sha256.Sum256([]byte(password)), open: open, files: files, limiter: rate.NewLimiter(1, 5), sessions: make(map[string]session)}, nil
}

func main() {
	listen := flag.String("listen", ":8080", "HTTP(S) listen address")
	files := flag.String("static", "/www", "device panel build directory")
	origin := flag.String("origin", os.Getenv("WENDY_WEB_ORIGIN"), "optional exact public origin; otherwise allow IP addresses, localhost and .local names")
	cert := flag.String("tls-cert", os.Getenv("WENDY_WEB_TLS_CERT"), "optional TLS certificate file")
	key := flag.String("tls-key", os.Getenv("WENDY_WEB_TLS_KEY"), "TLS private key file")
	flag.Parse()
	p, err := newPanel(os.Getenv("WENDY_AGENT_SOCKET"), *origin, os.Getenv("WENDY_WEB_PASSWORD"), os.Getenv("WENDY_WEB_ALLOW_OPEN") == "true", http.FileServer(http.Dir(*files)))
	if err != nil {
		log.Fatal(err)
	}
	if (*cert == "") != (*key == "") {
		log.Fatal("both TLS certificate and key are required")
	}
	server := &http.Server{Addr: *listen, Handler: p, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("Device panel listening at %s (open access: %t)", *listen, p.open)
	if *cert != "" {
		log.Fatal(server.ListenAndServeTLS(*cert, *key))
	}
	log.Fatal(server.ListenAndServe())
}

// Checking Host as well as Origin blocks DNS rebinding to an open panel. Custom
// DNS names and TLS reverse proxies require a configured public origin.
func (p *panel) requestOrigin(r *http.Request) (string, bool) {
	if p.origin != "" {
		u, _ := url.Parse(p.origin)
		return p.origin, r.Host == u.Host
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(host) == nil && host != "localhost" && !strings.HasSuffix(strings.ToLower(host), ".local") {
		return "", false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host, true
}

func (p *panel) authenticated(r *http.Request) (session, bool) {
	if p.open {
		return session{ctx: context.Background()}, true
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for token, s := range p.sessions {
		if time.Now().After(s.expires) {
			s.cancel()
			delete(p.sessions, token)
		}
	}
	s, ok := p.sessions[cookie.Value]
	return s, ok
}

func (p *panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin, valid := p.requestOrigin(r)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: data:; media-src 'self' blob:; connect-src 'self'; worker-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if !valid {
		http.Error(w, "Host not allowed; configure WENDY_WEB_ORIGIN for custom DNS names", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodGet {
		_, authenticated := p.authenticated(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": authenticated, "open": p.open})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Header.Get("Origin") != origin {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == "/api/session" && r.Method == http.MethodPost:
			p.login(w, r, strings.HasPrefix(origin, "https://"))
			return
		case r.URL.Path == "/api/session" && r.Method == http.MethodDelete:
			if cookie, err := r.Cookie(sessionCookie); err == nil {
				p.mu.Lock()
				if s, ok := p.sessions[cookie.Value]; ok {
					s.cancel()
					delete(p.sessions, cookie.Value)
				}
				p.mu.Unlock()
			}
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(origin, "https://"), SameSite: http.SameSiteStrictMode})
			w.WriteHeader(http.StatusNoContent)
			return
		case r.URL.Path == "/api/agent" && r.Method == http.MethodGet:
			s, ok := p.authenticated(r)
			if !ok {
				http.Error(w, "Sign in to this device", http.StatusUnauthorized)
				return
			}
			p.tunnel(w, r, s, origin)
			return
		default:
			http.NotFound(w, r)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Public assets contain no device data. Every connection to the socket is gated.
	p.files.ServeHTTP(w, r)
}

func (p *panel) login(w http.ResponseWriter, r *http.Request, secure bool) {
	if !p.limiter.Allow() {
		http.Error(w, "Too many attempts. Try again shortly.", http.StatusTooManyRequests)
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	hash := sha256.Sum256([]byte(input.Password))
	if !p.open && subtle.ConstantTimeCompare(hash[:], p.password[:]) != 1 {
		http.Error(w, "Incorrect device password", http.StatusUnauthorized)
		return
	}
	if !p.open {
		bytes := make([]byte, 32)
		if _, err := rand.Read(bytes); err != nil {
			http.Error(w, "Could not create session", http.StatusInternalServerError)
			return
		}
		token := hex.EncodeToString(bytes)
		expires := time.Now().Add(8 * time.Hour)
		ctx, cancel := context.WithDeadline(context.Background(), expires)
		p.mu.Lock()
		for key, s := range p.sessions {
			if time.Now().After(s.expires) {
				s.cancel()
				delete(p.sessions, key)
			}
		}
		if len(p.sessions) >= 128 {
			p.mu.Unlock()
			cancel()
			http.Error(w, "Too many active sessions", http.StatusServiceUnavailable)
			return
		}
		p.sessions[token] = session{expires: expires, ctx: ctx, cancel: cancel}
		p.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Expires: expires, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *panel) tunnel(w http.ResponseWriter, r *http.Request, s session, origin string) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", p.socket)
	if err != nil {
		http.Error(w, "Agent unavailable. Check the admin entitlement and agent service.", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	u, _ := url.Parse(origin)
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{u.Host}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	stopUpstream := context.AfterFunc(ctx, func() { _ = upstream.Close() })
	defer stopUpstream()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close(); close(done) }()
	_, _ = io.Copy(conn, upstream)
	cancel()
	_ = conn.Close()
	<-done
}
