package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type gatewayWebInventoryClient struct {
	agentpb.WendyContainerServiceClient
	list func(context.Context) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error)
}

func (c gatewayWebInventoryClient) ListContainers(ctx context.Context, _ *agentpb.ListContainersRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	return c.list(ctx)
}

type gatewayWebInventoryStream struct {
	grpc.ClientStream
	app *agentpb.AppContainer
}

func (s *gatewayWebInventoryStream) Recv() (*agentpb.ListContainersResponse, error) {
	if s.app == nil {
		return nil, io.EOF
	}
	app := s.app
	s.app = nil
	return &agentpb.ListContainersResponse{Container: app}, nil
}

type gatewayWebCloseFunc func() error

func (f gatewayWebCloseFunc) Close() error { return f() }

func TestGatewayWebOpenCancelsPendingDeviceConnection(t *testing.T) {
	started := make(chan struct{})
	g, err := NewRobotGateway(gatewayTestConfig(), func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(gatewayAccessContext(robotGatewayScopes, true))
	defer cancel()
	done := make(chan *mcpgo.CallToolResult, 1)
	go func() {
		result, _ := g.openRobotApp(ctx, callToolReq("open_robot_app", map[string]any{"robot_id": "alpha", "app_name": "companion"}))
		done <- result
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("device connection did not start")
	}
	cancel()
	select {
	case result := <-done:
		if result == nil || !result.IsError || len(g.webSlots) != 0 {
			t.Fatal("canceled setup did not fail and release its view slot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("device connection ignored opening cancellation")
	}
}

func TestGatewayWebOpenBoundsInventoryAndCleansUpFailure(t *testing.T) {
	var closed atomic.Bool
	var connectionCtx context.Context
	g, err := NewRobotGateway(gatewayTestConfig(), func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
		connectionCtx = ctx
		return &grpcclient.AgentConnection{
			ExtraClosers: []io.Closer{gatewayWebCloseFunc(func() error { closed.Store(true); return nil })},
			ContainerService: gatewayWebInventoryClient{list: func(ctx context.Context) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > 15*time.Second || time.Until(deadline) <= 0 {
					t.Error("app inventory has no bounded setup deadline")
				}
				return nil, fmt.Errorf("fixture inventory unavailable")
			}},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := g.openRobotApp(gatewayAccessContext(robotGatewayScopes, true), callToolReq("open_robot_app", map[string]any{"robot_id": "alpha", "app_name": "companion"}))
	if err != nil || !result.IsError || !closed.Load() || connectionCtx.Err() == nil || len(g.webSlots) != 0 {
		t.Fatalf("failed inventory leaked view resources: %v %v", result, err)
	}
}

func TestGatewayWebOpenRetainsSuccessfulViewAfterToolCompletion(t *testing.T) {
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade with HTTP keep-alives disabled: %v", err)
				return
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			kind, message, err := conn.ReadMessage()
			if err == nil {
				_ = conn.WriteMessage(kind, message)
			}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>Working app</html>")
	}))
	defer upstream.Close()
	closed := make(chan struct{})
	var connectionCtx context.Context
	g, err := NewRobotGateway(gatewayTestConfig(), func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
		connectionCtx = ctx
		return &grpcclient.AgentConnection{
			ExtraClosers: []io.Closer{gatewayWebCloseFunc(func() error { close(closed); return nil })},
			ContainerService: gatewayWebInventoryClient{list: func(context.Context) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
				return &gatewayWebInventoryStream{app: &agentpb.AppContainer{AppName: "companion", RunningState: agentpb.AppRunningState_RUNNING, HttpPort: 8080}}, nil
			}},
			RegistryDialer: func(dialCtx context.Context, port int) (net.Conn, error) {
				if port != 8080 {
					return nil, fmt.Errorf("wrong app port %d", port)
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return (&net.Dialer{}).DialContext(dialCtx, "tcp", upstream.Listener.Addr().String())
			},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, finishRequest := context.WithCancel(gatewayAccessContext(robotGatewayScopes, true))
	defer finishRequest()
	// Shorten only the view lease so the real server expires during this test.
	result, err := g.openRobotAppView(ctx, callToolReq("open_robot_app", map[string]any{"robot_id": "alpha", "app_name": "companion"}), 2*time.Second)
	if err != nil || result.IsError {
		t.Fatalf("open app: %v %v", result, err)
	}
	finishRequest()
	if connectionCtx.Err() != nil {
		t.Fatal("tool completion canceled the retained Cloud connection")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: time.Second}
	response, err := client.Get(result.StructuredContent.(map[string]any)["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Working app") {
		t.Fatalf("retained view failed: status=%d, error=%v", response.StatusCode, err)
	}
	viewURL, _ := url.Parse(result.StructuredContent.(map[string]any)["url"].(string))
	headers := http.Header{"Origin": []string{"http://" + viewURL.Host}}
	for _, cookie := range jar.Cookies(viewURL) {
		headers.Add("Cookie", cookie.String())
	}
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	websocketConn, _, err := dialer.Dial("ws://"+viewURL.Host+"/socket", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer websocketConn.Close()
	_ = websocketConn.SetReadDeadline(time.Now().Add(time.Second))
	if err := websocketConn.WriteMessage(websocket.TextMessage, []byte("live update")); err != nil {
		t.Fatal(err)
	}
	_, message, err := websocketConn.ReadMessage()
	if err != nil || string(message) != "live update" {
		t.Fatalf("retained view WebSocket failed: %q %v", message, err)
	}
	_ = websocketConn.Close()
	select {
	case <-closed:
		if connectionCtx.Err() == nil || len(g.webSlots) != 0 {
			t.Fatal("expired view did not release its connection and slot")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("view did not expire")
	}
}

// Match Python HTTPServer: service one HTTP/1.1 connection until it closes
// before accepting another. Idle keep-alive connections block other clients.
func gatewaySingleThreadedHTTPFixture(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
			reader := bufio.NewReader(conn)
			for {
				req, err := http.ReadRequest(reader)
				if err != nil {
					break
				}
				_ = req.Body.Close()
				connectionHeader := ""
				if req.Close {
					connectionHeader = "Connection: close\r\n"
				}
				_, err = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: 8\r\n%s\r\napp okay", connectionHeader)
				if err != nil || req.Close {
					break
				}
			}
			stopCancel()
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

func TestGatewayWebViewsDoNotMonopolizeSingleThreadedApp(t *testing.T) {
	upstream := gatewaySingleThreadedHTTPFixture(t)
	closed := make(chan struct{}, 2)
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{
			ExtraClosers: []io.Closer{gatewayWebCloseFunc(func() error { closed <- struct{}{}; return nil })},
			ContainerService: gatewayWebInventoryClient{list: func(context.Context) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
				return &gatewayWebInventoryStream{app: &agentpb.AppContainer{AppName: "companion", RunningState: agentpb.AppRunningState_RUNNING, HttpPort: 8080}}, nil
			}},
			RegistryDialer: func(ctx context.Context, _ int) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", upstream)
			},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(gatewayAccessContext(robotGatewayScopes, true), time.Second)
		result, err := g.openRobotAppView(ctx, callToolReq("open_robot_app", map[string]any{"robot_id": "alpha", "app_name": "companion"}), 3*time.Second)
		cancel()
		if err != nil || result.IsError {
			t.Fatalf("view %d was blocked by another view's idle connection: %v %v", i, result, err)
		}
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, Timeout: time.Second}
		response, err := client.Get(result.StructuredContent.(map[string]any)["url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(body) != "app okay" {
			t.Fatalf("view %d did not read its page: %q, %v", i, body, err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("view did not release its app connection")
		}
	}
}

func TestGatewayAppHomepageRejectsAPIAndMissingPages(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		status      int
		contentType string
		wantError   string
	}{
		{"web UI", 200, "text/html; charset=utf-8", ""},
		{"API only", 200, "application/json", "exposes an API"},
		{"no homepage", 404, "text/html", "no web page"},
		{"browser authentication", 401, "text/html", ""},
		{"browser sign-in page", 403, "text/html", ""},
		{"unavailable", 503, "text/plain", "HTTP 503"},
		{"redirect without following", 302, "text/html", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/" {
					t.Errorf("unexpected probe: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", scenario.contentType)
				w.Header().Set("Location", "http://must-not-follow.invalid/")
				w.WriteHeader(scenario.status)
			}))
			defer srv.Close()
			err := gatewayAppHomepage(context.Background(), http.DefaultTransport, strings.TrimPrefix(srv.URL, "http://"))
			if scenario.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.wantError) {
				t.Fatalf("error = %v, want %q", err, scenario.wantError)
			}
		})
	}
}

func TestGatewayAppHomepageAcceptsNegotiatedHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>Operator console</html>")
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{ "api": true }`)
		}
	}))
	defer srv.Close()
	if err := gatewayAppHomepage(context.Background(), http.DefaultTransport, strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatal(err)
	}
}

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
