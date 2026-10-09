package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestMain allows NewTools to launch this test binary exactly as it launches
// Wendy. The helper provides a protocol server without any devices or network.
func TestMain(m *testing.M) {
	if os.Getenv("WENDY_CHAT_TEST_SETUP") == "1" {
		os.Exit(runSetupTerminalFixture())
	}
	if os.Getenv("WENDY_CHAT_TEST_MCP") == "1" && len(os.Args) >= 3 && os.Args[1] == "mcp" && os.Args[2] == "serve" {
		srv := server.NewMCPServer("chat-test", "1", server.WithToolCapabilities(true))
		srv.AddTool(mcpgo.NewTool("test_status", mcpgo.WithReadOnlyHintAnnotation(true)), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			srv.AddTool(mcpgo.NewTool("new_hardware", mcpgo.WithReadOnlyHintAnnotation(true)), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return mcpgo.NewToolResultText("sensor ready"), nil
			})
			return mcpgo.NewToolResultText(cwd), nil
		})
		srv.AddTool(mcpgo.NewTool("test_args", mcpgo.WithReadOnlyHintAnnotation(true)), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			encoded, err := json.Marshal(os.Args[1:])
			if err != nil {
				return nil, err
			}
			return mcpgo.NewToolResultText(string(encoded)), nil
		})
		srv.AddTool(mcpgo.NewTool("test_write"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultError("test mutation rejected"), nil
		})
		srv.AddTool(mcpgo.NewTool("test_wait"), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if err := os.WriteFile("mcp-started", []byte("started"), 0600); err != nil {
				return nil, err
			}
			<-ctx.Done()
			if err := os.WriteFile("mcp-canceled", []byte(ctx.Err().Error()), 0600); err != nil {
				return nil, err
			}
			return mcpgo.NewToolResultError("operation canceled"), nil
		})
		srv.AddTool(mcpgo.NewTool("test_watch_notify", mcpgo.WithReadOnlyHintAnnotation(true)), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			_ = srv.SendNotificationToSpecificClient("stdio", "notifications/wendy/watch_status", map[string]any{"watch_id": "w1", "label": "front door", "state": "READY", "reason": "", "camera": "Brio 101", "watching": []string{"person"}})
			_ = srv.SendNotificationToSpecificClient("stdio", "notifications/wendy/watch_event", map[string]any{"watch_id": "w1", "label": "front door", "sequence": 1, "kind": "entered", "classes": []map[string]any{{"label": "person", "score": 0.91}}, "occurred_at": "2026-10-07T20:20:03Z"})
			_ = srv.SendNotificationToSpecificClient("stdio", "notifications/other/thing", map[string]any{"watch_id": "w1"})
			return mcpgo.NewToolResultText("sent"), nil
		})
		srv.AddTool(mcpgo.NewTool("watch_list", mcpgo.WithReadOnlyHintAnnotation(true)), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			listing := map[string]any{"free_slots": 1, "watches": []map[string]any{
				{"watch_id": "w1", "label": "front door", "camera_name": "Brio 101", "classes": []string{"person"}, "state": "READY"},
				{"watch_id": "w2", "label": "garage", "camera_name": "Brio 101", "classes": []string{"car"}, "state": "ENDED", "reason": "stopped"},
			}}
			return mcpgo.NewToolResultStructured(listing, "watches"), nil
		})
		srv.AddTool(mcpgo.NewTool("watch_stop"), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			id := req.GetString("watch_id", "")
			if err := os.WriteFile("stopped-"+id, nil, 0600); err != nil {
				return nil, err
			}
			return mcpgo.NewToolResultStructured(map[string]any{"watch_id": id, "state": "ENDED", "removed": true}, "stopped"), nil
		})
		fmt.Fprintln(os.Stderr, "fake MCP diagnostics, hidden from chat")
		if err := server.ServeStdio(srv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestMCPSubprocessInitializeListCallAndRefresh(t *testing.T) {
	t.Setenv("WENDY_CHAT_TEST_MCP", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := NewTools(ctx, executable, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	listing, err := tools.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]Tool)
	for _, tool := range listing {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"workspace_list", "workspace_read", "workspace_write", "workspace_exec", "wendy_docs", "test_status", "test_write"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("missing tool %q", name)
		}
	}
	if byName["test_status"].RequiresApproval || !byName["test_write"].RequiresApproval || !byName["workspace_exec"].RequiresApproval {
		t.Fatalf("incorrect approval annotations: %+v", byName)
	}
	output, err := tools.Execute(ctx, ToolCall{Name: "test_status", Arguments: json.RawMessage(`{}`)})
	if err != nil || strings.TrimSpace(output) != tools.workspace {
		t.Fatalf("MCP child directory/result = %q, %v; want %q", output, err, tools.workspace)
	}
	listing, err = tools.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listing {
		found = found || tool.Name == "new_hardware"
	}
	if !found {
		t.Fatal("newly registered hardware tool was not exposed")
	}
	output, err = tools.Execute(ctx, ToolCall{Name: "new_hardware", Arguments: json.RawMessage(`{}`)})
	if err != nil || !strings.Contains(output, "sensor ready") {
		t.Fatalf("dynamic MCP tool = %q, %v", output, err)
	}
	output, err = tools.Execute(ctx, ToolCall{Name: "test_write", Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(output, "mutation rejected") {
		t.Fatalf("MCP tool error = %q, %v", output, err)
	}
	if err := tools.Close(); err != nil {
		t.Fatalf("closing MCP child: %v", err)
	}
	if err := tools.Close(); err != nil {
		t.Fatalf("closing again: %v", err)
	}
}

// Chat's prompt directs the model to camera, cloud and other specialist tools,
// and chat only executes tools the server lists, so it must not inherit the
// server's core-only default.
func TestNewToolsRequestsEveryMCPToolGroup(t *testing.T) {
	t.Setenv("WENDY_CHAT_TEST_MCP", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := NewTools(ctx, executable, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	if _, err := tools.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	output, err := tools.Execute(ctx, ToolCall{Name: "test_args", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal([]byte(output), &args); err != nil {
		t.Fatalf("MCP child args %q: %v", output, err)
	}
	for i := range args {
		if args[i] == "--tool-groups" && i+1 < len(args) && args[i+1] == "all" {
			return
		}
	}
	t.Fatalf("MCP child args = %q; want --tool-groups all", args)
}

func TestNewToolsRejectsMissingExecutableAndWorkspace(t *testing.T) {
	if _, err := NewTools(context.Background(), "wendy-test-executable-does-not-exist", t.TempDir(), ""); err == nil {
		t.Fatal("accepted missing executable")
	}
	if _, err := NewTools(context.Background(), "", t.TempDir()+"/does-not-exist", ""); err == nil {
		t.Fatal("accepted missing workspace")
	}
}

func TestMCPCancellationReachesHandlerAndKeepsSessionUsable(t *testing.T) {
	t.Setenv("WENDY_CHAT_TEST_MCP", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := NewTools(ctx, executable, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	done := make(chan error, 1)
	go func() {
		_, err := tools.Execute(callCtx, ToolCall{Name: "test_wait", Arguments: json.RawMessage(`{}`)})
		done <- err
	}()
	waitForMarker := func(name string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(tools.workspace, name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("MCP handler did not create %s", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForMarker("mcp-started")
	cancelCall()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled MCP call error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MCP call did not return after cancellation")
	}
	waitForMarker("mcp-canceled")
	output, err := tools.Execute(ctx, ToolCall{Name: "test_status", Arguments: json.RawMessage(`{}`)})
	if err != nil || strings.TrimSpace(output) != tools.workspace {
		t.Fatalf("MCP session after cancellation = %q, %v", output, err)
	}
}
