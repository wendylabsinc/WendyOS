package tsarelay

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalHTTPSOverOpaqueTimestampTunnel(t *testing.T) {
	body := []byte("normal nonce-bearing timestamp request")
	reply := bytes.Repeat([]byte("normal signed timestamp response\n"), 1024)
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body) || r.Method != "POST" || r.URL.Path != "/time" || r.Header.Get("Content-Type") != "application/timestamp-query" {
			t.Errorf("ordinary HTTPS request differs: %s %s %q %v", r.Method, r.URL.Path, got, err)
		}
		_, _ = w.Write(reply)
	}))
	defer authority.Close()
	relay, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		incoming, err := relay.Accept()
		if err != nil {
			completed <- err
			return
		}
		completed <- tunnelConnection(ctx, incoming, authority.Listener.Addr().String())
	}()
	// The only TLS configuration comes from the real HTTPS authority. The
	// relay uses raw sockets and never sees the HTTP request or response.
	trust := authority.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport := &http.Transport{TLSClientConfig: trust, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, ignored string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, relay.Addr().String())
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, "POST", authority.URL+"/time", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/timestamp-query")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Equal(got, reply) {
		t.Fatalf("HTTPS response differs: status=%d bytes=%d err=%v", response.StatusCode, len(got), err)
	}
	if response.TLS == nil || response.TLS.Version < tls.VersionTLS12 || len(response.TLS.VerifiedChains) == 0 {
		t.Fatal("HTTPS authority was not verified end to end")
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
