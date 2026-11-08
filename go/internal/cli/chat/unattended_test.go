package chat

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// In a turn nobody is present for, only tools marked Unattended run. A tool
// that changes something is refused whether it needs approval or not, and
// whoever provides it: a device app, the Wendy MCP server or chat itself
// (background_process_stop needs no approval, yet stops a viewer; #2210
// review). The refusal never asks the approver.
func TestUnattendedTurnRunsOnlyUnattendedTools(t *testing.T) {
	const reason = "Not run: nobody started this turn."
	executor := &engineTestExecutor{tools: []Tool{
		{Name: "camera_list", Unattended: true},
		{Name: "robot__move", AppProvided: true},
		{Name: "background_process_stop"},
		{Name: "workspace_write", RequiresApproval: true},
	}}
	results := map[string]string{}
	provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		if messages[len(messages)-1].Role == "tool" {
			for _, m := range messages {
				if m.Role == "tool" {
					results[m.ToolCallID] = m.Content
				}
			}
			return Message{Content: "Done."}, nil
		}
		var calls []ToolCall
		for _, tool := range executor.tools {
			calls = append(calls, ToolCall{ID: tool.Name, Name: tool.Name, Arguments: json.RawMessage(`{}`)})
		}
		return Message{ToolCalls: calls}, nil
	})
	approver := func(context.Context, ToolCall) (bool, error) {
		t.Error("an unattended turn asked the approver")
		return false, nil
	}
	if err := NewEngine(provider, executor, "x").TurnWithOptions(context.Background(), "watch report", nil, approver, TurnOptions{UnattendedRefusal: reason}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || executor.calls[0].Name != "camera_list" {
		t.Fatalf("ran %v; only camera_list may run", executor.calls)
	}
	for _, name := range []string{"robot__move", "background_process_stop", "workspace_write"} {
		if results[name] != reason {
			t.Errorf("%s result = %q; want the refusal", name, results[name])
		}
	}
}

// unattendedChatTools is every built-in chat tool that may run in a turn a
// watch report started. Adding a name is a review decision: the tool must
// change nothing on the device, in Cloud or on this computer.
var unattendedChatTools = []string{"background_process_list", "memory_search", "profile_skill", "wendy_docs", "workspace_list", "workspace_read"}

func TestUnattendedChatToolsAreAnExplicitList(t *testing.T) {
	builtIn := append(append(slices.Clone(localTools), memoryTools...), profileSkillTool, delegateTool, remoteTool)
	var got []string
	for _, tool := range builtIn {
		if tool.Unattended && tool.RequiresApproval {
			t.Errorf("%s is marked unattended but needs approval", tool.Name)
		}
		if tool.Unattended {
			got = append(got, tool.Name)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, unattendedChatTools) {
		t.Fatalf("unattended chat tools = %v; want %v", got, unattendedChatTools)
	}
}

// fixedToolsMCP lists the given tools and records nothing.
type fixedToolsMCP struct{ tools []mcpgo.Tool }

func (m *fixedToolsMCP) ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{Tools: m.tools}, nil
}
func (*fixedToolsMCP) CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultText("ok"), nil
}
func (*fixedToolsMCP) Close() error { return nil }

// A Wendy MCP tool may run unattended only when the server marks it read-only
// and no device app provides it: an app sets its own hint.
func TestWendyMCPToolsAreUnattendedOnlyWhenBuiltInAndReadOnly(t *testing.T) {
	executor, err := newWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	executor.mcp = &fixedToolsMCP{tools: []mcpgo.Tool{
		mcpgo.NewTool("device_info", mcpgo.WithReadOnlyHintAnnotation(true)),
		mcpgo.NewTool("container_stop", mcpgo.WithReadOnlyHintAnnotation(false)),
		mcpgo.NewTool("robot"+appToolSeparator+"status", mcpgo.WithReadOnlyHintAnnotation(true)),
	}}
	tools, err := executor.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"device_info": true, "container_stop": false, "robot" + appToolSeparator + "status": false}
	for _, tool := range tools {
		if expected, ok := want[tool.Name]; ok && tool.Unattended != expected {
			t.Errorf("%s Unattended = %v; want %v", tool.Name, tool.Unattended, expected)
		}
	}
}
