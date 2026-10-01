package mcp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func TestSimulatorSceneTarget(t *testing.T) {
	for _, raw := range []string{"https://127.0.0.1:8890", "http://localhost:8890", "http://127.0.0.2:8890", "http://192.168.1.1:8890", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user@127.0.0.1:8890", "http://127.0.0.1:8890/api/command", "http://127.0.0.1:8890/?target=robot", "http://127.0.0.1:8890/#other"} {
		if _, err := simulatorSceneTarget(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := simulatorSceneTarget("http://127.0.0.1:8890/"); err != nil {
		t.Fatal(err)
	}
}

func TestSimulatorSceneSessionObserverBoundary(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.RawQuery != "" {
			t.Error("browser credentials or query reached the simulator")
		}
		if r.URL.Path == "/api/scene/state" {
			w.Header().Set("Location", "http://127.0.0.1:1/private")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"version":1}`))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	policy := true
	handler := simulatorSceneHandler("127.0.0.1:12345", "private-token", target, client, func() bool { return policy }, func() {})
	request := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:12345"+path, nil)
		r.Header.Set("Origin", "null")
		r.Header.Set("Cookie", "must-not-leak=secret")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{"", "wrong-token"} {
		if w := request("GET", "/api/scene", token); w.Code != http.StatusUnauthorized {
			t.Fatalf("auth status: %d", w.Code)
		}
	}
	for _, path := range []string{"/api/command", "/camera.jpg", "/api/scene/lidar", "/", "/api/scene/../command"} {
		if w := request("GET", path, "private-token"); w.Code != http.StatusNotFound {
			t.Fatalf("path %s: %d", path, w.Code)
		}
	}
	if w := request("POST", "/api/scene", "private-token"); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("write accepted")
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("denied requests reached upstream")
	}
	w := request("OPTIONS", "/api/scene", "")
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Access-Control-Allow-Headers") != "Authorization" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("sandbox bearer preflight failed")
	}
	w = request("GET", "/api/scene?target=physical-robot", "private-token")
	if w.Code != http.StatusOK || w.Body.String() != `{"version":1}` {
		t.Fatalf("scene: %d %s", w.Code, w.Body)
	}
	if w := request("GET", "/api/scene/state", "private-token"); w.Code != http.StatusBadGateway || w.Header().Get("Location") != "" {
		t.Fatal("upstream redirect escaped the observer")
	}
	policy = false
	if w := request("GET", "/api/scene", "private-token"); w.Code != http.StatusForbidden {
		t.Fatal("revoked policy still served poses")
	}
	policy = true
	wrongHost := httptest.NewRequest("GET", "http://rebound.example/api/scene", nil)
	wrongHost.Header.Set("Authorization", "Bearer private-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, wrongHost)
	if w.Code != http.StatusForbidden {
		t.Fatal("rebound host accepted")
	}
}

func sceneTestGateway(t *testing.T, rawURL, profile string) (*RobotGateway, context.Context) {
	t.Helper()
	cfg := gatewayTestConfig()
	cfg.AllowSimulators = true
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("viewing must not connect or start a VM")
		return nil, nil
	}, WithGatewayLifecycle(onboarding.Backend{}, ProjectBackend{}, SimulatorBackend{Viewer: func(_ context.Context, name string) (*SimulatorViewer, error) {
		return &SimulatorViewer{Name: name, Profile: profile, URL: rawURL, Ready: true, Healthy: true}, nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	return g, context.WithValue(ctx, gatewayLocalContextKey{}, true)
}

func TestGatewaySimulatorSceneMetadataAndCleanup(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"version":1}`)) }))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "go2")
	handler := g.protocol.ListTools()["simulator_viewer"].Handler
	result, err := handler(ctx, callToolReq("simulator_viewer", map[string]any{"name": "test-scene", "embedded": true}))
	if err != nil || result.IsError || result.Meta == nil {
		t.Fatalf("session: %v %v", result, err)
	}
	session := result.Meta.AdditionalFields["scene_session"].(map[string]any)
	token := session["token"].(string)
	visible, _ := json.Marshal([]any{result.Content, result.StructuredContent})
	if bytes.Contains(visible, []byte(token)) || bytes.Contains(visible, []byte(session["url"].(string))) {
		t.Fatal("session credentials entered model-visible content")
	}
	req, _ := http.NewRequest("DELETE", session["url"].(string)+"/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatal("close request failed")
	}
	waitForSlot := func() {
		deadline := time.Now().Add(2 * time.Second)
		for len(g.webSlots) != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if len(g.webSlots) != 0 {
			t.Fatal("viewer did not release its session slot")
		}
	}
	waitForSlot()
	_, err = g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "g1", URL: upstream.URL, Ready: true, Healthy: true}, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	waitForSlot()
}

