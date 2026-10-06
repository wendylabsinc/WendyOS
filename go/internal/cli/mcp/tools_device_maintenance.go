package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerDeviceMaintenanceTools(srv *server.MCPServer) {
	logs := []mcpgo.ToolOption{
		mcpgo.WithDescription("Read a bounded tail of the connected device's kernel ring buffer (dmesg). Timestamps are microseconds since boot. Does not follow or restart anything."),
		mcpgo.WithInteger("max_records", mcpgo.Min(1), mcpgo.Max(1000), mcpgo.DefaultNumber(100)),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1024), mcpgo.Max(1000000), mcpgo.DefaultNumber(16384)),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(30), mcpgo.DefaultNumber(10)),
	}
	logs = append(logs, readOnly()...)
	logs = append(logs, localOnly()...)
	srv.AddTool(mcpgo.NewTool("device_os_logs", logs...), s.handleDeviceOSLogs)
	update := []mcpgo.ToolOption{
		mcpgo.WithDescription("Update only the connected device's agent, then verify its running version or hash and reconnect. May interrupt the agent connection. OS updates are separate."),
		mcpgo.WithBoolean("nightly", mcpgo.DefaultBool(false)),
		mcpgo.WithString("binary_path", mcpgo.Description("Optional local agent binary or macOS agent bundle; otherwise download the release")),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(3600), mcpgo.DefaultNumber(300)),
	}
	update = append(update, destructive()...)
	update = append(update, openWorld()...)
	srv.AddTool(mcpgo.NewTool("device_update_agent", update...), s.handleDeviceUpdateAgent)
}

func (s *mcpServer) handleDeviceOSLogs(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errResult(errCodeNotConnected, "connect to a device first"), nil
	}
	maxRecords, err := ros2Int(req, "max_records", 100, 1, 1000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	maxBytes, err := ros2Int(req, "max_bytes", 16384, 1024, 1000000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	seconds, err := ros2Int(req, "timeout_seconds", 10, 1, 30)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	follow := false
	stream, err := conn.AgentService.DumpKernelLog(readCtx, &agentpb.DumpKernelLogRequest{Follow: &follow})
	if err != nil {
		return errResult(codeFromGRPC(err), err.Error()), nil
	}
	type record struct {
		Timestamp int64  `json:"timestamp_us"`
		Level     int32  `json:"level"`
		Message   string `json:"message"`
	}
	rows := make([]record, 0, maxRecords)
	sizes := make([]int, 0, maxRecords)
	seen, used := 0, 0
	metadata := map[string]any{"source": "kernel", "timestamp_origin": "boot", "complete": false}
	for batches := 0; ; batches++ {
		if batches == 10000 {
			metadata["collection_limited"] = true
			break
		}
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			metadata["complete"] = true
			break
		}
		if recvErr != nil {
			if seen == 0 {
				return errResult(codeFromGRPC(recvErr), recvErr.Error()), nil
			}
			metadata["collection_error"] = recvErr.Error()
			metadata["collection_limited"] = true
			break
		}
		for _, r := range resp.GetRecords() {
			if r == nil {
				continue
			}
			seen++
			row := record{Timestamp: r.GetTimestampUs(), Level: r.GetLevel(), Message: r.GetMessage()}
			encoded, _ := json.Marshal(row)
			if len(encoded) > maxBytes {
				continue
			}
			// Keep the latest complete records within a bounded working set.
			for len(rows) > 0 && (len(rows) >= maxRecords || used+len(encoded) > maxBytes) {
				used -= sizes[0]
				rows, sizes = rows[1:], sizes[1:]
			}
			if len(encoded) <= maxBytes {
				rows, sizes = append(rows, row), append(sizes, len(encoded))
				used += len(encoded)
			}
		}
	}
	metadata["collected"] = seen
	metadata["tail_omitted"] = seen - len(rows)
	if seen > len(rows) {
		metadata["tail_truncated"] = true
	}
	return okRowsBounded("logs", rows, metadata, maxBytes, maxRecords), nil
}

