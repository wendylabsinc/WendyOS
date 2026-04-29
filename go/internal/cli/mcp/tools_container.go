package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func (s *mcpServer) registerContainerTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("container_list",
		mcpgo.WithDescription("List all containers on the connected device"),
	), s.handleContainerList)

	srv.AddTool(mcpgo.NewTool("container_start",
		mcpgo.WithDescription("Start a container and stream its output (bounded snapshot)"),
		mcpgo.WithString("app_name",
			mcpgo.Required(),
			mcpgo.Description("App name of the container to start"),
		),
	), s.handleContainerStart)

	srv.AddTool(mcpgo.NewTool("container_stop",
		mcpgo.WithDescription("Stop a running container"),
		mcpgo.WithString("app_name",
			mcpgo.Required(),
			mcpgo.Description("App name of the container to stop"),
		),
	), s.handleContainerStop)

	srv.AddTool(mcpgo.NewTool("container_delete",
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
	), s.handleContainerDelete)

	srv.AddTool(mcpgo.NewTool("container_stats",
		mcpgo.WithDescription("Get memory and storage stats for all containers"),
	), s.handleContainerStats)

	srv.AddTool(mcpgo.NewTool("container_attach",
		mcpgo.WithDescription("Attach to a running container and collect a bounded snapshot of its output"),
		mcpgo.WithString("app_name",
			mcpgo.Required(),
			mcpgo.Description("App name of the container to attach to"),
		),
		mcpgo.WithNumber("max_lines",
			mcpgo.Description("Maximum output chunks to collect (default 100)"),
		),
	), s.handleContainerAttach)
}

func (s *mcpServer) handleContainerList(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	var containers []map[string]any
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return mcpgo.NewToolResultError(grpcErrString(err)), nil
		}
		c := resp.GetContainer()
		if c == nil {
			continue
		}
		containers = append(containers, map[string]any{
			"app_name":      c.GetAppName(),
			"app_version":   c.GetAppVersion(),
			"running_state": c.GetRunningState().String(),
			"failure_count": c.GetFailureCount(),
		})
	}
	if containers == nil {
		containers = []map[string]any{}
	}
	b, _ := json.MarshalIndent(containers, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleContainerStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return mcpgo.NewToolResultError("app_name is required"), nil
	}
	stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{AppName: appName})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	var sb strings.Builder
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return mcpgo.NewToolResultError(grpcErrString(err)), nil
		}
		switch resp.ResponseType.(type) {
		case *agentpb.RunContainerLayersResponse_StdoutOutput:
			sb.Write(resp.GetStdoutOutput().GetData())
		case *agentpb.RunContainerLayersResponse_StderrOutput:
			sb.Write(resp.GetStderrOutput().GetData())
		}
	}
	out := sb.String()
	if out == "" {
		out = fmt.Sprintf("container %s started", appName)
	}
	return mcpgo.NewToolResultText(out), nil
}

func (s *mcpServer) handleContainerStop(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return mcpgo.NewToolResultError("app_name is required"), nil
	}
	_, err := conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: appName})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	return mcpgo.NewToolResultText(fmt.Sprintf("container %s stopped", appName)), nil
}

func (s *mcpServer) handleContainerDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return mcpgo.NewToolResultError("app_name is required"), nil
	}
	_, err := conn.ContainerService.DeleteContainer(ctx, &agentpb.DeleteContainerRequest{
		AppName:       appName,
		DeleteImage:   req.GetBool("delete_image", false),
		DeleteVolumes: req.GetBool("delete_volumes", false),
	})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	return mcpgo.NewToolResultText(fmt.Sprintf("container %s deleted", appName)), nil
}

func (s *mcpServer) handleContainerStats(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.ContainerService.ListContainerStats(ctx, &agentpb.ListContainerStatsRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	var stats []map[string]any
	for _, cs := range resp.GetStats() {
		stats = append(stats, map[string]any{
			"app_name":      cs.GetAppName(),
			"memory_bytes":  cs.GetMemoryBytes(),
			"storage_bytes": cs.GetStorageBytes(),
		})
	}
	if stats == nil {
		stats = []map[string]any{}
	}
	b, _ := json.MarshalIndent(stats, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleContainerAttach(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	appName := stringParam(req, "app_name")
	if appName == "" {
		return mcpgo.NewToolResultError("app_name is required"), nil
	}
	maxChunks := intParam(req, "max_lines", 100)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stream, err := conn.ContainerService.AttachContainer(ctx)
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	if err := stream.Send(&agentpb.AttachContainerRequest{
		RequestType: &agentpb.AttachContainerRequest_AppName{AppName: appName},
	}); err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
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
			return mcpgo.NewToolResultError(grpcErrString(err)), nil
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
	return mcpgo.NewToolResultText(sb.String()), nil
}
