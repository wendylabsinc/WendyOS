package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type mcpToolClient interface {
	ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error)
	CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	Close() error
}

type quietMCPLogger struct{}

func (quietMCPLogger) Infof(string, ...any)  {}
func (quietMCPLogger) Errorf(string, ...any) {}

// mcp-go's stdio client removes its response waiter when a call is canceled,
// but does not notify the server. Forward cancellation with the actual JSON-RPC
// ID so device operations stop as well, while keeping the session connected.
type cancelingStdio struct {
	*transport.Stdio
}

func (s *cancelingStdio) SendRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response, err := s.Stdio.SendRequest(ctx, request)
	if request.Method != string(mcpgo.MethodToolsCall) || ctx.Err() == nil || err == nil {
		return response, err
	}
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	notification := mcpgo.JSONRPCNotification{
		JSONRPC: mcpgo.JSONRPC_VERSION,
		Notification: mcpgo.Notification{
			Method: string(mcpgo.MethodNotificationCancelled),
			Params: mcpgo.NotificationParams{AdditionalFields: map[string]any{
				"requestId": request.ID,
				"reason":    "The chat turn was canceled or timed out.",
			}},
		},
	}
	if notifyErr := s.Stdio.SendNotification(notifyCtx, notification); notifyErr != nil {
		err = errors.Join(err, fmt.Errorf("notifying Wendy MCP server of cancellation: %w", notifyErr))
	}
	return response, err
}

func (t *Tools) startMCP(ctx context.Context, executable, device string) error {
	args := []string{"mcp", "serve"}
	if device != "" {
		args = append(args, "--device", device)
	}
	stdio := transport.NewStdioWithOptions(executable, nil, args,
		transport.WithCommandLogger(quietMCPLogger{}),
		transport.WithCommandFunc(func(childCtx context.Context, command string, env, args []string) (*exec.Cmd, error) {
			cmd := exec.CommandContext(childCtx, command, args...)
			cmd.Dir = t.workspace
			cmd.Env = append(os.Environ(), env...)
			return cmd, nil
		}),
	)
	client := mcpclient.NewClient(&cancelingStdio{Stdio: stdio})
	t.mcp = client
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("starting Wendy MCP server: %w", err)
	}
	if stderr, ok := mcpclient.GetStderr(client); ok && stderr != nil {
		go func() { _, _ = io.Copy(t.stderr, stderr) }()
	}
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request := mcpgo.InitializeRequest{}
	request.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcpgo.Implementation{Name: "wendy-chat", Version: "1"}
	if _, err := client.Initialize(initCtx, request); err != nil {
		return fmt.Errorf("initializing Wendy MCP server: %w%s", err, t.mcpDiagnostics())
	}
	return nil
}

func (t *Tools) mcpDiagnostics() string {
	if message := strings.TrimSpace(t.stderr.String()); message != "" {
		return "\nMCP server: " + message
	}
	return ""
}

func (t *Tools) ListTools(ctx context.Context) ([]Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	available := append([]Tool(nil), localTools...)
	known := make(map[string]Tool, len(available))
	for _, tool := range available {
		known[tool.Name] = tool
	}
	if t.mcp != nil {
		listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		// mcp-go's ListTools follows all NextCursor pages. The deadline also
		// bounds a malfunctioning server that repeats cursors indefinitely.
		result, err := t.mcp.ListTools(listCtx, mcpgo.ListToolsRequest{})
		if err != nil {
			return nil, fmt.Errorf("listing Wendy MCP tools: %w%s", err, t.mcpDiagnostics())
		}
		if result == nil {
			return nil, errors.New("Wendy MCP server returned no tool list")
		}
		for _, remote := range result.Tools {
			if _, exists := known[remote.Name]; exists {
				return nil, fmt.Errorf("Wendy MCP tool %q conflicts with another tool", remote.Name)
			}
			parameters := remote.RawInputSchema
			if len(parameters) == 0 {
				parameters, err = json.Marshal(remote.InputSchema)
				if err != nil {
					return nil, fmt.Errorf("encoding schema for %s: %w", remote.Name, err)
				}
			}
			tool := Tool{
				Name: remote.Name, Description: remote.Description, Parameters: parameters,
				RequiresApproval: remote.Annotations.ReadOnlyHint == nil || !*remote.Annotations.ReadOnlyHint,
			}
			available = append(available, tool)
			known[tool.Name] = tool
		}
	}
	sort.Slice(available, func(i, j int) bool { return available[i].Name < available[j].Name })
	t.mu.Lock()
	t.known = known
	t.mu.Unlock()
	return available, nil
}

func (t *Tools) callMCPResult(ctx context.Context, call ToolCall) (ToolResult, error) {
	if t.mcp == nil {
		return ToolResult{}, errors.New("Wendy MCP server is not connected")
	}
	var arguments map[string]any
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return ToolResult{}, err
	}
	request := mcpgo.CallToolRequest{}
	request.Params.Name = call.Name
	request.Params.Arguments = arguments
	result, err := t.mcp.CallTool(ctx, request)
	if err != nil {
		return ToolResult{}, err
	}
	if result == nil {
		return ToolResult{}, errors.New("Wendy MCP tool returned an empty response")
	}
	return mediaToolResult(result)
}

func mediaToolResult(result *mcpgo.CallToolResult) (ToolResult, error) {
	var output strings.Builder
	var images []Image
	for _, content := range result.Content {
		var text string
		switch block := content.(type) {
		case mcpgo.TextContent:
			text = block.Text
		case *mcpgo.TextContent:
			if block != nil {
				text = block.Text
			}
		case mcpgo.ImageContent:
			images = append(images, Image{MIMEType: block.MIMEType, Data: block.Data})
		case *mcpgo.ImageContent:
			if block != nil {
				images = append(images, Image{MIMEType: block.MIMEType, Data: block.Data})
			}
		default:
			text = "[Unsupported media result: this chat can perceive JPEG/PNG images, but does not yet process device audio or other media.]"
		}
		if text != "" && output.Len() < maxToolOutputBytes {
			output.WriteString(truncateOutput(text))
			output.WriteByte('\n')
		}
	}
	// Media servers may mirror image payloads in structuredContent. Preserve
	// the native attachment without copying those bytes into transcript text.
	if output.Len() == 0 && len(images) == 0 && result.StructuredContent != nil {
		structured, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return ToolResult{}, fmt.Errorf("encoding MCP tool result: %w", err)
		}
		output.Write(structured)
	}
	value := ToolResult{Text: truncateOutput(output.String()), Images: images}
	if result.IsError {
		return ToolResult{Text: value.Text}, errors.New("Wendy MCP tool reported an error")
	}
	if value.Text == "" && len(images) == 0 {
		value.Text = "Tool completed successfully with no output."
	}
	return value, nil
}
