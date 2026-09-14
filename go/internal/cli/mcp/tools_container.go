package mcp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerContainerTools(srv *server.MCPServer) {
	listOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List all containers on the connected device"),
	}
	listOpts = append(listOpts, readOnly()...)
	listOpts = append(listOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_list", listOpts...), s.handleContainerList)

	startOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Start a container and stream its output (bounded snapshot). The app runs with the entitlements declared in its wendy.json (e.g. gpu, network, persistence); if the device denies a required entitlement, the start fails (or the container exits) with error_code ENTITLEMENT_DENIED, also visible later as termination_reason in container_list."),
		mcpgo.WithString("app_name",
			mcpgo.Required(),
			mcpgo.Description("App name of the container to start"),
		),
		mcpgo.WithNumber("max_chunks",
			mcpgo.Description("Maximum output chunks to collect (default 200)"),
		),
		mcpgo.WithNumber("max_bytes",
			mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)"),
		),
	}
	startOpts = append(startOpts, mutating()...)
	startOpts = append(startOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_start", startOpts...), s.handleContainerStart)

	stopOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Stop a running container"),
		mcpgo.WithString("app_name",
			mcpgo.Required(),
			mcpgo.Description("App name of the container to stop"),
		),
	}
	stopOpts = append(stopOpts, destructive()...)
	stopOpts = append(stopOpts, idempotent()...)
	stopOpts = append(stopOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_stop", stopOpts...), s.handleContainerStop)

	deleteOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Delete a container, optionally removing its image and volumes"),
		mcpgo.WithString("app_name",
			mcpgo.Required(),
			mcpgo.Description("App name of the container to delete"),
		),
		mcpgo.WithBoolean("delete_image",
			mcpgo.Description("Also delete the container image (frees disk space)"),
		),
		mcpgo.WithBoolean("delete_volumes",
			mcpgo.Description("Also delete persistent volumes"),
		),
	}
	deleteOpts = append(deleteOpts, destructive()...)
	deleteOpts = append(deleteOpts, idempotent()...)
	deleteOpts = append(deleteOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_delete", deleteOpts...), s.handleContainerDelete)

	statsOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Get memory and storage stats for all containers"),
	}
	statsOpts = append(statsOpts, readOnly()...)
	statsOpts = append(statsOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_stats", statsOpts...), s.handleContainerStats)

	attachOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Attach to a running container and collect a bounded snapshot of its output"),
		mcpgo.WithString("app_name",
			mcpgo.Required(),
			mcpgo.Description("App name of the container to attach to"),
		),
		mcpgo.WithNumber("max_chunks",
			mcpgo.Description("Maximum output chunks to collect (default 100)"),
		),
		mcpgo.WithNumber("max_lines",
			mcpgo.Description("Deprecated alias for max_chunks (maximum output chunks to collect, default 100)"),
		),
		mcpgo.WithNumber("max_bytes",
			mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)"),
		),
	}
	attachOpts = append(attachOpts, readOnly()...)
	attachOpts = append(attachOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_attach", attachOpts...), s.handleContainerAttach)
}

func (s *mcpServer) handleContainerList(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	var containers []map[string]any
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		c := resp.GetContainer()
		if c == nil {
			continue
		}
		entry := map[string]any{
			"app_name":      c.GetAppName(),
			"app_version":   c.GetAppVersion(),
			"running_state": c.GetRunningState().String(),
			"failure_count": c.GetFailureCount(),
		}
		// Interface ports: "which apps on this device expose something I can
		// talk to" is the first question a fleet-managing agent asks, and the
		// answer was being dropped on the floor -- the proto carries both
		// ports, this handler just never copied them. Omitted when 0 so their
		// presence means "this app declares that entitlement".
		if port := c.GetMcpPort(); port != 0 {
			entry["mcp_port"] = port
		}
		if port := c.GetHttpPort(); port != 0 {
			entry["http_port"] = port
		}
		if svcs := c.GetServices(); len(svcs) > 0 {
			services := make([]map[string]any, 0, len(svcs))
			for _, svc := range svcs {
				services = append(services, map[string]any{
					"name":          svc.GetName(),
					"running_state": svc.GetRunningState().String(),
				})
			}
			entry["services"] = services
		}
		// Exit diagnostics: why a stopped app stopped (crashed / OOM / start
		// failure / entitlement denied). Only present when recorded.
		if reason := c.GetTerminationReason(); reason != "" {
			entry["termination_reason"] = reason
			entry["exit_code"] = c.GetExitCode()
		}
		containers = append(containers, entry)
	}
	return okList("containers", containers), nil
}

func (s *mcpServer) handleContainerStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return errResult(errCodeInvalidArgument, "app_name is required"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{AppName: appName})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	maxChunks := intParam(req, "max_chunks", 200)
	var sb strings.Builder
	chunks := 0
	for chunks < maxChunks {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		switch resp.ResponseType.(type) {
		case *agentpb.RunContainerLayersResponse_StdoutOutput:
			sb.Write(resp.GetStdoutOutput().GetData())
			chunks++
		case *agentpb.RunContainerLayersResponse_StderrOutput:
			sb.Write(resp.GetStderrOutput().GetData())
			chunks++
		}
	}
	out := sb.String()
	if out == "" {
		out = fmt.Sprintf("container %s started", appName)
	}
	return okTextBounded(out, "", intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleContainerStop(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return errResult(errCodeInvalidArgument, "app_name is required"), nil
	}
	_, err := conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: appName})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	return okText(fmt.Sprintf("container %s stopped", appName)), nil
}

func (s *mcpServer) handleContainerDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return errResult(errCodeInvalidArgument, "app_name is required"), nil
	}
	_, err := conn.ContainerService.DeleteContainer(ctx, &agentpb.DeleteContainerRequest{
		AppName:       appName,
		DeleteImage:   req.GetBool("delete_image", false),
		DeleteVolumes: req.GetBool("delete_volumes", false),
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	return okText(fmt.Sprintf("container %s deleted", appName)), nil
}

func (s *mcpServer) handleContainerStats(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.ContainerService.ListContainerStats(ctx, &agentpb.ListContainerStatsRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	var stats []map[string]any
	for _, cs := range resp.GetStats() {
		stats = append(stats, map[string]any{
			"app_name":      cs.GetAppName(),
			"memory_bytes":  cs.GetMemoryBytes(),
			"storage_bytes": cs.GetStorageBytes(),
		})
	}
	return okList("stats", stats), nil
}

func (s *mcpServer) handleContainerAttach(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return errResult(errCodeInvalidArgument, "app_name is required"), nil
	}
	maxChunks := intParamAlias(req, "max_chunks", "max_lines", 100)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stream, err := conn.ContainerService.AttachContainer(ctx)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	if err := stream.Send(&agentpb.AttachContainerRequest{
		RequestType: &agentpb.AttachContainerRequest_AppName{AppName: appName},
	}); err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	_ = stream.CloseSend()

	var sb strings.Builder
	collected := 0
	for collected < maxChunks {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		switch resp.ResponseType.(type) {
		case *agentpb.RunContainerLayersResponse_StdoutOutput:
			sb.Write(resp.GetStdoutOutput().GetData())
			collected++
		case *agentpb.RunContainerLayersResponse_StderrOutput:
			sb.Write(resp.GetStderrOutput().GetData())
			collected++
		}
	}
	return okTextBounded(sb.String(), "", intParam(req, "max_bytes", 100000)), nil
}
