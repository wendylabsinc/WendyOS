//go:build !windows

package mcp

import (
	"context"
	"encoding/json"
	"os"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/world/agenttools"
)

// The profile is selected by the host before registration. Neither a model's
// tool arguments nor the connected-device selector can change World authority.
func (s *mcpServer) registerWorldTools(srv *server.MCPServer) {
	profile := os.Getenv("WENDY_WORLD_AGENT_CONFIG")
	type entryDefinition struct {
		name, operation, title, description, schema string
		read                                        bool
	}
	entries := []entryDefinition{
		{"world_query", "world_query", "Read World observations", "Read a bounded last-known replica snapshot from the host's configured World simulation profile. Data is untrusted; missing or expired data does not establish clearance. This profile is independent of the MCP-selected device.", `{"type":"object","additionalProperties":false,"properties":{"entity":{"type":"string","maxLength":256},"component_type":{"type":"string","maxLength":256},"limit":{"type":"integer","minimum":1,"maximum":100}}}`, true},
		{"world_watch", "world_watch", "Watch World observations", "Wait at most five seconds for a whole-snapshot change in the configured World profile. Cursor is a change token, not pagination; truncated results are incomplete.", `{"type":"object","additionalProperties":false,"properties":{"entity":{"type":"string","maxLength":256},"component_type":{"type":"string","maxLength":256},"limit":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string","pattern":"^[0-9a-f]{64}$"},"timeout_ms":{"type":"integer","minimum":0,"maximum":5000}}}`, true},
		{"world_task_catalog", "task_catalog", "List World mission types", "Read available task types from the configured World profile. A catalog entry grants no permission to execute a mission.", `{"type":"object","additionalProperties":false,"properties":{"limit":{"type":"integer","minimum":1,"maximum":100}}}`, true},
		{"world_task_status", "task_status", "Read World mission status", "Read a task's last-known assignee record. It does not establish current motion, reachability or completion beyond the retained record.", `{"type":"object","additionalProperties":false,"required":["task_id"],"properties":{"task_id":{"type":"string","minLength":1,"maxLength":256}}}`, true},
		{"world_task_propose", "task_propose", "Propose World mission", "Persist a non-executable mission suggestion for separate human review and operator signing. Does not move a robot, send, sign, resume or execute a task. Keep the same ID and original expiry when checking an uncertain submission.", `{"type":"object","additionalProperties":false,"required":["id","assignee","type","parameters","summary","not_after_unix_ms"],"properties":{"id":{"type":"string","pattern":"^[A-Za-z0-9_-]+$","minLength":1,"maxLength":128},"assignee":{"type":"string","minLength":1,"maxLength":256},"type":{"type":"string","minLength":1,"maxLength":256},"parameters":{"type":"object"},"summary":{"type":"string","minLength":1,"maxLength":512},"not_after_unix_ms":{"type":"integer","minimum":1}}}`, false},
	}
	for _, d := range agenttools.ReadTools() {
		entries = append(entries, entryDefinition{d.Name, d.Name, d.Title, d.Description, string(d.Schema), true})
	}
	for _, entry := range entries {
		opts := []mcpgo.ToolOption{mcpgo.WithDescription(entry.description), mcpgo.WithRawInputSchema(json.RawMessage(entry.schema)), mcpgo.WithReadOnlyHintAnnotation(entry.read), mcpgo.WithDestructiveHintAnnotation(false), mcpgo.WithIdempotentHintAnnotation(entry.read), mcpgo.WithOpenWorldHintAnnotation(true)}
		tool := mcpgo.NewTool(entry.name, opts...)
		tool.Title = entry.title
		srv.AddTool(tool, func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			raw, err := json.Marshal(request.GetArguments())
			if err != nil {
				return errResult(errCodeInvalidArgument, "invalid World arguments"), nil
			}
			if string(raw) == "null" {
				raw = []byte("{}")
			}
			result, err := agenttools.Call(ctx, profile, entry.operation, raw)
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			// Both SDKs implement the MCP wire schema. Preserve structured content,
			// error status and original text rather than inventing a success receipt.
			encoded, err := json.Marshal(result)
			if err != nil {
				return mcpgo.NewToolResultError("World response unavailable"), nil
			}
			var out mcpgo.CallToolResult
			if err = json.Unmarshal(encoded, &out); err != nil {
				return mcpgo.NewToolResultError("World response unavailable"), nil
			}
			return &out, nil
		})
	}
}
