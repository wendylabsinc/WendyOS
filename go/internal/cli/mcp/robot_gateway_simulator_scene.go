package mcp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const simulatorSceneResourcePrefix = "wendy://simulator-scenes/"

type gatewaySimulatorScene struct {
	ctx     context.Context
	url     string
	token   string
	subject string
	cancel  context.CancelFunc
	client  *http.Client
	created time.Time
	history simulatorPoseHistory
}

func (g *RobotGateway) registerSimulatorSceneResources() {
	g.protocol.AddResourceTemplate(mcpgo.NewResourceTemplate(simulatorSceneResourcePrefix+"{session}/{part}", "Simulator observer scene",
		mcpgo.WithTemplateMIMEType("application/json"),
		mcpgo.WithTemplateDescription("Temporary, authorized scene geometry or poses for the simulator viewer.")), g.readSimulatorSceneResource)
	read := gatewayTool("simulator_scene_read", "Read temporary simulator observer geometry or poses. Does not start, reset or move the simulator.", readOnly(),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.MinLength(36), mcpgo.MaxLength(36)),
		mcpgo.WithString("part", mcpgo.Required(), mcpgo.Enum("geometry", "state")),
		mcpgo.WithBoolean("history", mcpgo.Description("Return recently captured poses in order instead of a single snapshot.")),
		mcpgo.WithInteger("after_sequence", mcpgo.Min(0), mcpgo.Max(9007199254740991)),
		mcpgo.WithString("encoding", mcpgo.Enum("json", "gzip"), mcpgo.Description("Accept gzip-compressed large geometry in UI-only metadata. Defaults to JSON for older widgets.")))
	read.Meta.AdditionalFields["ui"] = map[string]any{"visibility": []string{"app"}}
	g.protocol.AddTool(read, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var body []byte
		var err error
		if req.GetBool("history", false) {
			if req.GetString("part", "") != "state" {
				return mcpgo.NewToolResultError("Pose history requires the state part."), nil
			}
			after, e := snapshotInteger(req, "after_sequence", 0, 0, 9007199254740991)
			if e != nil {
				return mcpgo.NewToolResultError(e.Error()), nil
			}
			body, err = g.readSimulatorPoseHistory(ctx, req.GetString("session_id", ""), uint64(after))
		} else {
			body, err = g.readSimulatorScene(ctx, req.GetString("session_id", ""), req.GetString("part", ""))
		}
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		metadata := map[string]any{"scene_data": json.RawMessage(body)}
		// Detailed G1 geometry is over 11 MB of JSON. Compress large immutable
		// geometry before passing it through the host tool bridge. Pose updates
		// remain small JSON objects, and decompressed response limits still apply.
		if req.GetString("part", "") == "geometry" && req.GetString("encoding", "json") == "gzip" && len(body) > 1<<20 {
			if err := ctx.Err(); err != nil {
				return mcpgo.NewToolResultError("Simulator geometry loading canceled."), nil
			}
			var packed bytes.Buffer
			writer := gzip.NewWriter(&packed)
			_, _ = writer.Write(body)
			_ = writer.Close()
			encoded := base64.StdEncoding.EncodeToString(packed.Bytes())
			if len(encoded) < len(body) {
				metadata = map[string]any{"scene_data_gzip": encoded}
			}
			if err := ctx.Err(); err != nil {
				return mcpgo.NewToolResultError("Simulator geometry loading canceled."), nil
			}
		}
		return &mcpgo.CallToolResult{Content: []mcpgo.Content{}, Meta: mcpgo.NewMetaFromMap(metadata)}, nil
	})
	tool := gatewayTool("simulator_scene_close", "Release a temporary observer view. Does not stop or change the simulator.", readOnly(),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.MaxLength(36)))
	tool.Meta.AdditionalFields["ui"] = map[string]any{"visibility": []string{"app"}}
	g.protocol.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		scene, err := g.authorizedSimulatorScene(ctx, req.GetString("session_id", ""))
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		scene.cancel()
		return okResult(map[string]any{"closed": true}), nil
	})
	pause := gatewayTool("simulator_scene_pause", "Suspend observer pose capture while the view is hidden. Does not pause or change simulator physics.", readOnly(), mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.MaxLength(36)))
	pause.Meta.AdditionalFields["ui"] = map[string]any{"visibility": []string{"app"}}
	g.protocol.AddTool(pause, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		scene, err := g.authorizedSimulatorScene(ctx, req.GetString("session_id", ""))
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		scene.history.pause()
		return okResult(map[string]any{"paused": true}), nil
	})
}

func (g *RobotGateway) authorizedSimulatorScene(ctx context.Context, id string) (*gatewaySimulatorScene, error) {
	if !g.localSimulatorAllowed(ctx) {
		return nil, fmt.Errorf("simulator scene access is not authorized")
	}
	g.sceneMu.Lock()
	scene := g.scenes[id]
	g.sceneMu.Unlock()
	_, principal := g.grant(ctx)
	if scene == nil || scene.ctx.Err() != nil || scene.subject != principal.Subject {
		return nil, fmt.Errorf("the viewer session ended. Choose Reconnect view")
	}
	return scene, nil
}