func TestGatewaySimulatorSceneResourceBridge(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || (r.URL.Path != "/api/scene" && r.URL.Path != "/api/scene/state") {
			t.Error("observer requested a control endpoint")
		}
		_, _ = w.Write([]byte(`{"version":1}`))
	}))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "go2")
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "go2", URL: upstream.URL, Ready: true, Healthy: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		scene, _ := g.authorizedSimulatorScene(ctx, session["token"].(string))
		if scene != nil {
			scene.cancel()
		}
	}()
	uri := session["resource_uri"].(string)
	read := func(ctx context.Context, uri string) ([]mcpgo.ResourceContents, error) {
		var req mcpgo.ReadResourceRequest
		req.Params.URI = uri
		return g.readSimulatorSceneResource(ctx, req)
	}
	for _, part := range []string{"geometry", "state"} {
		contents, err := read(ctx, uri+"/"+part)
		if err != nil || len(contents) != 1 {
			t.Fatalf("resource: %v %v", contents, err)
		}
		text := contents[0].(mcpgo.TextResourceContents)
		if text.URI != uri+"/"+part || text.Text != `{"version":1}` {
			t.Fatal("wrong scene resource")
		}
	}
	for _, invalid := range []string{uri + "/api/command", uri + "/state?target=other", simulatorSceneResourcePrefix + "unknown/state"} {
		if _, err := read(ctx, invalid); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	remote := context.WithValue(ctx, gatewayLocalContextKey{}, false)
	if _, err := read(remote, uri+"/state"); err == nil {
		t.Fatal("remote principal read a laptop scene")
	}
	other := context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{"bob", robotGatewayScopes})
	if _, err := read(other, uri+"/state"); err == nil {
		t.Fatal("another principal read a scene")
	}
	if calls.Load() != 2 {
		t.Fatal("rejected requests reached upstream")
	}
	closed, err := g.protocol.ListTools()["simulator_scene_close"].Handler(ctx, callToolReq("simulator_scene_close", map[string]any{"session_id": session["token"]}))
	if err != nil || closed.IsError {
		t.Fatal("close failed", err)
	}
	if _, err := read(ctx, uri+"/state"); err == nil {
		t.Fatal("closed scene stayed readable")
	}
	deadline := time.Now().Add(time.Second)
	for len(g.webSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(g.webSlots) != 0 {
		t.Fatal("closed resource scene retained a slot")
	}
}

