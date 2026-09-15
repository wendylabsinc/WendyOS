package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
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
		mcpgo.WithDescription("Start or restart a container and collect a bounded snapshot of its output. This interrupts an existing running task. For passive log inspection use telemetry_logs with app_name. To run a command inside an existing container use container_exec."),
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
	attachOpts = append(attachOpts, destructive()...)
	attachOpts = append(attachOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_attach", attachOpts...), s.handleContainerAttach)

	execOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Run an explicit command inside a running container on the connected device, including a cloud-connected device. Uses the current connection; no separate CLI connection is needed. The command array is passed directly without shell expansion, with no TTY and closed stdin. To run a script, explicitly pass its interpreter and arguments. Returns bounded stdout/stderr and the exit code; nonzero exits and timeouts are errors. This can change files or device state and requires approval."),
		mcpgo.WithString("app_name", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(256), mcpgo.Description("App/container name from container_list")),
		mcpgo.WithArray("command", mcpgo.Required(), mcpgo.MinItems(1), mcpgo.MaxItems(128), mcpgo.WithStringItems(mcpgo.MaxLength(16384)), mcpgo.Description("Executable followed by its arguments, e.g. [\"python3\", \"-c\", \"print('hello')\"]. At most 65536 total argument bytes.")),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(300), mcpgo.Description("Maximum time to wait for completion, default 30 seconds")),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1), mcpgo.Max(1000000), mcpgo.Description("Maximum combined stdout/stderr bytes retained, default 100000; excess output is drained until exit or timeout")),
	}
	execOpts = append(execOpts, destructive()...)
	execOpts = append(execOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("container_exec", execOpts...), s.handleContainerExec)
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
	defer s.refreshContainerMCPTools()
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
	s.refreshContainerMCPTools()
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
	s.refreshContainerMCPTools()
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

func (s *mcpServer) handleContainerExec(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	var args struct {
		AppName        string    `json:"app_name"`
		Command        []*string `json:"command"`
		TimeoutSeconds *int      `json:"timeout_seconds"`
		MaxBytes       *int      `json:"max_bytes"`
	}
	raw, err := json.Marshal(req.GetArguments())
	if err != nil || json.Unmarshal(raw, &args) != nil {
		return errResult(errCodeInvalidArgument, "app_name must be a string, command a string array, and limits integers"), nil
	}
	if strings.TrimSpace(args.AppName) == "" || len(args.AppName) > 256 || strings.ContainsRune(args.AppName, 0) {
		return errResult(errCodeInvalidArgument, "app_name must contain 1..256 bytes and no NUL"), nil
	}
	if len(args.Command) == 0 || len(args.Command) > 128 || args.Command[0] == nil || *args.Command[0] == "" {
		return errResult(errCodeInvalidArgument, "command must contain an executable and at most 128 arguments"), nil
	}
	argumentBytes := 0
	command := make([]string, len(args.Command))
	for i, argument := range args.Command {
		if argument == nil {
			return errResult(errCodeInvalidArgument, "command arguments must be strings"), nil
		}
		command[i] = *argument
		argumentBytes += len(*argument)
		if len(*argument) > 16384 || strings.ContainsRune(*argument, 0) || argumentBytes > 65536 {
			return errResult(errCodeInvalidArgument, "command arguments must have no NUL, at most 16384 bytes each and 65536 bytes total"), nil
		}
	}
	timeoutSeconds, maxBytes := 30, 100000
	if args.TimeoutSeconds != nil {
		timeoutSeconds = *args.TimeoutSeconds
	}
	if args.MaxBytes != nil {
		maxBytes = *args.MaxBytes
	}
	if timeoutSeconds < 1 || timeoutSeconds > 300 || maxBytes < 1 || maxBytes > 1000000 {
		return errResult(errCodeInvalidArgument, "timeout_seconds must be 1..300 and max_bytes must be 1..1000000"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	var stdout, stderr strings.Builder
	remaining, truncated := maxBytes, false
	appendOutput := func(destination *strings.Builder, data []byte) {
		retained := min(remaining, len(data))
		destination.Write(data[:retained])
		remaining -= retained
		truncated = truncated || retained < len(data)
	}
	result := func(exitCode *int32, failure error) *mcpgo.CallToolResult {
		out := map[string]any{
			"stdout": strings.ToValidUTF8(stdout.String(), ""), "stderr": strings.ToValidUTF8(stderr.String(), ""),
			"truncated": truncated,
		}
		if exitCode != nil {
			out["exit_code"] = *exitCode
			if *exitCode != 0 {
				out["error_code"] = "COMMAND_FAILED"
				out["message"] = fmt.Sprintf("command exited with code %d", *exitCode)
			}
		}
		if failure != nil {
			out["error_code"], out["message"] = string(codeFromGRPC(failure)), grpcErrString(failure)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				out["error_code"], out["message"] = string(errCodeTimeout), "container exec timed out before completion was observed"
			}
		}
		r := okResult(out)
		r.IsError = failure != nil || (exitCode != nil && *exitCode != 0)
		if r.IsError {
			// Keep the failure visible before potentially large captured output.
			// Hosts may truncate the JSON fallback before they can parse it.
			summary := mcpgo.NewTextContent(fmt.Sprintf("[%s] %s", out["error_code"], out["message"]))
			r.Content = append([]mcpgo.Content{summary}, r.Content...)
		}
		return r
	}
	stream, err := conn.ContainerService.ExecContainer(ctx, grpc.MaxCallRecvMsgSize(2*1024*1024))
	if err != nil {
		return result(nil, err), nil
	}
	if err := stream.Send(&agentpb.ExecContainerRequest{
		RequestType: &agentpb.ExecContainerRequest_Start{Start: &agentpb.ExecContainerRequest_ExecStart{
			AppName: args.AppName, Command: command, Tty: false,
		}},
	}); err != nil {
		return result(nil, err), nil
	}
	if err := stream.CloseSend(); err != nil {
		return result(nil, err), nil
	}
	for {
		response, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				err = errors.New("container exec stream ended without an exit code; completion is unknown")
			}
			return result(nil, err), nil
		}
		switch output := response.ResponseType.(type) {
		case *agentpb.ExecContainerResponse_StdoutData:
			appendOutput(&stdout, output.StdoutData)
		case *agentpb.ExecContainerResponse_StderrData:
			appendOutput(&stderr, output.StderrData)
		case *agentpb.ExecContainerResponse_ExitCode:
			return result(&output.ExitCode, nil), nil
		}
	}
}
