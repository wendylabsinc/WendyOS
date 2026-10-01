// wendy-web-relay forwards binary WebSocket bytes to one fixed agent endpoint.
// Agent mTLS is end-to-end; this process never receives operator private keys.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8788", "loopback IP:port; remote access is unsupported")
	target := flag.String("target", "", "fixed agent mTLS host:port")
	origin := flag.String("origin", "http://localhost:5173", "exact allowed browser origin")
	cloud := flag.Bool("cloud", false, "enable the fixed api.dev.wendy.sh TLS relay at /cloud")
	flag.Parse()
	if !loopbackAddress(*listen) {
		log.Fatal("-listen must use a loopback IP address; this relay must not be exposed remotely")
	}
	if _, _, err := net.SplitHostPort(*target); err != nil && !(*cloud && *target == "") {
		log.Fatal("-target must be an agent host:port")
	}
	u, err := url.Parse(*origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		log.Fatal("-origin must be a browser HTTP(S) origin")
	}
	mux := http.NewServeMux()

	if *target != "" {
		mux.HandleFunc("GET /tunnel", relayHandler(*target, false, *origin))
	}
	if *cloud {
		mux.HandleFunc("GET /cloud", relayHandler("api.dev.wendy.sh:443", true, *origin))
		mux.HandleFunc("GET /broker", func(w http.ResponseWriter, r *http.Request) {
			endpoint, err := brokerEndpoint(r.URL.Query().Get("endpoint"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}

			relayHandler(endpoint, true, *origin)(w, r)
		})
	}
	log.Printf("Relay listening at %s; allowed origin %s", *listen, *origin)
	log.Fatal((&http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}

func brokerEndpoint(endpoint string) (string, error) {
	return cloudrelay.BrowserBrokerTarget(endpoint)
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && net.ParseIP(host).IsLoopback()
}

// Origin is a browser CSRF check, not authentication. This development relay
// only accepts local connections and must not be published through a proxy.
func localRequest(r *http.Request) bool {
	if !loopbackAddress(r.RemoteAddr) || !loopbackAddress(r.Host) {
		return false
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
		if len(r.Header.Values(header)) != 0 {
			return false
		}
	}
	return true
}

// Connect upstream before upgrading the browser connection. Otherwise a TLS or
// network failure looks like a successful dial followed by a closed gRPC preface.
func relayHandler(destination string, cloudTLS bool, origin string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r) {
			http.Error(w, "Relay requires a direct loopback connection", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != origin {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		tcp, err := dialUpstream(ctx, destination, cloudTLS)
		if err != nil {
			log.Printf("Relay upstream %s: %v", destination, err)
			http.Error(w, "Relay upstream unavailable; check the relay terminal for details", http.StatusBadGateway)
			return
		}
		defer tcp.Close()
		u, _ := url.Parse(origin) // main validates the configured origin.
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{u.Host}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		done := make(chan struct{})
		go func() { _, _ = io.Copy(tcp, conn); tcp.Close(); close(done) }()
		_, err = io.Copy(conn, tcp)
		if err != nil && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil && websocket.CloseStatus(err) == -1 {
			log.Printf("Relay upstream %s disconnected: %v", destination, err)
		}
		cancel()
		ws.CloseNow()
		<-done
	}
}

func dialUpstream(ctx context.Context, destination string, cloudTLS bool) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !cloudTLS {
		return dialer.DialContext(ctx, "tcp", destination)
	}
	host, _, err := net.SplitHostPort(destination)
	if err != nil {
		return nil, err
	}
	conn, err := (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{
		ServerName: host, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"},
	}}).DialContext(ctx, "tcp", destination)
	if err != nil {
		return nil, err
	}
	if conn.(*tls.Conn).ConnectionState().NegotiatedProtocol != "h2" {
		conn.Close()
		return nil, fmt.Errorf("upstream did not negotiate HTTP/2")
	}
	return conn, nil
}