func TestGatewaySimulatorSceneCompressedGeometry(t *testing.T) {
	body := []byte(`{"version":1,"mesh":"` + strings.Repeat("private geometry ", 100000) + `"}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "g1")
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "g1", URL: upstream.URL, Ready: true, Healthy: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	scene, _ := g.authorizedSimulatorScene(ctx, session["token"].(string))
	defer scene.cancel()
	result, err := g.protocol.ListTools()["simulator_scene_read"].Handler(ctx, callToolReq("simulator_scene_read", map[string]any{"session_id": session["token"], "part": "geometry", "encoding": "gzip"}))
	if err != nil || result.IsError {
		t.Fatal("large scene failed", err)
	}
	encoded, ok := result.Meta.AdditionalFields["scene_data_gzip"].(string)
	if !ok || len(encoded) >= len(body) || result.Meta.AdditionalFields["scene_data"] != nil {
		t.Fatal("large scene was not compacted")
	}
	packed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	restored, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(body, restored) {
		t.Fatal("scene changed during compression", err)
	}
	visible, _ := json.Marshal([]any{result.Content, result.StructuredContent})
	if bytes.Contains(visible, []byte("private")) || len(result.Content) != 0 {
		t.Fatal("compressed geometry entered model context")
	}
	legacy, err := g.protocol.ListTools()["simulator_scene_read"].Handler(ctx, callToolReq("simulator_scene_read", map[string]any{"session_id": session["token"], "part": "geometry"}))
	if err != nil || legacy.IsError || !bytes.Equal(legacy.Meta.AdditionalFields["scene_data"].(json.RawMessage), body) {
		t.Fatal("older widgets must retain the JSON scene contract", err)
	}
}

func TestGatewaySimulatorSceneToolBridge(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || (r.URL.Path != "/api/scene" && r.URL.Path != "/api/scene/state") {
			t.Error("observer requested a control endpoint")
		}
		_, _ = w.Write([]byte(`{"version":1,"private_scene":"observer"}`))
	}))
	defer upstream.Close()
	g, ctx := sceneTestGateway(t, upstream.URL, "go2")
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "go2", URL: upstream.URL, Ready: true, Healthy: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	id := session["token"].(string)
	scene, _ := g.authorizedSimulatorScene(ctx, id)
	defer scene.cancel()
	tool := g.protocol.ListTools()["simulator_scene_read"]
	visibility := tool.Tool.Meta.AdditionalFields["ui"].(map[string]any)["visibility"].([]string)
	if len(visibility) != 1 || visibility[0] != "app" || tool.Tool.Meta.AdditionalFields["openai/widgetAccessible"] != true {
		t.Fatal("scene tool must be widget accessible and app only")
	}
	read := func(ctx context.Context, id, part string) *mcpgo.CallToolResult {
		t.Helper()
		result, err := tool.Handler(ctx, callToolReq("simulator_scene_read", map[string]any{"session_id": id, "part": part}))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, part := range []string{"geometry", "state"} {
		result := read(ctx, id, part)
		if result.IsError || result.Meta == nil || string(result.Meta.AdditionalFields["scene_data"].(json.RawMessage)) != `{"version":1,"private_scene":"observer"}` {
			t.Fatalf("scene tool: %v", result)
		}
		visible, _ := json.Marshal([]any{result.Content, result.StructuredContent})
		if bytes.Contains(visible, []byte("private_scene")) || len(result.Content) != 0 || result.StructuredContent != nil {
			t.Fatal("scene data entered model-visible content")
		}
	}
	for _, part := range []string{"", "command", "state?target=other", "../api/command"} {
		if !read(ctx, id, part).IsError {
			t.Fatalf("accepted %q", part)
		}
	}
	if !read(ctx, "unknown", "state").IsError ||
		!read(context.WithValue(ctx, gatewayLocalContextKey{}, false), id, "state").IsError ||
		!read(context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{"bob", robotGatewayScopes}), id, "state").IsError ||
		!read(context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{"alice", []string{RobotReadScope}}), id, "state").IsError {
		t.Fatal("unauthorized scene tool read accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("denied reads reached upstream")
	}
	scene.cancel()
	if !read(ctx, id, "state").IsError {
		t.Fatal("ended session stayed readable")
	}
}

func TestGatewaySimulatorSceneToolLimitsAndCancellation(t *testing.T) {
	for _, mode := range []string{"oversized", "invalid", "redirect", "cancel", "close", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "oversized":
					_, _ = io.WriteString(w, `{"data":"`+strings.Repeat("x", 2<<20)+`"}`)
				case "invalid":
					_, _ = io.WriteString(w, "not JSON")
				case "redirect":
					http.Redirect(w, r, "/api/command", http.StatusFound)
				default:
					close(started)
					<-r.Context().Done()
					close(canceled)
				}
			}))
			defer upstream.Close()
			g, ctx := sceneTestGateway(t, upstream.URL, "go2")
			lifetime := time.Minute
			if mode == "expiry" {
				lifetime = 100 * time.Millisecond
			}
			session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: "go2", URL: upstream.URL, Ready: true, Healthy: true}, lifetime)
			if err != nil {
				t.Fatal(err)
			}
			id := session["token"].(string)
			scene, _ := g.authorizedSimulatorScene(ctx, id)
			defer scene.cancel()
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if mode == "cancel" || mode == "close" {
				go func() {
					select {
					case <-started:
						if mode == "cancel" {
							cancel()
						} else {
							scene.cancel()
						}
					case <-ctx.Done():
					}
				}()
			}
			result, err := g.protocol.ListTools()["simulator_scene_read"].Handler(ctx, callToolReq("simulator_scene_read", map[string]any{"session_id": id, "part": "state"}))
			if err != nil || !result.IsError || result.Meta != nil {
				t.Fatalf("invalid scene returned data: %v %v", result, err)
			}
			if mode == "cancel" || mode == "close" || mode == "expiry" {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("upstream request was not canceled")
				}
			}
		})
	}
}

// Opt-in browser integration reads an already running local simulator. It never
// starts, resets, arms or moves it. The browser receives the real gateway session.
func TestGatewaySimulatorSceneBrowser(t *testing.T) {
	raw := os.Getenv("WENDY_TEST_SIMULATOR_URL")
	if raw == "" {
		t.Skip("set WENDY_TEST_SIMULATOR_URL for local browser integration")
	}
	if _, err := simulatorSceneTarget(raw); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Get(raw + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var health struct {
		Ready, Healthy bool
		Robot          string `json:"robot"`
		RobotKind      string `json:"robot_kind"`
	}
	if err := json.NewDecoder(res.Body).Decode(&health); err != nil || !health.Ready || !health.Healthy {
		t.Fatalf("simulator health: %v", err)
	}
	if health.Robot == "" {
		health.Robot = health.RobotKind
	}
	g, ctx := sceneTestGateway(t, raw, health.Robot)
	session, err := g.openSimulatorScene(ctx, &SimulatorViewer{Profile: health.Robot, URL: raw, Ready: true, Healthy: true}, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		req, _ := http.NewRequest("DELETE", session["url"].(string)+"/session", nil)
		req.Header.Set("Authorization", "Bearer "+session["token"].(string))
		if response, err := client.Do(req); err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	data, _ := json.Marshal(session)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil || (input.Name != "simulator_scene_read" && input.Name != "simulator_scene_close" && input.Name != "simulator_scene_pause") {
			http.Error(w, "unsupported observer call", http.StatusBadRequest)
			return
		}
		// Retain the authorized principal, but cancel a tool when its host closes
		// the request rather than extending it to the scene session lifetime.
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(r.Context(), cancel)
		defer stop()
		result, err := g.protocol.ListTools()[input.Name].Handler(callCtx, callToolReq(input.Name, input.Arguments))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer bridge.Close()
	script, _ := filepath.Abs("../../../../web-client/mcp-app/scripts/verify-simulator-view.mjs")
	cmd := exec.Command("node", script)
	cmd.Env = append(os.Environ(), "WENDY_SCENE_TEST_SESSION="+string(data), "WENDY_SCENE_TEST_PROFILE="+health.Robot, "WENDY_SCENE_TEST_BRIDGE="+bridge.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
