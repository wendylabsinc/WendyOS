package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func TestToolAnalyticsProtocol(t *testing.T) {
	analytics.Disable()
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
	type event struct {
		name  string
		props map[string]string
	}
	events := make(chan event, 16)
	analytics.SetTrackHookForTesting(func(name string, props map[string]string) {
		events <- event{name, props}
	})

	originalServe := serveStdio
	t.Cleanup(func() { serveStdio = originalServe })
	serveStdio = func(srv *server.MCPServer) error {
		// Discovery and rejected, unknown names must not produce command events
		// or allow untrusted request names into telemetry.
		for _, request := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"private-unknown-name","arguments":{}}}`,
		} {
			srv.HandleMessage(context.Background(), []byte(request))
			select {
			case got := <-events:
				t.Fatalf("unexpected event: %+v", got)
			default:
			}
		}

		for _, tc := range []struct {
			name       string
			tool       string
			result     *mcpgo.CallToolResult
			err        error
			command    string
			errorClass string
		}{
			{name: "built-in success", tool: "wendy_status", command: "wendy mcp wendy_status"},
			{name: "built-in failure", tool: "container_list", command: "wendy mcp container_list", errorClass: "not_connected"},
			{name: "handler error", tool: "device_info", err: errors.New("private-host: private failure"), command: "wendy mcp device_info", errorClass: "other"},
			{name: "canceled", tool: "device_info", err: fmt.Errorf("private context: %w", context.Canceled), command: "wendy mcp device_info", errorClass: "context_canceled"},
			{name: "deadline", tool: "device_info", err: context.DeadlineExceeded, command: "wendy mcp device_info", errorClass: "context_deadline"},
			{name: "unstructured error", tool: "device_info", result: mcpgo.NewToolResultError("private failure"), command: "wendy mcp device_info", errorClass: "tool_error"},
			{name: "app success", tool: "private_app__private_tool", result: mcpgo.NewToolResultText("private output"), command: "wendy mcp container_tool"},
			{name: "app error", tool: "private_app__private_tool", result: errResult(errorCode("private-error-code"), "private failure"), command: "wendy mcp container_tool", errorClass: "tool_error"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.result != nil || tc.err != nil {
					srv.AddTool(mcpgo.NewTool(tc.tool), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
						if req.GetString("private_argument", "") != "private-value" {
							t.Fatal("middleware changed tool arguments")
						}
						select {
						case got := <-events:
							t.Fatalf("event emitted before handler completed: %+v", got)
						default:
						}
						return tc.result, tc.err
					})
				}
				request, err := json.Marshal(map[string]any{
					"jsonrpc": "2.0", "id": 3, "method": "tools/call",
					"params": map[string]any{"name": tc.tool, "arguments": map[string]any{"private_argument": "private-value"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				response := srv.HandleMessage(context.Background(), request)
				if tc.err != nil {
					if _, ok := response.(mcpgo.JSONRPCError); !ok {
						t.Fatalf("handler error was lost: %+v", response)
					}
				} else {
					reply, ok := response.(mcpgo.JSONRPCResponse)
					if !ok {
						t.Fatalf("unexpected response: %+v", response)
					}
					result, ok := reply.Result.(*mcpgo.CallToolResult)
					if !ok || result.IsError != (tc.errorClass != "") {
						t.Fatalf("unexpected result: %+v", reply.Result)
					}
					if tc.result != nil && result != tc.result {
						t.Fatal("middleware replaced the result")
					}
				}

				var got event
				select {
				case got = <-events:
				default:
					t.Fatal("completed call did not emit telemetry")
				}
				if got.name != "command_executed" {
					t.Fatalf("event = %q", got.name)
				}
				duration, err := strconv.ParseInt(got.props["duration_ms"], 10, 64)
				if err != nil || duration < 0 {
					t.Fatalf("invalid duration: %q", got.props["duration_ms"])
				}
				want := map[string]string{
					"command_name": tc.command, "command_root": "mcp",
					"success": strconv.FormatBool(tc.errorClass == ""), "duration_ms": got.props["duration_ms"],
					"is_dev_build": strconv.FormatBool(version.IsDev(version.Version)),
				}
				if tc.errorClass != "" {
					want["error_class"] = tc.errorClass
				}
				// Exact fields also guard against arguments, result text, and
				// arbitrary names or error codes leaking into the payload.
				if !reflect.DeepEqual(got.props, want) {
					t.Fatalf("properties = %#v, want %#v", got.props, want)
				}
				select {
				case got := <-events:
					t.Fatalf("duplicate event: %+v", got)
				default:
				}
			})
		}
		return nil
	}
	if err := New(&config.Config{}, nil).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestToolAnalyticsConcurrentCalls(t *testing.T) {
	analytics.Disable()
	const count = 20
	events := make(chan map[string]string, count)
	analytics.SetTrackHookForTesting(func(_ string, props map[string]string) { events <- props })
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
	srv := server.NewMCPServer("test", "0")
	srv.AddTool(mcpgo.NewTool("success"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	srv.AddTool(mcpgo.NewTool("failure"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return errNotConnected(), nil
	})
	registerToolAnalytics(srv)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			name := "success"
			if i%2 != 0 {
				name = "failure"
			}
			srv.HandleMessage(context.Background(), []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, i, name)))
		})
	}
	wg.Wait()
	close(events)
	total := 0
	for props := range events {
		total++
		wantSuccess := props["command_name"] == "wendy mcp success"
		if props["success"] != strconv.FormatBool(wantSuccess) {
			t.Fatalf("concurrent calls mixed properties: %v", props)
		}
	}
	if total != count {
		t.Fatalf("got %d events for %d calls", total, count)
	}
}
