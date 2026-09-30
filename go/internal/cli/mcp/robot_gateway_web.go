package mcp

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Only a declared app HTTP port can be opened, never an arbitrary URL or port
// from tool input. The local view is private, short-lived, and host-bound.
func (g *RobotGateway) registerAppWebTool() {
	g.webSlots = make(chan struct{}, 8)
	g.protocol.AddTool(gatewayTool("open_robot_app", "Open an installed app's declared web UI on the gateway laptop. Requires a running app and app-tool access. Creates a private loopback view for 30 minutes; does not start the app. Open the returned URL in the user's browser.", mutating(), robotArgument(), mcpgo.WithString("app_name", mcpgo.Required(), mcpgo.MaxLength(256))), g.openRobotApp)
}

func (g *RobotGateway) openRobotApp(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if ctx.Value(gatewayLocalContextKey{}) != true || !g.hasScope(ctx, RobotToolsScope) {
		return mcpgo.NewToolResultError("App web views require a local gateway with app-tool access."), nil
	}
	r, err := g.authorize(ctx, req.GetString("robot_id", ""), RobotReadScope)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	select {
	case g.webSlots <- struct{}{}:
	default:
		return mcpgo.NewToolResultError("Eight app views are already open. Wait for an existing view to expire."), nil
	}
	retained := false
	defer func() {
		if !retained {
			<-g.webSlots
		}
	}()
	viewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	defer func() {
		if !retained {
			cancel()
		}
	}()
	connectCtx, connectCancel := context.WithCancel(viewCtx)
	connectTimer := time.AfterFunc(20*time.Second, connectCancel)
	conn, err := g.connect(connectCtx, r.Device)
	connectTimer.Stop()
	// The connection's cloud dialer may use its establishment context, so keep
	// it alive with the view and bound individual HTTP dials below.
	if err != nil {
		connectCancel()
		return mcpgo.NewToolResultError("Could not connect to this app's device."), nil
	}
	defer func() {
		if !retained {
			connectCancel()
			conn.Close()
		}
	}()
	s := New(&config.Config{}, nil)
	s.SetConn(conn)
	apps, err := gatewayApps(ctx, r, s)
	if err != nil {
		return mcpgo.NewToolResultError("Could not inspect this app."), nil
	}
	appName := req.GetString("app_name", "")
	port := uint32(0)
	for _, app := range apps {
		if app["name"] == appName && app["state"] == "RUNNING" {
			port, _ = app["http_port"].(uint32)
		}
	}
	if port == 0 || port > 65535 {
		return mcpgo.NewToolResultError("This running app does not advertise a web UI. Declare its HTTP entitlement and redeploy it."), nil
	}
	var target string
	if conn.RegistryDialer == nil {
		if conn.SimulatorName != "" {
			target, err = appInspectSimulatorAddress(ctx, conn, int(port))
		} else {
			var host string
			host, _, err = net.SplitHostPort(conn.Addr)
			target = net.JoinHostPort(host, strconv.Itoa(int(port)))
		}
		if err != nil {
			return mcpgo.NewToolResultError("This device has no verified route to its app web UI."), nil
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if conn.RegistryDialer != nil {
			return gatewayAppWebDial(viewCtx, ctx, func(dialCtx context.Context) (net.Conn, error) {
				return conn.RegistryDialer(dialCtx, int(port))
			})
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", target)
	}, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token := uuid.NewString()
	host := ln.Addr().String()
	upstream := &url.URL{Scheme: "http", Host: "wendy-app.invalid"}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "The device app is unavailable. Check its state in Wendy.", http.StatusBadGateway)
	}
	server := &http.Server{Handler: gatewayAppWebHandler(host, token, proxy), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	retained = true
	go func() {
		defer func() { <-g.webSlots; cancel(); connectCancel(); conn.Close(); transport.CloseIdleConnections() }()
		go func() { <-viewCtx.Done(); _ = server.Close() }()
		_ = server.Serve(ln)
	}()
	return okResult(map[string]any{"app_name": appName, "robot_id": r.ID, "url": "http://" + host + "/?wendy_view=" + token, "expires_in_seconds": 1800, "scope": "gateway_laptop"}), nil
}

// A cloud dial returns a gRPC-backed connection that keeps using its context.
// Limit establishment, then retain that context until the connection or view closes.
func gatewayAppWebDial(viewCtx, requestCtx context.Context, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	ctx, cancel := context.WithCancel(viewCtx)
	timer := time.AfterFunc(10*time.Second, cancel)
	stopRequest := context.AfterFunc(requestCtx, cancel)
	conn, err := dial(ctx)
	timedOut := !timer.Stop()
	requestCanceled := !stopRequest()
	if timedOut || requestCanceled {
		cancel()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		return nil, err
	}
	return &gatewayAppWebConn{Conn: conn, cancel: cancel}, nil
}

type gatewayAppWebConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *gatewayAppWebConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func gatewayAppWebHandler(host, token string, next http.Handler) http.Handler {
	cookieName := "wendy_view_" + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "Invalid host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+host {
			http.Error(w, "Invalid origin", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet && subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("wendy_view")), []byte(token)) == 1 {
			// ChatGPT opens this URL from another site. Lax permits the initial
			// top-level GET redirect; strict cookies would require a manual reload.
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 1800})
			q := r.URL.Query()
			q.Del("wendy_view")
			r.URL.RawQuery = q.Encode()
			w.Header().Set("Referrer-Policy", "no-referrer")
			http.Redirect(w, r, r.URL.RequestURI(), http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
			http.Error(w, "Open this app from Wendy to continue.", http.StatusUnauthorized)
			return
		}
		// Cookies are scoped to a host, not its port. Other open app views on
		// 127.0.0.1 also send their access cookies here; none belong upstream.
		cookies := r.Cookies()
		r.Header.Del("Cookie")
		for _, c := range cookies {
			if !strings.HasPrefix(c.Name, "wendy_view_") {
				r.AddCookie(c)
			}
		}
		r.Header.Del("Forwarded")
		r.Header.Del("X-Forwarded-Host")
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Forwarded-Proto")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
