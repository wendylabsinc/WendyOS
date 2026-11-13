package mcp

import (
	"context"
	"fmt"
	"io"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *mcpServer) registerOSTools(srv *server.MCPServer) {
	updateOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Trigger an OS update on the connected device and stream progress"),
		mcpgo.WithString("artifact_url",
			mcpgo.Description("URL of the OS update artifact (leave empty to use the device's configured update channel)"),
		),
	}
	updateOpts = append(updateOpts, destructive()...)
	updateOpts = append(updateOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("os_update", updateOpts...), s.handleOSUpdate)

	statusOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Report the outcome of the device's most recent OS update: committed after post-reboot healthchecks, or rolled back with the services that failed"),
	}
	statusOpts = append(statusOpts, readOnly()...)
	statusOpts = append(statusOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("os_update_status", statusOpts...), s.handleOSUpdateStatus)
}

func (s *mcpServer) handleOSUpdateStatus(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.GetOSUpdateStatus(ctx, &agentpb.GetOSUpdateStatusRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	if !resp.GetHasResult() {
		return okResult(map[string]any{"has_result": false}), nil
	}

	outcome := strings.ToLower(strings.TrimPrefix(resp.GetOutcome().String(), "OUTCOME_"))
	result := map[string]any{
		"has_result": true,
		"outcome":    outcome,
	}
	if v := resp.GetOldOsVersion(); v != "" {
		result["old_os_version"] = v
	}
	if v := resp.GetNewOsVersion(); v != "" {
		result["new_os_version"] = v
	}
	var services []map[string]any
	for _, svc := range resp.GetServices() {
		status := strings.ToLower(strings.TrimPrefix(svc.GetStatus().String(), "STATUS_"))
		entry := map[string]any{
			"unit":   svc.GetUnit(),
			"status": status,
		}
		if reason := svc.GetReason(); reason != "" {
			entry["reason"] = reason
		}
		services = append(services, entry)
	}
	if services != nil {
		result["services"] = services
	}
	if re := resp.GetRollbackError(); re != "" {
		result["rollback_error"] = re
	}
	return okResult(result), nil
}

func (s *mcpServer) handleOSUpdate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	// Cancelling the stream once Completed arrives tells the agent it can reboot.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := conn.AgentService.UpdateOS(ctx, &agentpb.UpdateOSRequest{
		ArtifactUrl:    stringParam(req, "artifact_url"),
		UpdaterBackend: "",
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}

	tok := progressToken(req)
	var sb strings.Builder
	installed := false
	for done := false; !done; {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Agents that reboot straight after the last frames can lose them.
			if installed && status.Code(err) == codes.Unavailable {
				sb.WriteString("connection lost after the install finished — the device is rebooting; check os_update_status once it is back\n")
				break
			}
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		switch resp.ResponseType.(type) {
		case *agentpb.UpdateOSResponse_Progress_:
			p := resp.GetProgress()
			sb.WriteString(fmt.Sprintf("[%s] %d%%\n", p.GetPhase(), p.GetPercent()))
			reportProgress(ctx, tok, float64(p.GetPercent()), 100, fmt.Sprintf("[%s] %d%%", p.GetPhase(), p.GetPercent()))
			if p.GetPhase() == "done" || p.GetPhase() == "finalizing" {
				installed = true
			}
		case *agentpb.UpdateOSResponse_Completed_:
			c := resp.GetCompleted()
			if c.GetRebootRequired() {
				sb.WriteString("update complete — reboot required\n")
			} else {
				sb.WriteString("update complete\n")
			}
			// The agent reboots instead of ending the stream.
			done = true
		case *agentpb.UpdateOSResponse_Failed_:
			return errResultf(errCodeInternal, "update failed: %s", resp.GetFailed().GetErrorMessage()), nil
		}
	}
	out := sb.String()
	if out == "" {
		out = "OS update initiated"
	}
	return okText(out), nil
}
