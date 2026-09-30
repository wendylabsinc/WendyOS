package mcp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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
