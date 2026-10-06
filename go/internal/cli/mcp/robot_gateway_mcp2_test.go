package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func gatewayModernRequest(id int, method string, params map[string]any) []byte {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{gatewayProtocolVersionKey: gatewayMCP2Version, "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "fixture", "version": "1"}}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return raw
}

func gatewayModernFixture(t *testing.T) (*RobotGateway, context.Context) {
	t.Helper()
	cfg := gatewayTestConfig()
	cfg.StateDirectory = t.TempDir()
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, fmt.Errorf("unexpected device access")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	return g, context.WithValue(ctx, gatewayLocalContextKey{}, true)
}

func gatewayModernEnvelope(t *testing.T, response any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil {
		t.Fatal("invalid response")
	}
	return result
}

func TestGatewayMCP2DiscoveryToolsResourcesAndLegacy(t *testing.T) {
	g, ctx := gatewayModernFixture(t)
	for _, method := range []string{"server/discover", "tools/list", "events/list", "resources/read", "skills/list"} {
		params := map[string]any{}
		if method == "resources/read" {
			params["uri"] = robotPanelURI
		}
		response, handled := g.handleGatewayMCP2(ctx, gatewayModernRequest(1, method, params), "")
		envelope := gatewayModernEnvelope(t, response)
		if !handled || envelope["error"] != nil {
			t.Fatalf("%s: %v", method, envelope["error"])
		}
		result := envelope["result"].(map[string]any)
		if result["resultType"] != "complete" || result["ttlMs"] != float64(0) || result["cacheScope"] != "private" {
			t.Fatalf("%s result lacks cache/complete metadata", method)
		}
		if result["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"] == nil {
			t.Fatal("missing server identity")
		}
		if method == "server/discover" {
			caps := result["capabilities"].(map[string]any)
			if caps["events"] == nil || caps["tasks"] != nil {
				t.Fatal("wrong modern capabilities")
			}
		}
		if method == "tools/list" {
			found := false
			for _, row := range result["tools"].([]any) {
				tool := row.(map[string]any)
				if tool["execution"] != nil {
					t.Fatal("legacy tasks leaked into modern catalog")
				}
				if tool["name"] == "open_devices" {
					found = tool["_meta"].(map[string]any)["ui"] != nil
				}
			}
			if !found {
				t.Fatal("lost app entrypoint metadata")
			}
		}
	}
	response, handled := g.handleGatewayMCP2(ctx, gatewayModernRequest(2, "tools/call", map[string]any{"name": "list_robots", "arguments": map[string]any{}}), "")
	envelope := gatewayModernEnvelope(t, response)
	if !handled || envelope["error"] != nil || envelope["result"].(map[string]any)["structuredContent"] == nil {
		t.Fatal("modern tool did not execute", envelope)
	}
	legacy := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	if _, handled := g.handleGatewayMCP2(ctx, legacy, ""); handled {
		t.Fatal("intercepted legacy request")
	}
	legacyResult := gatewayModernEnvelope(t, g.protocol.HandleMessage(ctx, legacy))["result"].(map[string]any)
	if legacyResult["resultType"] != nil {
		t.Fatal("changed MCP1 result")
	}
}

func TestGatewayMCP2HTTPMetadataAndAccountScopes(t *testing.T) {
	g, _ := gatewayModernFixture(t)
	handler, err := g.httpHandler(http.DefaultClient, gatewayTestEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, header, body string
		status, code              int
	}{
		{"auth", "wrong", gatewayMCP2Version, string(gatewayModernRequest(1, "events/list", nil)), 401, 0},
		{"missing header", gatewayTestEnv("ALICE_TOKEN"), "", string(gatewayModernRequest(1, "tools/list", nil)), 400, -32020},
		{"mismatched header", gatewayTestEnv("ALICE_TOKEN"), "2025-11-25", string(gatewayModernRequest(1, "tools/list", nil)), 400, -32020},
		{"missing metadata", gatewayTestEnv("ALICE_TOKEN"), gatewayMCP2Version, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`, 400, -32602},
		{"unsupported", gatewayTestEnv("ALICE_TOKEN"), "future", strings.ReplaceAll(string(gatewayModernRequest(1, "tools/list", nil)), gatewayMCP2Version, "future"), 400, -32022},
		{"no events grant", gatewayTestEnv("BOB_TOKEN"), gatewayMCP2Version, string(gatewayModernRequest(1, "events/list", nil)), 200, -32001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/mcp", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+tc.token)
			request.Header.Set("Mcp-Protocol-Version", tc.header)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.code != 0 {
				var body struct {
					Error struct {
						Code int `json:"code"`
					} `json:"error"`
				}
				if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code != tc.code {
					t.Fatal(w.Body.String())
				}
			}
		})
	}
}

func TestGatewayMCP2StdioCancellationAndLegacyCoexist(t *testing.T) {
	g, ctx := gatewayModernFixture(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	g.protocol.AddTool(gatewayTool("fixture_wait", "Wait for cancellation", readOnly()), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	})
	in, send := io.Pipe()
	receive, out := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer send.Close()
	defer receive.Close()
	finished := make(chan error, 1)
	go func() { finished <- g.listenGatewayStdio(ctx, in, out) }()
	lines := make(chan []byte, 8)
	go func() {
		scanner := bufio.NewScanner(receive)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	_, _ = send.Write(append(gatewayModernRequest(7, "tools/call", map[string]any{"name": "fixture_wait", "arguments": map[string]any{}}), '\n'))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("modern call did not start")
	}
	_, _ = io.WriteString(send, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":7}}\n")
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("modern cancellation went to wrong session")
	}
	_, _ = io.WriteString(send, "{\"jsonrpc\":\"2.0\",\"id\":8,\"method\":\"ping\"}\n")
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-lines:
			var value map[string]any
			_ = json.Unmarshal(line, &value)
			if value["id"] == float64(8) {
				send.Close()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("stdio did not stop")
				}
				return
			}
		case <-deadline:
			t.Fatal("legacy request stopped working")
		}
	}
}
