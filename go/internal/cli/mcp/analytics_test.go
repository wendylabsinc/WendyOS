package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
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
			panicValue any
			command    string
			errorClass string
		}{
			{name: "built-in success", tool: "wendy_status", command: "wendy mcp wendy_status"},
			{name: "built-in failure", tool: "container_list", command: "wendy mcp container_list", errorClass: "not_connected"},
			{name: "handler error", tool: "device_info", err: errors.New("private-host: private failure"), command: "wendy mcp device_info", errorClass: "other"},
			{name: "built-in panic", tool: "device_info", panicValue: errors.New("private-host: private panic"), command: "wendy mcp device_info", errorClass: "other"},
			{name: "canceled", tool: "device_info", err: fmt.Errorf("private context: %w", context.Canceled), command: "wendy mcp device_info", errorClass: "context_canceled"},
			{name: "deadline", tool: "device_info", err: context.DeadlineExceeded, command: "wendy mcp device_info", errorClass: "context_deadline"},
			{name: "unstructured error", tool: "device_info", result: mcpgo.NewToolResultError("private failure"), command: "wendy mcp device_info", errorClass: "tool_error"},
			{name: "app success", tool: "private_app__private_tool", result: mcpgo.NewToolResultText("private output"), command: "wendy mcp container_tool"},
			{name: "app error", tool: "private_app__private_tool", result: errResult(errorCode("private-error-code"), "private failure"), command: "wendy mcp container_tool", errorClass: "tool_error"},
			{name: "app panic", tool: "private_app__private_tool", panicValue: errors.New("private app panic"), command: "wendy mcp container_tool", errorClass: "other"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.result != nil || tc.err != nil || tc.panicValue != nil {
					srv.AddTool(mcpgo.NewTool(tc.tool), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
						if req.GetString("private_argument", "") != "private-value" {
							t.Fatal("middleware changed tool arguments")
						}
						select {
						case got := <-events:
							t.Fatalf("event emitted before handler completed: %+v", got)
						default:
						}
						if tc.panicValue != nil {
							panic(tc.panicValue)
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
				var response mcpgo.JSONRPCMessage
				var panicValue any
				func() {
					defer func() { panicValue = recover() }()
					response = srv.HandleMessage(context.Background(), request)
				}()
				if panicValue != tc.panicValue {
					t.Fatalf("handler panic changed: got %v, want %v", panicValue, tc.panicValue)
				}
				if tc.panicValue != nil {
					if response != nil {
						t.Fatalf("middleware swallowed handler panic: %+v", response)
					}
				} else if tc.err != nil {
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
				if _, err := uuid.Parse(got.props["attempt_id"]); err != nil {
					t.Fatalf("invalid attempt id: %v", err)
				}
				want := map[string]string{
					"attempt_id": got.props["attempt_id"], "entrypoint": "mcp",
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
	seenAttempts := map[string]bool{}
	for props := range events {
		if id := props["attempt_id"]; id == "" || seenAttempts[id] {
			t.Fatal("concurrent MCP calls reused an attempt")
		} else {
			seenAttempts[id] = true
		}
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

func TestToolAnalyticsPanicPreservesStdioRecovery(t *testing.T) {
	analytics.Disable()
	events := make(chan map[string]string, 2)
	analytics.SetTrackHookForTesting(func(_ string, props map[string]string) { events <- props })
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
	srv := server.NewMCPServer("test", "0")
	srv.AddTool(mcpgo.NewTool("panic"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		panic("private panic details")
	})
	srv.AddTool(mcpgo.NewTool("success"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	registerToolAnalytics(srv)
	stdio := server.NewStdioServer(srv)
	stdio.SetErrorLogger(log.New(io.Discard, "", 0))
	var output bytes.Buffer
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"panic","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"success","arguments":{}}}`,
	}, "\n") + "\n"
	if err := stdio.Listen(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var responses []map[string]any
	decoder := json.NewDecoder(&output)
	for {
		var response map[string]any
		if err := decoder.Decode(&response); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want the panic error and subsequent success", len(responses))
	}
	var sawPanic, sawSuccess bool
	for _, response := range responses {
		if failure, ok := response["error"].(map[string]any); ok {
			sawPanic = failure["code"] == float64(mcpgo.INTERNAL_ERROR)
		} else if response["id"] == float64(2) {
			sawSuccess = response["result"] != nil
		}
	}
	if !sawPanic || !sawSuccess {
		t.Fatalf("SDK recovery changed: %+v", responses)
	}
	close(events)
	got := map[string]map[string]string{}
	for props := range events {
		got[props["command_name"]] = props
	}
	if got["wendy mcp panic"]["success"] != "false" || got["wendy mcp panic"]["error_class"] != "other" || got["wendy mcp success"]["success"] != "true" {
		t.Fatalf("missing panic or subsequent success telemetry: %+v", got)
	}
}

// allErrorCodes lists every errorCode the built-in tools return.
var allErrorCodes = []errorCode{
	errCodeNotConnected, errCodeInvalidArgument, errCodeDeviceUnreachable, errCodeEntitlementDenied,
	errCodeAuthRequired, errCodeMultipleSessions, errCodeNotFound, errCodeTimeout, errCodeCancelled,
	errCodeUnsupported, errCodeInternal, errCodeConfirmationRequired,
}

func TestToolErrorClassReportsEveryBuiltInCode(t *testing.T) {
	for _, code := range allErrorCodes {
		if got := toolErrorClass(errResult(code, "detail"), nil); got != strings.ToLower(string(code)) {
			t.Errorf("toolErrorClass(%s) = %q", code, got)
		}
	}
	if got := toolErrorClass(errResult("APP_DEFINED", "detail"), nil); got != "tool_error" {
		t.Errorf("an app-defined code must not reach telemetry: %q", got)
	}
}
