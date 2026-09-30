package mcp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGatewayWebDialRetainsCloudStreamUntilConnectionCloses(t *testing.T) {
	viewCtx, closeView := context.WithCancel(context.Background())
	defer closeView()
	requestCtx, endRequest := context.WithCancel(context.Background())
	defer endRequest()
	var streamCtx context.Context
	local, remote := net.Pipe()
	defer remote.Close()
	conn, err := gatewayAppWebDial(viewCtx, requestCtx, func(ctx context.Context) (net.Conn, error) {
		streamCtx = ctx
		return local, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	endRequest()
	if streamCtx.Err() != nil {
		t.Fatal("cloud stream canceled after dial or request completion")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if streamCtx.Err() != context.Canceled {
		t.Fatal("closing connection did not cancel its cloud stream")
	}
}

func TestGatewayWebDialCancelsPendingDialWithRequest(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	_, err := gatewayAppWebDial(context.Background(), requestCtx, func(ctx context.Context) (net.Conn, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != context.Canceled {
		t.Fatalf("dial error = %v, want canceled", err)
	}
}

func TestGatewayWebViewRequiresItsHostAndAccessCookie(t *testing.T) {
	const host = "127.0.0.1:12345"
	const token = "test-private-token"
	h := gatewayAppWebHandler(host, token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("wendy_view_" + token); err == nil {
			t.Error("access cookie leaked to app")
		}
		if _, err := r.Cookie("wendy_view_another_view"); err == nil {
			t.Error("another app view's access cookie leaked to app")
		}
		if cookie, err := r.Cookie("app_session"); err != nil || cookie.Value != "preserved" {
			t.Error("app session cookie was lost")
		}
		w.WriteHeader(http.StatusOK)
	}))
	for _, tc := range []struct {
		name, host, path, origin string
		cookie                   bool
		want                     int
	}{
		{"no access", host, "/", "", false, 401},
		{"token establishes session", host, "/?wendy_view=" + token, "", false, 303},
		{"private session", host, "/", "", true, 200},
		{"DNS rebinding", "attacker.example:12345", "/", "", true, 403},
		{"foreign origin", host, "/", "https://attacker.example", true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+tc.path, nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.cookie {
				r.AddCookie(&http.Cookie{Name: "wendy_view_" + token, Value: token})
				r.AddCookie(&http.Cookie{Name: "wendy_view_another_view", Value: "another-private-token"})
				r.AddCookie(&http.Cookie{Name: "app_session", Value: "preserved"})
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
			if tc.want == 303 && w.Header().Get("Location") != "/" {
				t.Fatal("token retained in redirect")
			}
			if tc.want == 303 {
				cookies := w.Result().Cookies()
				if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
					t.Fatal("cookie must permit initial navigation from ChatGPT and remain inaccessible to scripts")
				}
			}
		})
	}
}

func TestGatewayWebViewProxiesAppAssetsAndWebSockets(t *testing.T) {
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `<html><script src="assets/app.js"></script></html>`)
		case "/assets/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(w, "window.appLoaded = true;")
		case "/socket":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upstream WebSocket upgrade: %v", err)
				return
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			kind, body, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("upstream WebSocket read: %v", err)
				return
			}
			_ = conn.WriteMessage(kind, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	view := httptest.NewUnstartedServer(nil)
	view.Config.Handler = gatewayAppWebHandler(view.Listener.Addr().String(), "private-view", httputil.NewSingleHostReverseProxy(upstreamURL))
	view.Start()
	defer view.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	for _, path := range []string{"/?wendy_view=private-view", "/assets/app.js"} {
		response, err := client.Get(view.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.StatusCode)
		}
	}
	viewURL, _ := url.Parse(view.URL)
	headers := http.Header{"Origin": []string{view.URL}}
	for _, cookie := range jar.Cookies(viewURL) {
		headers.Add("Cookie", cookie.String())
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(view.URL, "http")+"/socket", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte("live app update")); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.ReadMessage()
	if err != nil || string(body) != "live app update" {
		t.Fatalf("WebSocket reply = %q, error = %v", body, err)
	}
}