func (s *mcpServer) handleDeviceUpdateAgent(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return errResult(errCodeUnsupported, "agent updates require a host MCP session"), nil
	}
	s.mu.RLock()
	conn, connType, target, revision := s.conn, s.connType, s.commandTarget, s.connRevision
	s.mu.RUnlock()
	if conn == nil {
		return errResult(errCodeNotConnected, "connect to the device to update first"), nil
	}
	selector := target.Selector
	if selector == "" {
		selector = target.Device
	}
	if selector == "" || (target.Transport == "cloud" && target.Selector == "") {
		return errResult(errCodeUnsupported, "reconnect with an explicit replayable device selector before updating"), nil
	}
	seconds, err := ros2Int(req, "timeout_seconds", 300, 1, 3600)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	args := []string{"--json", "--device", selector, "device", "update"}
	if value, present := req.GetArguments()["nightly"]; present {
		nightly, valid := value.(bool)
		if !valid {
			return errResult(errCodeInvalidArgument, "nightly must be a boolean"), nil
		}
		if nightly {
			args = append(args, "--nightly")
		}
	}
	if path := stringParam(req, "binary_path"); path != "" {
		path, err = filepath.Abs(path)
		if err != nil {
			return errResult(errCodeInvalidArgument, err.Error()), nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return errResult(errCodeInvalidArgument, "binary_path must be a regular local file"), nil
		}
		args = append(args, "--binary", path)
	}
	if !s.agentUpdateMu.TryLock() {
		return errResult(errCodeInvalidArgument, "an agent update is already in progress in this MCP session"), nil
	}
	defer s.agentUpdateMu.Unlock()
	updateCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	runner := s.updateAgentCommandFn
	if runner == nil {
		runner = executeRunCommand
	}
	output, truncated, runErr := runner(updateCtx, args, target, 16384)
	result := map[string]any{"target": target, "os_update": "not_requested", "output_truncated": truncated}
	response, parseErr := parseAgentUpdateResult(output)
	if runErr != nil || updateCtx.Err() != nil || parseErr != nil {
		result["status"], result["output"] = "unconfirmed", strings.TrimSpace(output)
		result["error_code"] = string(errCodeInternal)
		if updateCtx.Err() != nil {
			result["error_code"] = string(errCodeTimeout)
		}
		result["message"] = "Agent update was not verified. Inspect device_info before retrying; the agent may have restarted."
		if output == "" && runErr != nil {
			result["output"] = runErr.Error()
		}
	} else {
		result["status"], result["version"], result["message"] = response.Status, response.Version, response.Message
	}
	// A restart can invalidate the session's transport. Reconnect only if the
	// caller has not selected another device while the update was running.
	s.mu.Lock()
	unchanged := s.connRevision == revision
	if unchanged {
		s.setConnectionLocked(nil, "", commandTarget{})
	}
	reconnectRevision := s.connRevision
	s.mu.Unlock()
	if unchanged {
		result["connection"] = "disconnected"
		if (conn.Reconnect != nil || s.connectFn != nil) && ctx.Err() == nil {
			reconnectCtx, reconnectCancel := context.WithTimeout(ctx, 20*time.Second)
			reconnect := conn.Reconnect
			if reconnect == nil {
				reconnect = func(ctx context.Context) (*grpcclient.AgentConnection, error) { return s.connectFn(ctx, selector) }
			}
			next, reconnectErr := reconnect(reconnectCtx)
			reconnectCancel()
			if reconnectErr == nil && next != nil {
				s.mu.Lock()
				if s.connRevision == reconnectRevision {
					s.setConnectionLocked(next, connType, target)
					result["connection"] = "connected"
				} else {
					_ = next.Close()
					result["connection"] = "changed_during_update"
				}
				s.mu.Unlock()
			} else if reconnectErr != nil {
				result["reconnect_error"] = reconnectErr.Error()
			}
		}
	} else {
		result["connection"] = "changed_during_update"
	}
	if result["connection"] == "disconnected" {
		result["suggested_next_step"] = "Call device_connect with the returned target selector."
	}
	r := okResult(result)
	r.IsError = runErr != nil || updateCtx.Err() != nil || parseErr != nil
	return r, nil
}

type agentUpdateResult struct{ Status, Version, Message string }

func parseAgentUpdateResult(output string) (*agentUpdateResult, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var result agentUpdateResult
		if json.Unmarshal([]byte(lines[i]), &result) == nil && (result.Status == "success" || result.Status == "up-to-date") {
			return &result, nil
		}
	}
	return nil, fmt.Errorf("agent updater returned no verified result")
}