func (g *RobotGateway) readSimulatorSceneResource(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	ref, ok := strings.CutPrefix(req.Params.URI, simulatorSceneResourcePrefix)
	id, part, split := strings.Cut(ref, "/")
	if !ok || !split || (part != "geometry" && part != "state") {
		return nil, fmt.Errorf("unsupported scene resource")
	}
	body, err := g.readSimulatorScene(ctx, id, part)
	if err != nil {
		return nil, err
	}
	return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)}}, nil
}

func (g *RobotGateway) readSimulatorScene(ctx context.Context, id, part string) ([]byte, error) {
	if part != "geometry" && part != "state" {
		return nil, fmt.Errorf("unsupported scene part")
	}
	scene, err := g.authorizedSimulatorScene(ctx, id)
	if err != nil {
		return nil, err
	}
	path, limit := "/api/scene", int64(32<<20)
	if part == "state" {
		path, limit = "/api/scene/state", 2<<20
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stop := context.AfterFunc(scene.ctx, cancel)
	defer stop()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, scene.url+path, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+scene.token)
	res, err := scene.client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("simulator scene %s connection interrupted", part)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return nil, fmt.Errorf("simulator scene %s is unavailable (HTTP %d): %s", part, res.StatusCode, strings.TrimSpace(string(detail)))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("simulator scene %s response interrupted", part)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("simulator scene %s exceeds the %d MiB response limit", part, limit>>20)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid simulator scene %s JSON response", part)
	}
	return body, nil
}

// Scene sessions expose only observer data from the backend's verified local
// simulator URL. The browser never receives a general proxy or a control API.
func simulatorSceneTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid simulator address")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("embedded scenes require a verified loopback simulator address")
	}
	return u, nil
}

func (g *RobotGateway) openSimulatorScene(ctx context.Context, viewer *SimulatorViewer, lifetime time.Duration) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !g.localSimulatorAllowed(ctx) || !viewer.Ready || !viewer.Healthy || (viewer.Profile != "go2" && viewer.Profile != "g1" && viewer.Profile != "rosmaster-r2") {
		return nil, fmt.Errorf("a healthy local robot simulation is required")
	}
	target, err := simulatorSceneTarget(viewer.URL)
	if err != nil {
		return nil, err
	}
	select {
	case g.webSlots <- struct{}{}:
	default:
		return nil, fmt.Errorf("eight local views are already open; close an existing viewer first")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		<-g.webSlots
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = ln.Close()
		<-g.webSlots
		return nil, err
	}
	// Preserve the principal for policy checks, while allowing the live view to
	// outlive the finite MCP request. The session expires or closes explicitly.
	viewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifetime)
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	token := uuid.NewString()
	host := ln.Addr().String()
	observerClient := client
	if viewer.Profile == "rosmaster-r2" {
		observerClient = &http.Client{Transport: &rosmasterSceneTransport{base: transport}, CheckRedirect: client.CheckRedirect}
	}
	handler := simulatorSceneHandler(host, token, target, observerClient, func() bool { return g.localSimulatorAllowed(viewCtx) }, cancel)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: 20 * time.Second, BaseContext: func(net.Listener) context.Context { return viewCtx }}
	_, principal := g.grant(ctx)
	g.sceneMu.Lock()
	if g.scenes == nil {
		g.scenes = make(map[string]*gatewaySimulatorScene)
	}
	g.scenes[token] = &gatewaySimulatorScene{ctx: viewCtx, url: "http://" + host, token: token, subject: principal.Subject, cancel: cancel, client: client, created: time.Now()}
	g.sceneMu.Unlock()
	go func() {
		defer func() {
			cancel()
			g.sceneMu.Lock()
			delete(g.scenes, token)
			g.sceneMu.Unlock()
			transport.CloseIdleConnections()
			<-g.webSlots
		}()
		go func() { <-viewCtx.Done(); _ = server.Close() }()
		_ = server.Serve(ln)
	}()
	expires, _ := viewCtx.Deadline()
	return map[string]any{"url": "http://" + host, "token": token, "resource_uri": simulatorSceneResourcePrefix + token, "expires_at": expires.UTC().Format(time.RFC3339Nano)}, nil
}

func simulatorSceneHandler(host, token string, target *url.URL, client *http.Client, allowed func() bool, closeSession context.CancelFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Host != host || !allowed() {
			http.Error(w, "Scene session unavailable", http.StatusForbidden)
			return
		}
		// A sandbox may have an opaque origin. Access relies on the unguessable
		// bearer token delivered in UI-only MCP metadata, never browser cookies.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization")
			w.Header().Set("Access-Control-Max-Age", "300")
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "Scene session authorization required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/session" {
			w.WriteHeader(http.StatusNoContent)
			// Let the close response complete before closing server connections.
			defer time.AfterFunc(10*time.Millisecond, closeSession)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Observer sessions are read-only", http.StatusMethodNotAllowed)
			return
		}
		limit := int64(2 << 20)
		switch r.URL.Path {
		case "/api/scene":
			limit = 32 << 20
		case "/api/scene/state":
		default:
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		upstream := *target
		upstream.Path = r.URL.Path
		upstream.RawQuery = ""
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.String(), nil)
		if err != nil {
			http.Error(w, "Scene unavailable", http.StatusBadGateway)
			return
		}
		res, err := client.Do(req)
		if err != nil {
			http.Error(w, "Simulator connection interrupted", http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			http.Error(w, "Simulator scene unavailable", http.StatusBadGateway)
			return
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
		if err != nil || int64(len(body)) > limit {
			http.Error(w, "Simulator scene exceeds the viewer limit", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(body)
	})
}
